package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
)

// newConvSession records what the new conversation did with the session
// record: whether it began and committed one.
type newConvSession struct {
	asked     int
	began     int
	committed int
	beginErr  error
}

func newConvFixtureWith(t *testing.T) (*convFixture, *newConvSession) {
	t.Helper()
	f := newConvFixture(t)
	sess := &newConvSession{}
	f.ops.session = func() sessionBegin {
		sess.asked++
		return func() (func() error, error) {
			sess.began++
			if sess.beginErr != nil {
				return nil, sess.beginErr
			}
			return func() error { sess.committed++; return nil }, nil
		}
	}
	f.ops.bootTask = func() (string, error) { return "boot prompt", nil }
	return f, sess
}

func (f *convFixture) calls() string { return strings.Join(f.sc.calls, ",") }

// TestNewConversation is the whole path: the running manager is suspended,
// its transcripts backed up, stopped (its old session ends) and started in a
// fresh session with the boot prompt, through the resume checks, the gate,
// and a session record committed after the start; the guest boot is recorded
// once the manager is live. The old transcript stays in its home.
func TestNewConversation(t *testing.T) {
	f, sess := newConvFixtureWith(t)
	writeConvFile(t, f.transcript("old.jsonl"), "{\"old\":1}\n")
	m, saved, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "new conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if f.sc.task != "boot prompt" {
		t.Fatalf("task = %q, want the prompt as it is now", f.sc.task)
	}
	if !saved || m.Files != 1 || m.Reason != "new conversation" {
		t.Fatalf("backup = %+v saved %v", m, saved)
	}
	if got := f.calls(); got != "suspend,stop,resume-fresh" {
		t.Fatalf("calls = %q", got)
	}
	if f.sc.phase != scion.PhaseRunning {
		t.Fatalf("phase %q", f.sc.phase)
	}
	if got := strings.Join(f.guard.steps, ","); got != "begin,resumed,live,boot" || f.gated != 1 {
		t.Fatalf("checks = %q gated %d", got, f.gated)
	}
	if sess.asked != 1 || sess.began != 1 || sess.committed != 1 {
		t.Fatalf("session = %+v", sess)
	}
	if b, err := os.ReadFile(f.transcript("old.jsonl")); err != nil || string(b) != "{\"old\":1}\n" {
		t.Fatalf("the old transcript left the home: %q %v", b, err)
	}
	store, _ := f.ops.store()
	if all, _ := store.List(); len(all) != 1 || all[0].ID != m.ID {
		t.Fatalf("backups = %+v", all)
	}
}

// TestNewConversationBusy: a manager mid-turn is left alone unless forced;
// the refusal has its own exit code.
func TestNewConversationBusy(t *testing.T) {
	for _, act := range []string{"working", "thinking", "executing"} {
		f, _ := newConvFixtureWith(t)
		f.sc.activity = act
		_, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
		var ee *cli.ExitError
		if !errors.Is(err, errConvBusy) || !errors.As(err, &ee) || ee.Code != exitConversationBusy {
			t.Fatalf("%s: err = %v", act, err)
		}
		if len(f.sc.calls) != 0 {
			t.Fatalf("%s: calls = %v, want none", act, f.sc.calls)
		}
	}
	f, _ := newConvFixtureWith(t)
	f.sc.activity = "working"
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x", Force: true}); err != nil {
		t.Fatalf("forced: %v", err)
	}
	for _, act := range []string{"", "completed", "stalled", "waiting_for_input", "blocked"} {
		if managerBusy(act) {
			t.Errorf("%q counts as busy", act)
		}
	}
}

// TestNewConversationBackupFailsClosed: no backup, no reset. The manager the
// call suspended is resumed on its own conversation, never stopped.
func TestNewConversationBackupFailsClosed(t *testing.T) {
	f, sess := newConvFixtureWith(t)
	outside := filepath.Join(f.home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.agentHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
	if !errors.Is(err, convbackup.ErrNotPlainDir) || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "on its own conversation") {
		t.Fatalf("err = %v", err)
	}
	if got := f.calls(); got != "suspend,resume" {
		t.Fatalf("calls = %q, want the manager resumed and never stopped", got)
	}
	if sess.asked != 0 {
		t.Fatalf("session begun without a backup: %+v", sess)
	}

	// No home at all: the same refusal.
	f, _ = newConvFixtureWith(t)
	f.ops.guest.Agent = "elsewhere"
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); !errors.Is(err, convbackup.ErrNoHome) || f.calls() != "suspend,resume" {
		t.Fatalf("no home: err %v calls %q", err, f.calls())
	}
}

// TestNewConversationStopNotLanded: a stop that leaves the record suspended
// ended nothing, so the manager is resumed on its old session.
func TestNewConversationStopNotLanded(t *testing.T) {
	f, sess := newConvFixtureWith(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	f.sc.stopErr = errors.New("hub said no")
	_, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
	if err == nil || !strings.Contains(err.Error(), "was not ended") {
		t.Fatalf("err = %v", err)
	}
	if got := f.calls(); got != "suspend,stop,resume" {
		t.Fatalf("calls = %q", got)
	}
	if sess.began != 0 {
		t.Fatalf("session = %+v", sess)
	}

	// A stop that errs but lands goes on.
	f, _ = newConvFixtureWith(t)
	f.sc.stopErr, f.sc.stopLands = errors.New("timeout"), true
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err != nil || f.calls() != "suspend,stop,resume-fresh" {
		t.Fatalf("landed stop: err %v calls %q", err, f.calls())
	}
}

// TestNewConversationFailureAfterStop: once the old session ended no resume
// can continue it; the manager stays down and the error names the backup.
func TestNewConversationFailureAfterStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*convFixture, *newConvSession)
		calls string
	}{
		{"start fails", func(f *convFixture, _ *newConvSession) { f.sc.resumeErr = errors.New("broker down") }, "suspend,stop,resume-fresh"},
		{"session begin fails", func(_ *convFixture, s *newConvSession) { s.beginErr = errors.New("disk full") }, "suspend,stop"},
		{"resume check refuses", func(f *convFixture, _ *newConvSession) { f.guard.failAt = "begin" }, "suspend,stop"},
		{"role guard refuses", func(f *convFixture, _ *newConvSession) {
			f.ops.verifyRole = func(context.Context) error { return errors.New("pre-role record") }
		}, "suspend,stop"},
		{"gate fails", func(f *convFixture, _ *newConvSession) {
			f.ops.gate = func(context.Context) error { return errors.New("claude died") }
		}, "suspend,stop,resume-fresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := newConvFixtureWith(t)
			writeConvFile(t, f.transcript("old.jsonl"), "old\n")
			tc.setup(f, sess)
			m, saved, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
			if err == nil || !saved || !strings.Contains(err.Error(), "lever conversation restore "+m.ID) {
				t.Fatalf("err = %v saved %v", err, saved)
			}
			if got := f.calls(); got != tc.calls {
				t.Fatalf("calls = %q, want %q", got, tc.calls)
			}
			var ee *cli.ExitError
			if errors.As(err, &ee) {
				t.Fatalf("exit code %d: a resume failure's code says the conversation was kept", ee.Code)
			}
		})
	}
}

// TestNewConversationPhases: a suspended manager is used as it is and left
// suspended when the backup fails; any other phase, or no record, changes
// nothing.
func TestNewConversationPhases(t *testing.T) {
	f, _ := newConvFixtureWith(t)
	f.sc.phase = scion.PhaseSuspended
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err != nil || f.calls() != "stop,resume-fresh" {
		t.Fatalf("suspended: err %v calls %q", err, f.calls())
	}
	f, _ = newConvFixtureWith(t)
	f.sc.phase = scion.PhaseSuspended
	f.ops.guest.Agent = "elsewhere"
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err == nil || len(f.sc.calls) != 0 {
		t.Fatalf("suspended, no backup: err %v calls %q, want it left suspended", err, f.calls())
	}
	for _, phase := range []string{scion.PhaseStopped, scion.PhaseError, "starting", ""} {
		f, _ := newConvFixtureWith(t)
		f.sc.phase = phase
		if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err == nil || len(f.sc.calls) != 0 {
			t.Fatalf("phase %q: err %v calls %q", phase, err, f.calls())
		}
	}
}

// TestLockConversation: one change to the manager record at a time across
// new, restore, up, apply, stop and reload, with its own exit code and the holder
// named; new and restore refuse a state directory inside the tree, where the
// others take no lock.
func TestLockConversation(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree")}
	unlock, err := lockConversation(app, st, "lever conversation new", " from the chat page")
	if err != nil {
		t.Fatal(err)
	}
	if got := managerLockHolder(app, st); !strings.Contains(got, "`lever conversation new` from the chat page (pid ") {
		t.Fatalf("holder = %q", got)
	}
	for _, verb := range []string{"lever conversation restore", "lever up", "lever apply", "lever stop", "lever reload"} {
		take := func() (func(), error) { return lockLifecycle(app, st, verb) }
		if verb == "lever conversation restore" {
			take = func() (func(), error) { return lockConversation(app, st, verb, "") }
		}
		_, err = take()
		var ee *cli.ExitError
		if !errors.As(err, &ee) || ee.Code != exitConversationLocked {
			t.Fatalf("%s: %v", verb, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "from the chat page") || !strings.Contains(msg, "Run `"+verb+"` again") {
			t.Fatalf("%s: %v", verb, err)
		}
	}
	unlock()
	if got := managerLockHolder(app, st); got != "" {
		t.Fatalf("holder after release = %q", got)
	}
	up, err := lockLifecycle(app, st, "lever up")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConversation(app, st, "lever conversation new", ""); err == nil || !strings.Contains(err.Error(), "`lever up` (pid ") {
		t.Fatalf("new under up: %v", err)
	}
	up()
	in := &config.App{Name: "mgr", Tree: root}
	if _, err := lockConversation(in, st, "lever conversation new", ""); !errors.Is(err, errConvStateInTree) {
		t.Fatalf("state in tree: %v", err)
	}
	free, err := lockLifecycle(in, st, "lever up")
	if err != nil {
		t.Fatalf("up with the state in the tree: %v", err)
	}
	free()
}

// TestNewConversationEmptyTranscripts: transcripts with no content are not
// saved (no backup is pushed out for them), and the reset goes on.
func TestNewConversationEmptyTranscripts(t *testing.T) {
	f, _ := newConvFixtureWith(t)
	writeConvFile(t, f.transcript("empty.jsonl"), "")
	_, saved, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
	if err != nil || saved || f.calls() != "suspend,stop,resume-fresh" {
		t.Fatalf("err %v saved %v calls %q", err, saved, f.calls())
	}
	store, _ := f.ops.store()
	if all, _ := store.List(); len(all) != 0 {
		t.Fatalf("backups = %+v", all)
	}
}

// TestBeginFreshSession: the new session is recorded on the skill on disk;
// one whose skill cannot be read is recorded on none, before the start, so
// the previous record never vouches for it.
func TestBeginFreshSession(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	tree := filepath.Join(root, "tree")
	for _, d := range []string{st.Dir, tree} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	app := &config.App{Name: "mgr", Tree: tree}
	if err := sessionrec.Append(st.Sessions(), sessionrec.Record{Agent: "mgr", SkillHash: "old", Started: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	latest := func() string {
		recs, err := sessionrec.Latest(st.Sessions())
		if err != nil {
			t.Fatal(err)
		}
		return recs["mgr"].SkillHash
	}
	commit, err := beginFreshSession(app, st)()
	if err != nil {
		t.Fatal(err)
	}
	if got := latest(); got != brokerctl.UncheckedSkill {
		t.Fatalf("before the start: %q, want the fence", got)
	}
	if commit() == nil {
		t.Fatal("commit of an unreadable skill succeeded")
	}
	rel, _ := sessionrec.SkillRel(app, "mgr")
	writeConvFile(t, filepath.Join(tree, rel), "skill text\n")
	commit, err = beginFreshSession(app, st)()
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	if got := latest(); got == brokerctl.UncheckedSkill || got == "old" {
		t.Fatalf("after a readable skill: %q", got)
	}
}

// TestNewConversationBootTask: the prompt is read before anything changes, so
// an unreadable one changes nothing; with no prompt_file the record's stored
// task runs (a plain resume of the stopped record).
func TestNewConversationBootTask(t *testing.T) {
	f, sess := newConvFixtureWith(t)
	f.ops.bootTask = func() (string, error) { return "", errors.New("reading manager prompt /x: denied") }
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if len(f.sc.calls) != 0 || sess.asked != 0 {
		t.Fatalf("calls = %v session %+v, want none", f.sc.calls, sess)
	}

	f, _ = newConvFixtureWith(t)
	f.ops.bootTask = func() (string, error) { return "", nil }
	if _, _, err := f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls(); got != "suspend,stop,resume-fresh" || f.sc.task != "" {
		t.Fatalf("calls = %q task %q", got, f.sc.task)
	}
}
