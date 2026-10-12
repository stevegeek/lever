package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
)

// fakeConvScion is a manager record a restore drives: Suspend and Resume
// move its phase, and every call is recorded in order.
type fakeConvScion struct {
	mu         sync.Mutex
	phase      string
	calls      []string
	suspendErr error
	resumeErr  error
}

func (f *fakeConvScion) List(context.Context, string) ([]scion.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phase == "" {
		return nil, nil
	}
	cs := "stopped"
	if f.phase == scion.PhaseRunning {
		cs = "Up 1 minute"
	}
	return []scion.Agent{{Slug: "mgr", Phase: f.phase, ContainerStatus: cs}}, nil
}

func (f *fakeConvScion) Suspend(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "suspend")
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.phase = scion.PhaseSuspended
	return nil
}

func (f *fakeConvScion) Resume(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "resume")
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.phase = scion.PhaseRunning
	return nil
}

// convFixture is an instance (tree and state outside it) and a fake jail
// HOME laid out as scion lays out the run user's, with the manager "mgr"'s
// home. The guest scripts run with the real bash and GNU tar.
type convFixture struct {
	ops       convOps
	sc        *fakeConvScion
	agentHome string
	home      string
	logs      []string
	gated     int
}

func newConvFixture(t *testing.T) *convFixture {
	t.Helper()
	if out, err := exec.Command("tar", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("needs GNU tar")
	}
	if out, err := exec.Command("du", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("needs GNU coreutils")
	}
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	stDir := filepath.Join(root, state.DirName)
	home := filepath.Join(root, "jailhome")
	for _, d := range []string{tree, stDir, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	entry := filepath.Join(home, ".scion", "project-configs", "lever__abc", ".scion")
	ah := filepath.Join(entry, "agents", "mgr", "home")
	writeConvFile(t, filepath.Join(entry, "settings.yaml"), "workspace_path: /lever\n")
	if err := os.MkdirAll(ah, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &convFixture{sc: &fakeConvScion{phase: scion.PhaseRunning}, agentHome: ah, home: home}
	f.ops = convOps{
		app:         &config.App{Name: "mgr", Tree: tree},
		st:          state.State{Dir: stDir},
		guest:       convbackup.Guest{R: proc.RealRunner{}, Workspace: "/lever", Agent: "mgr"},
		sc:          f.sc,
		log:         func(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) },
		verifyRole:  func(context.Context) error { return nil },
		gate:        func(context.Context) error { f.gated++; return nil },
		suspendWait: apply.RetryBudget{Attempts: 3, Interval: time.Millisecond},
	}
	return f
}

func writeConvFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *convFixture) transcript(name string) string {
	return filepath.Join(f.agentHome, ".claude", "projects", "-workspace", name)
}

func (f *convFixture) latestSkill(t *testing.T) string {
	t.Helper()
	recs, err := sessionrec.Latest(f.ops.st.Sessions())
	if err != nil {
		t.Fatal(err)
	}
	return recs["mgr"].SkillHash
}

// TestFreshBackupThenRestore is the whole --keep-conversation path on one
// home: the backup before the delete suspends the running manager and
// stores its transcripts with the session's skill; the fresh record's
// session is moved aside, the old one restored, the restored session
// recorded with the old skill, and the manager resumed and gated.
func TestFreshBackupThenRestore(t *testing.T) {
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "{\"old\":1}\n")
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := sessionrec.Append(f.ops.st.Sessions(), sessionrec.Record{Agent: "mgr", SkillHash: "skill-v1", Started: t0}); err != nil {
		t.Fatal(err)
	}

	var saved *convbackup.Meta
	if err := f.ops.backupBeforeDelete(&saved)(context.Background(), &scion.Agent{Slug: "mgr", Phase: scion.PhaseRunning}); err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.Files != 1 || saved.SkillHash != "skill-v1" || saved.Reason != "up --fresh" {
		t.Fatalf("saved = %+v", saved)
	}
	if strings.Join(f.sc.calls, ",") != "suspend" {
		t.Fatalf("calls = %v, want the running manager suspended before the copy", f.sc.calls)
	}
	fi, err := os.Stat(filepath.Join(f.ops.st.Dir, convbackup.DirName, saved.ID))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("backup dir: %v %v", fi, err)
	}

	// The fresh create: a new home with the boot turn's session, a fresh
	// session record on the current skill, and a running manager.
	if err := os.RemoveAll(filepath.Join(f.agentHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	writeConvFile(t, f.transcript("fresh.jsonl"), "{\"fresh\":1}\n")
	if err := sessionrec.Append(f.ops.st.Sessions(), sessionrec.Record{Agent: "mgr", SkillHash: "skill-v2", Started: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	f.sc.phase, f.sc.calls = scion.PhaseRunning, nil

	if err := f.ops.restore(context.Background(), saved.ID, false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.sc.calls, ",") != "suspend,resume" || f.gated != 1 {
		t.Fatalf("calls = %v, gated %d", f.sc.calls, f.gated)
	}
	if b, err := os.ReadFile(f.transcript("old.jsonl")); err != nil || string(b) != "{\"old\":1}\n" {
		t.Fatalf("restored: %q %v", b, err)
	}
	if _, err := os.Stat(f.transcript("fresh.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("the fresh session is still where --continue looks: %v", err)
	}
	if got := f.latestSkill(t); got != "skill-v1" {
		t.Fatalf("session record skill = %q, want the restored conversation's skill-v1", got)
	}
}

// TestRestoreFailureResumesAndKeepsRecord: an install the agent blocked (a
// .claude link) resumes the manager the restore suspended, on its own
// conversation, and puts its session record back.
func TestRestoreFailureResumesAndKeepsRecord(t *testing.T) {
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	m, found, err := f.ops.backup(context.Background(), "manual")
	if err != nil || !found {
		t.Fatalf("backup: %v %v", found, err)
	}
	if err := sessionrec.Append(f.ops.st.Sessions(), sessionrec.Record{Agent: "mgr", SkillHash: "current", Started: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.agentHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.agentHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	err = f.ops.restore(context.Background(), m.ID, false)
	if !errors.Is(err, convbackup.ErrNotPlainDir) {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(f.sc.calls, ",") != "suspend,resume" || f.gated != 0 {
		t.Fatalf("calls = %v gated %d, want the manager resumed after the failure", f.sc.calls, f.gated)
	}
	if got := f.latestSkill(t); got != "current" {
		t.Fatalf("session record = %q, want it put back to current", got)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote through the link: %v", ents)
	}
}

// TestRestoreUnknownSkillRefusesContacts: a backup that does not know its
// session's skill is recorded with a hash no skill has.
func TestRestoreUnknownSkillRefusesContacts(t *testing.T) {
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	m, _, err := f.ops.backup(context.Background(), "manual")
	if err != nil || m.SkillHash != "" {
		t.Fatalf("backup: %+v %v", m, err)
	}
	f.sc.phase = scion.PhaseSuspended
	if err := f.ops.restore(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if got := f.latestSkill(t); got != restoredUnknownSkill {
		t.Fatalf("session record = %q", got)
	}
	// backupFirst saved the conversation it replaced: two backups now.
	store, _ := f.ops.store()
	if all, _ := store.List(); len(all) != 2 || all[0].Reason != "before restore" {
		t.Fatalf("backups = %+v", all)
	}
	// The manager was suspended already: no suspend, one resume.
	if strings.Join(f.sc.calls, ",") != "resume" {
		t.Fatalf("calls = %v", f.sc.calls)
	}
}

// TestRestoreRefusals: a manager in a phase a restore cannot use, no
// record, a role guard refusal (never resumed), and a state directory inside
// the tree.
func TestRestoreRefusals(t *testing.T) {
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	m, _, err := f.ops.backup(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}

	f.sc.phase = scion.PhaseStopped
	if err := f.ops.restore(context.Background(), m.ID, false); err == nil || !strings.Contains(err.Error(), "running or suspended") {
		t.Fatalf("stopped: %v", err)
	}
	f.sc.phase = ""
	if err := f.ops.restore(context.Background(), m.ID, false); err == nil || !strings.Contains(err.Error(), "no manager record") {
		t.Fatalf("absent: %v", err)
	}
	if len(f.sc.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.sc.calls)
	}

	guard := errors.New("pre-role record")
	f.sc.phase = scion.PhaseRunning
	f.ops.verifyRole = func(context.Context) error { return guard }
	if err := f.ops.restore(context.Background(), m.ID, false); !errors.Is(err, guard) {
		t.Fatalf("guard: %v", err)
	}
	if strings.Join(f.sc.calls, ",") != "suspend" {
		t.Fatalf("calls = %v: a record the role guard refused must not be resumed", f.sc.calls)
	}

	in := *f.ops.app
	in.Tree = filepath.Dir(f.ops.st.Dir)
	f.ops.app = &in
	if _, _, err := f.ops.backup(context.Background(), "manual"); !errors.Is(err, errConvStateInTree) {
		t.Fatalf("state in tree: %v", err)
	}
}

// TestCheckFreshFlags: --no-backup and --keep-conversation need --fresh, and
// not each other.
func TestCheckFreshFlags(t *testing.T) {
	for _, c := range []struct {
		fresh, noBackup, keep, ok bool
	}{
		{false, false, false, true},
		{true, false, false, true},
		{true, true, false, true},
		{true, false, true, true},
		{false, true, false, false},
		{false, false, true, false},
		{true, true, true, false},
	} {
		if err := checkFreshFlags(c.fresh, c.noBackup, c.keep); (err == nil) != c.ok {
			t.Errorf("fresh=%v noBackup=%v keep=%v: %v", c.fresh, c.noBackup, c.keep, err)
		}
	}
}
