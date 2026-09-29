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

// ChatConfig configures message verification (verify.go): the host records
// an agent's message_verify call is answered from. Web chat posts are in the
// remote proxy's chat ledger (package chatledger), lever's own sends in the
// broker's sent ledger (package sentledger).
type ChatConfig struct {
	// Configured is whether verified web chat is configured (remote access
	// with allowed_users). It is the 0.27 "enabled" of the answer.
	Configured bool
	// LedgerPath is the remote proxy's chat ledger. "" means no web chat
	// post can be verified: off when !Configured, unsafe when the state
	// directory is inside the tree.
	LedgerPath string
	// WebSenders are the envelope senders the remote proxy's sign-ins post
	// as ("user:" + hub email, lowercased): a message from one of them is
	// looked up in the chat ledger only. Empty when remote access is off.
	WebSenders []string
	// UsedPath records which messages have been verified, so the one-use
	// rule survives a broker restart. "" keeps it in memory only (tests).
	UsedPath string
	// SentLedgerDir is the sent ledger (package sentledger): the record of
	// every message the broker sends to an agent. "" means the state
	// directory is inside the tree: sends go out unrecorded and no lever
	// message can be verified.
	SentLedgerDir string
}

// maxChatFromLen bounds the "from" an agent may send: a sender reference is
// "user:" plus an email.
const maxChatFromLen = 320

// chatVerifyWindow is how long after the proxy recorded a web chat post it
// can still be verified. With the one-use rule below it bounds a replay: text that
// copies an old real envelope (in an email, a tool result, a worker's
// message) cannot turn an old "yes, go ahead" into a new one. A message the
// agent reads later than this is unverified: the operator sends it again.
const chatVerifyWindow = time.Hour

// chatRepeatGrace is how long after its first verification a message still
// verifies for the same agent, marked as a repeat. A repeat is not a second
// authority: the skill tells the agent to act on it only if it has not acted
// on the message yet. It exists so a genuine envelope still verifies after
// an earlier check (a retry, a timed-out call, or injected text that made the
// agent check early) — without it the operator's message would be lost.
const chatRepeatGrace = 10 * time.Minute

// useRetention is how long a recorded use is kept: past the longest window a
// message can verify in (sentledger.Window, from its send; a use comes after
// the send), with room to spare.
const useRetention = 2 * sentledger.Window

// usesRotateAt is the size past which the record of uses is moved to
// <path>.1 before the next append, but only once that .1 holds nothing that
// can still matter (see rotateUses).
const usesRotateAt = 4 << 20

// chatUses records which recorded messages each agent has verified, so each
// verifies once (like a directive is consumed once): web chat posts by the
// hub's message id, lever sends by "lever:" + their sent-ledger id, so the
// two can never collide. It is written through to path (one JSON line per
// use, 0600), read back at start and again before each use, so neither a
// broker restart nor a second broker running for a moment re-opens a message
// inside its window.
//
// Every persistence failure fails closed. A use that cannot be written is
// not granted. A record that cannot be read at start is left alone, and
// until sentledger.Window has passed the broker refuses every entry recorded
// before it started (notBefore): it cannot know which of those were used.
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

// usesLabel prefixes the errors of the record of uses.
const usesLabel = "record of verified messages"

// readUses returns the uses recorded at path and its rotated copy that can
// still matter (inside useRetention). A missing file is none. The record is
// read like the ledgers (hostledger.CheckDir, hostledger.ReadFile): a
// directory or file another user can write, a symlink, or a file swapped
// while it was opened is hostledger.ErrUnsafe, so no one but the broker's
// user can mark a message used (and so lose it) or make the broker start
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

// rotateUses moves the record of uses to <path>.1 when it has grown past
// usesRotateAt and the current .1 is absent or untouched for longer than
// useRetention, so a rotation never drops a use that can still matter (the
// dropped .1 holds only uses older than that). A second broker appending
// into the renamed file during a handoff is still read, since readUses reads
// .1 too. It never follows a symlink and does nothing in an unsafe
// directory; readUses refuses what it would leave behind anyway.
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

// newChatUses loads the record. It never rewrites it: a rewrite could drop a
// use another broker (a restart handoff) appends at the same moment. Old
// lines are only skipped on read; the file grows by one line per first
// verification and is rotated (rotateUses) only when that is safe.
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

// errUseNotRecorded means a use could not be written, so it is not granted.
var errUseNotRecorded = errors.New("cannot record the verification")

// useWant is one use takeAll is asked for.
type useWant struct {
	id       string
	recorded time.Time // when the record was made (for the degraded start)
}

// useGot is takeAll's answer for one useWant: when the message was first
// verified, and whether this is that first time.
type useGot struct {
	at    time.Time
	fresh bool
}

// take marks id used by caller. It returns when the message was first
// verified and whether this is that first time, or an error when the use
// could not be recorded (then nothing is granted).
func (u *chatUses) take(caller, id string, recorded, now time.Time) (time.Time, bool, error) {
	got, err := u.takeAll(caller, []useWant{{id: id, recorded: recorded}}, now)
	if err != nil {
		return time.Time{}, false, err
	}
	return got[0].at, got[0].fresh, nil
}

// takeAll marks every wanted id used by caller, all or nothing: it checks
// every id (the degraded start, the record read once) before it writes, and
// writes the new uses in one append. So a failure never leaves an earlier
// match used without its text reaching the agent. On an error nothing is
// granted.
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
		// Another broker (a restart handoff) may have recorded a use since
		// this one started: read the record again.
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
			// The same id twice in one call: one use.
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
		// After a torn last line (a crash mid-write), start on a new line so
		// these uses are not read as part of it.
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
