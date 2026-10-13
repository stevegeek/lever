package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/sentledger"
)

// ChatConfig configures message verification: the host records an agent's
// message_verify call is answered from.
type ChatConfig struct {
	// Configured is whether verified web chat is configured (remote access
	// with allowed_users).
	Configured bool
	// LedgerPath is the remote proxy's chat ledger. "" means no web chat
	// post can be verified: off when !Configured, unsafe when the state
	// directory is inside the tree.
	LedgerPath string
	// WebSenders are the lowercased senders the remote proxy's sign-ins post
	// as; a message from one is looked up in the chat ledger only.
	WebSenders []string
	// UsedPath persists the one-use rule across restarts. "" keeps it in
	// memory only.
	UsedPath string
	// SentLedgerDir is the sent ledger. "" means the state directory is
	// inside the tree: sends go out unrecorded and no lever message can be
	// verified.
	SentLedgerDir string
}

const maxChatFromLen = 320

// chatVerifyWindow bounds, with the one-use rule, a replay: text that copies
// an old real envelope cannot turn an old "yes, go ahead" into a new one.
const chatVerifyWindow = time.Hour

// chatRepeatGrace is how long a verified message still verifies for the same
// agent, marked as a repeat. A repeat is not a second authority (the skill
// acts on it only if not acted on yet); it exists so a genuine envelope that
// injected text made the agent check early is not lost.
const chatRepeatGrace = 10 * time.Minute

// useRetention outlasts the longest verify window (sentledger.Window).
const useRetention = 2 * sentledger.Window

const usesRotateAt = 4 << 20

// chatUses records which messages each agent has verified, so each verifies
// once. Web posts are keyed by hub id and lever sends by "lever:" + sent id,
// so the two never collide. It is written through to path (0600) and re-read
// before each use, so neither a restart nor a second broker during a handoff
// re-opens a message inside its window.
//
// Every persistence failure fails closed. A use that cannot be written is
// not granted. If the record cannot be read at start, then until
// sentledger.Window has passed every entry recorded before start is refused
// (notBefore): the broker cannot know which were used.
type chatUses struct {
	mu        sync.Mutex
	path      string
	used      map[string]time.Time // caller + "\x00" + message id → when verified
	notBefore time.Time            // zero unless the record could not be read at start
	degraded  time.Time            // until when notBefore applies
}

type chatUseLine struct {
	Caller string    `json:"caller"`
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
}

func useKey(caller, id string) string { return caller + "\x00" + id }

const usesLabel = "record of verified messages"

// readUses reads path and its rotated copy. A directory or file another user
// can write, a symlink, or a swapped file is hostledger.ErrUnsafe, so no one
// but the broker's user can mark a message used or make the broker start
// degraded.
func readUses(path string, now time.Time) ([]chatUseLine, error) {
	if err := hostledger.CheckDir(filepath.Dir(path), usesLabel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []chatUseLine
	for _, p := range []string{path + ".1", path} {
		err := hostledger.ReadFile(p, usesLabel, func(l []byte) {
			var c chatUseLine
			if json.Unmarshal(l, &c) == nil && c.ID != "" && now.Sub(c.At) <= useRetention {
				out = append(out, c)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// rotateUses rotates only when the current .1 is older than useRetention, so
// a rotation never drops a use that can still matter. It never follows a
// symlink and does nothing in an unsafe directory.
func rotateUses(path string, now time.Time) {
	if hostledger.CheckDir(filepath.Dir(path), usesLabel) != nil {
		return
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= usesRotateAt {
		return
	}
	if old, err := os.Lstat(path + ".1"); err == nil && now.Sub(old.ModTime()) <= useRetention {
		return
	}
	_ = os.Rename(path, path+".1")
}

// newChatUses never rewrites the record: a rewrite could drop a use another
// broker appends during a restart handoff.
func newChatUses(path string, now time.Time) *chatUses {
	u := &chatUses{path: path, used: map[string]time.Time{}}
	if path == "" {
		return u
	}
	keep, err := readUses(path, now)
	if err != nil {
		u.notBefore, u.degraded = now, now.Add(sentledger.Window)
		return u
	}
	for _, c := range keep {
		u.used[useKey(c.Caller, c.ID)] = c.At
	}
	return u
}

var errUseNotRecorded = errors.New("cannot record the verification")

type useWant struct {
	id       string
	recorded time.Time
}

type useGot struct {
	at    time.Time
	fresh bool
}

func (u *chatUses) take(caller, id string, recorded, now time.Time) (time.Time, bool, error) {
	got, err := u.takeAll(caller, []useWant{{id: id, recorded: recorded}}, now)
	if err != nil {
		return time.Time{}, false, err
	}
	return got[0].at, got[0].fresh, nil
}

// takeAll is all or nothing: it checks every id before it writes one append,
// so a failure never leaves a match used without its text reaching the agent.
func (u *chatUses) takeAll(caller string, want []useWant, now time.Time) ([]useGot, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, w := range want {
		if !u.notBefore.IsZero() && now.Before(u.degraded) && w.recorded.Before(u.notBefore) {
			return nil, errors.New("the record of verified messages could not be read at broker start")
		}
	}
	for k, t := range u.used {
		if now.Sub(t) > useRetention {
			delete(u.used, k)
		}
	}
	if u.path != "" {
		// Another broker may have recorded a use since this one started.
		lines, err := readUses(u.path, now)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errUseNotRecorded, err)
		}
		for _, c := range lines {
			if t, ok := u.used[useKey(c.Caller, c.ID)]; !ok || c.At.Before(t) {
				u.used[useKey(c.Caller, c.ID)] = c.At
			}
		}
	}
	got := make([]useGot, len(want))
	var buf []byte
	var fresh []string
	for i, w := range want {
		k := useKey(caller, w.id)
		if t, ok := u.used[k]; ok {
			got[i] = useGot{at: t}
			continue
		}
		if slices.Contains(fresh, k) {
			got[i] = useGot{at: now}
			continue
		}
		fresh = append(fresh, k)
		got[i] = useGot{at: now, fresh: true}
		line, _ := json.Marshal(chatUseLine{Caller: caller, ID: w.id, At: now})
		buf = append(append(buf, line...), '\n')
	}
	if u.path != "" && len(buf) > 0 {
		rotateUses(u.path, now)
		f, err := os.OpenFile(u.path, os.O_CREATE|os.O_RDWR|os.O_APPEND|hostledger.ONoFollow, 0o600)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errUseNotRecorded, err)
		}
		// After a torn last line, start on a new line.
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
			last := make([]byte, 1)
			if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
				buf = append([]byte{'\n'}, buf...)
			}
		}
		_, werr := f.Write(buf)
		cerr := f.Chmod(0o600)
		if err := errors.Join(werr, cerr, f.Close()); err != nil {
			return nil, fmt.Errorf("%w: %v", errUseNotRecorded, err)
		}
	}
	for _, k := range fresh {
		u.used[k] = now
	}
	return got, nil
}

// verifiedBy re-reads the record so another broker's use counts. An
// unreadable record answers false, so a reply fails closed.
func (u *chatUses) verifiedBy(caller, id string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.used[useKey(caller, id)]; ok {
		return true
	}
	if u.path == "" {
		return false
	}
	lines, err := readUses(u.path, now)
	if err != nil {
		return false
	}
	for _, c := range lines {
		if c.Caller == caller && c.ID == id {
			return true
		}
	}
	return false
}
