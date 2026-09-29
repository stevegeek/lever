// Package chatledger is the host-side record of web chat posts that passed
// through the remote proxy (verified web chat).
//
// The remote proxy is the ONLY writer: after the hub accepts a chat post that
// came through the authenticating front from an allowed login, the proxy
// appends one Entry. The broker is the only reader: an agent asks it, over
// its own mTLS channel, whether a message it received is in the ledger
// (`message_verify`), and acts only on the text the broker returns.
//
// Why an agent cannot forge an entry: the file lives in the host state
// directory, outside every jail mount; agents reach the hub directly, never
// through the proxy, so their posts are never recorded; and the proxy records
// only a request it authorized with a verified login. Nothing is ever added to
// the message text itself — text in an agent's pane is never evidence.
package chatledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
)

// The tiers a ledger entry can carry (config.TierOperator/TierContact): what
// the verified message counts as.
const (
	// TierOperator: the operator's own steering.
	TierOperator = "operator"
	// TierContact: an external contact's words for the task.
	TierContact = "contact"
)

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

// fileWriter appends entries to one login's file.
type fileWriter struct {
	path string
	once sync.Once
	f    *hostledger.File
}

// Append writes e as one JSON line, rotating first when the file has grown
// past RotateCap (hostledger.File.Append: 0600, O_NOFOLLOW).
func (w *fileWriter) Append(e Entry) error {
	w.once.Do(func() { w.f = &hostledger.File{Path: w.path, Label: label, Cap: RotateCap} })
	return w.f.Append(e)
}

// label prefixes the ledger's errors.
const label = "chat ledger"

// ErrUnsafe means a ledger file can be written by another user, so its
// entries prove nothing (hostledger.ErrUnsafe).
var ErrUnsafe = hostledger.ErrUnsafe

// lookupFile returns the entries for agentID whose Sender is sender and whose
// CreatedAt is createdAt, from path and its rotated copy. A missing file is
// no entries, not an error. A file with group or other write permission is
// ErrUnsafe: its lines could come from anyone.
func lookupFile(path, agentID, sender, createdAt string) ([]Entry, error) {
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

// maxLine bounds one ledger line (hostledger.MaxLine).
const maxLine = hostledger.MaxLine

func readFile(p string) ([]Entry, error) {
	var out []Entry
	err := hostledger.ReadFile(p, label, func(line []byte) {
		var e Entry
		// A torn line (a crash mid-write) or an oversized one is skipped,
		// not fatal: every other entry is still good.
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

// The ledger is a directory with one file per login (FileFor), so the size
// rotation of one login's file never drops another login's entries: a
// contact who floods the chat can push out only their own.

var ledgerFile = regexp.MustCompile(`^l-[0-9a-f]{24}\.jsonl$`)

// FileFor is the ledger file name for a login.
func FileFor(login string) string {
	h := sha256.Sum256([]byte(login))
	return "l-" + hex.EncodeToString(h[:12]) + ".jsonl"
}

// Writer appends entries to the ledger directory. Safe for concurrent use by
// one process.
type Writer struct {
	dir   string
	mu    sync.Mutex
	files map[string]*fileWriter
}

// NewWriter returns a Writer for the ledger directory dir.
func NewWriter(dir string) *Writer { return &Writer{dir: dir, files: map[string]*fileWriter{}} }

// Append writes e to its login's file, creating the directory (0700) first.
func (w *Writer) Append(e Entry) error {
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return fmt.Errorf("chat ledger: %w", err)
	}
	if err := checkDir(w.dir); err != nil {
		return err
	}
	name := FileFor(e.Login)
	w.mu.Lock()
	fw := w.files[name]
	if fw == nil {
		fw = &fileWriter{path: filepath.Join(w.dir, name)}
		w.files[name] = fw
	}
	w.mu.Unlock()
	return fw.Append(e)
}

// checkDir refuses a ledger directory that is a symlink, not a directory,
// writable by others, or owned by another user.
func checkDir(dir string) error { return hostledger.CheckDir(dir, label) }

// Lookup returns the entries for agentID whose Sender is sender and whose
// CreatedAt is createdAt, from every login's file (and its rotated copy) in
// the ledger directory. A missing directory is no entries.
func Lookup(dir, agentID, sender, createdAt string) ([]Entry, error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("chat ledger: %w", err)
	}
	var out []Entry
	seen := map[string]bool{}
	for _, n := range names {
		if !ledgerFile.MatchString(n.Name()) {
			continue
		}
		got, err := lookupFile(filepath.Join(dir, n.Name()), agentID, sender, createdAt)
		if err != nil {
			return nil, err
		}
		for _, e := range got {
			if e.MessageID != "" && seen[e.MessageID] {
				continue
			}
			seen[e.MessageID] = true
			out = append(out, e)
		}
	}
	return out, nil
}
