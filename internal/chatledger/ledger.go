// Package chatledger is the host-side record of web chat posts that passed
// through the remote proxy (verified web chat).
//
// The remote proxy is the ONLY writer: after the hub accepts a chat post that
// came through the authenticating front from an allowed login, the proxy
// appends one Entry. The broker is the only reader: an agent asks it, over
// its own mTLS channel, whether a message it received is in the ledger
// (`chat_verify`), and acts only on the text the broker returns.
//
// Why an agent cannot forge an entry: the file lives in the host state
// directory, outside every jail mount; agents reach the hub directly, never
// through the proxy, so their posts are never recorded; and the proxy records
// only a request it authorized with a verified login. Nothing is ever added to
// the message text itself — text in an agent's pane is never evidence.
package chatledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// TierOperator is the tier of every allowed login in this version: the
// login is the operator's own.
const TierOperator = "operator"

// RotateCap is the size at which the ledger is moved to <path>.1 (replacing
// any previous .1) before the next append. Lookup reads both files, so a
// message stays verifiable for at least RotateCap of later chat — far more
// than the broker's one-hour verification window needs — and the text of
// older chat does not pile up on the host.
const RotateCap = 1 << 20

// TimeLayout is the second-resolution UTC form both scion's delivery
// envelope ("timestamp") and the ledger's CreatedAt use.
const TimeLayout = "2006-01-02T15:04:05Z07:00"

// Entry is one recorded chat post.
type Entry struct {
	// Recorded is the proxy's clock when it wrote the entry.
	Recorded time.Time `json:"recorded"`
	// Login is the verified login the front asserted (the identity header).
	Login string `json:"login"`
	// Tier is what that login may speak for (TierOperator).
	Tier string `json:"tier"`
	// Conversation is the hub conversation key the post went to.
	Conversation string `json:"conversation"`
	// AgentID is the hub id of the agent side of that conversation.
	AgentID string `json:"agent_id"`
	// MessageID is the hub's id for the stored message.
	MessageID string `json:"message_id"`
	// Sender is the hub's sender reference ("user:<email>"), the value the
	// agent's envelope shows as "from".
	Sender string `json:"sender"`
	// CreatedAt is the hub's creation time in TimeLayout, the value the
	// agent's envelope shows as "timestamp".
	CreatedAt string `json:"created_at"`
	// Text is the message as the hub stored it.
	Text string `json:"text"`
}

// Writer appends entries. Safe for concurrent use by one process.
type Writer struct {
	path string
	mu   sync.Mutex
}

// NewWriter returns a Writer for path.
func NewWriter(path string) *Writer { return &Writer{path: path} }

// Append writes e as one JSON line, rotating first when the file has grown
// past RotateCap. The file is created 0600, and an existing file is set back
// to 0600, so no other user can add a line.
func (w *Writer) Append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if fi, err := os.Stat(w.path); err == nil && fi.Size() > RotateCap {
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return fmt.Errorf("chat ledger: rotate: %w", err)
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("chat ledger: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chat ledger: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("chat ledger: %w", err)
	}
	return f.Close()
}

// ErrUnsafe means a ledger file can be written by another user, so its
// entries prove nothing.
var ErrUnsafe = errors.New("chat ledger is writable by another user")

// Lookup returns the entries for agentID whose Sender is sender and whose
// CreatedAt is createdAt, from path and its rotated copy. A missing file is
// no entries, not an error. A file with group or other write permission is
// ErrUnsafe: its lines could come from anyone.
func Lookup(path, agentID, sender, createdAt string) ([]Entry, error) {
	var out []Entry
	seen := map[string]bool{}
	for _, p := range []string{path + ".1", path} {
		entries, err := readFile(p)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.AgentID != agentID || e.Sender != sender || e.CreatedAt != createdAt {
				continue
			}
			if e.MessageID != "" {
				if seen[e.MessageID] {
					continue
				}
				seen[e.MessageID] = true
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// maxLine bounds one ledger line: a message is at most 16000 characters
// (scion's messages.MaxMessageLength), so this leaves room for JSON escaping.
const maxLine = 1 << 20

func readFile(p string) ([]Entry, error) {
	// Lstat first: a symlink could point at a file some other process can
	// write, whatever its own mode says.
	li, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chat ledger: %w", err)
	}
	if li.Mode()&fs.ModeSymlink != 0 || !li.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsafe, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("chat ledger: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("chat ledger: %w", err)
	}
	if !os.SameFile(li, fi) {
		return nil, fmt.Errorf("%w: %s changed while it was opened", ErrUnsafe, p)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is %v", ErrUnsafe, p, fi.Mode().Perm())
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return nil, fmt.Errorf("%w: %s belongs to uid %d", ErrUnsafe, p, owner)
	}
	var out []Entry
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := readLine(rd)
		if len(line) > 0 {
			var e Entry
			// A torn line (a crash mid-write) or an oversized one is skipped,
			// not fatal: every other entry is still good.
			if json.Unmarshal(line, &e) == nil {
				out = append(out, e)
			}
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("chat ledger: %w", err)
		}
	}
}

// readLine returns the next line without its newline, or nil when the line
// is longer than maxLine (the rest of it is consumed and dropped).
func readLine(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := rd.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, err
		}
		return bytes.TrimSpace(buf), err
	}
}

// NormalizeTimestamp parses an envelope timestamp (RFC 3339) and returns it
// in TimeLayout, UTC, truncated to the second — the form CreatedAt uses.
func NormalizeTimestamp(s string) (string, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", err
	}
	return FormatTime(t), nil
}

// FormatTime is t in TimeLayout, UTC, truncated to the second: how scion
// renders a message's creation time in the delivery envelope.
func FormatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(TimeLayout)
}
