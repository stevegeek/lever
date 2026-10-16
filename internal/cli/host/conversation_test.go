package host

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// fakeConvScion is a manager record a restore or a new conversation drives:
// Suspend, Stop and Resume move its phase, and every call is recorded.
type fakeConvScion struct {
	mu         sync.Mutex
	phase      string
	activity   string
	calls      []string
	suspendErr error
	resumeErr  error
	// stopErr fails Stop; stopLands says whether the phase moves anyway.
	stopErr   error
	stopLands bool
	// task is the last task ResumeTask passed ("" for none).
	task string
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
	return []scion.Agent{{Slug: "mgr", Phase: f.phase, Activity: f.activity, ContainerStatus: cs}}, nil
}

func (f *fakeConvScion) Stop(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	if f.stopErr == nil || f.stopLands {
		f.phase = scion.PhaseStopped
	}
	return f.stopErr
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

// Resume continues a suspended record's session ("resume") and starts a
// stopped one in a new session ("resume-fresh"), as the hub does.
func (f *fakeConvScion) Resume(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phase == scion.PhaseStopped {
		f.calls = append(f.calls, "resume-fresh")
	} else {
		f.calls = append(f.calls, "resume")
	}
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.phase = scion.PhaseRunning
	return nil
}

// ResumeTask is Resume with a task; on a suspended record the hub would add
// it to the continued session ("resume-task"), which no caller may do.
func (f *fakeConvScion) ResumeTask(ctx context.Context, w, p, task string) error {
	f.mu.Lock()
	f.task = task
	if f.phase != scion.PhaseStopped {
		f.calls = append(f.calls, "resume-task")
		f.mu.Unlock()
		return errors.New("fake: a task on a record that is not stopped")
	}
	f.mu.Unlock()
	return f.Resume(ctx, w, p)
}

// convFixture is an instance and a fake jail HOME laid out as scion lays out
// the run user's, with the manager "mgr"'s home. The guest scripts run with
// the real bash and GNU tar.
type convFixture struct {
	ops       convOps
	sc        *fakeConvScion
	agentHome string
	home      string
	logs      []string
	gated     int
	guard     *fakeResumeGuard
}

// fakeResumeGuard records the checks around a resume, in order, and fails the
// step named in failAt.
type fakeResumeGuard struct {
	steps  []string
	failAt string
}

func (g *fakeResumeGuard) step(name string) error {
	g.steps = append(g.steps, name)
	if g.failAt == name {
		return errors.New(name + " refused")
	}
	return nil
}

func (g *fakeResumeGuard) begin(context.Context) (managerResume, error) {
	if err := g.step("begin"); err != nil {
		return nil, err
	}
	return g, nil
}
func (g *fakeResumeGuard) Resumed(context.Context) error { return g.step("resumed") }
func (g *fakeResumeGuard) Live(context.Context) error    { return g.step("live") }
func (g *fakeResumeGuard) Settle(context.Context) error  { return g.step("settle") }
func (g *fakeResumeGuard) AfterFailure(_ context.Context, cause error) error {
	if err := g.step("after-failure"); err != nil {
		return errors.Join(cause, err)
	}
	return cause
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
	f := &convFixture{sc: &fakeConvScion{phase: scion.PhaseRunning}, agentHome: ah, home: home, guard: &fakeResumeGuard{}}
	f.ops = convOps{
		app:         &config.App{Name: "mgr", Tree: tree},
		st:          state.State{Dir: stDir},
		guest:       convbackup.Guest{R: proc.RealRunner{}, Workspace: "/lever", Agent: "mgr"},
		sc:          f.sc,
		log:         func(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) },
		verifyRole:  func(context.Context) error { return nil },
		gate:        func(context.Context) error { f.gated++; return nil },
		beginResume: f.guard.begin,
		suspendWait: apply.RetryBudget{Attempts: 3, Interval: time.Millisecond},
		recordBoot:  func(ctx context.Context) { _ = f.guard.step("boot") },
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

	// The fresh create: a new home with the boot turn's session, a fresh session
	// record on the current skill, and a running manager.
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
	if got := strings.Join(f.guard.steps, ","); got != "begin,resumed,live,boot" {
		t.Fatalf("resume checks = %q, want begin,resumed,live (lever#158), then the guest boot recorded", got)
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
	if got := strings.Join(f.guard.steps, ","); got != "begin,resumed,settle" {
		t.Fatalf("resume checks = %q, want begin,resumed,settle: no gate follows that resume (lever#158)", got)
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
	// backupFirst saved the replaced conversation: two backups now.
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

// lostInstallRunner fails every install like a lost transport: no exit status
// the script chose.
type lostInstallRunner struct{ proc.RealRunner }

func (lostInstallRunner) RunStdin(context.Context, io.Reader, map[string]string, string, ...string) (proc.Result, error) {
	return proc.Result{Code: 255}, errors.New("transport lost")
}

// TestRestoreUnsureInstallRefusesContacts: an install failure that may have
// come after the swap records the unknown skill, never the previous one, and
// says the manager holds either conversation.
func TestRestoreUnsureInstallRefusesContacts(t *testing.T) {
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	m, _, err := f.ops.backup(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionrec.Append(f.ops.st.Sessions(), sessionrec.Record{Agent: "mgr", SkillHash: "current", Started: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	f.ops.guest.R = lostInstallRunner{}
	err = f.ops.restore(context.Background(), m.ID, false)
	if err == nil || !strings.Contains(err.Error(), "whichever conversation") {
		t.Fatalf("err = %v", err)
	}
	if got := f.latestSkill(t); got != restoredUnknownSkill {
		t.Fatalf("session record = %q, want the unknown skill", got)
	}
	if strings.Join(f.sc.calls, ",") != "suspend,resume" {
		t.Fatalf("calls = %v", f.sc.calls)
	}
}

// TestFreshBackupNoHomeFailsClosed: a record whose home lever cannot find
// stops the fresh start, and the manager the hook suspended is resumed
// through the role guard; a guard refusal leaves it suspended.
func TestFreshBackupNoHomeFailsClosed(t *testing.T) {
	f := newConvFixture(t)
	f.ops.guest.Agent = "elsewhere"
	var saved *convbackup.Meta
	err := f.ops.backupBeforeDelete(&saved)(context.Background(), &scion.Agent{Slug: "mgr", Phase: scion.PhaseRunning})
	if !errors.Is(err, convbackup.ErrNoHome) || saved != nil {
		t.Fatalf("err = %v saved %v", err, saved)
	}
	if strings.Join(f.sc.calls, ",") != "suspend,resume" {
		t.Fatalf("calls = %v", f.sc.calls)
	}

	f.sc.phase, f.sc.calls = scion.PhaseRunning, nil
	guard := errors.New("pre-role record")
	f.ops.verifyRole = func(context.Context) error { return guard }
	err = f.ops.backupBeforeDelete(&saved)(context.Background(), &scion.Agent{Slug: "mgr", Phase: scion.PhaseRunning})
	if !errors.Is(err, guard) || strings.Join(f.sc.calls, ",") != "suspend" {
		t.Fatalf("guard: err %v calls %v", err, f.sc.calls)
	}
}

// TestRestoreResumeChecks: every resume a restore makes passes the tree-mount
// checks. A refused pre-check leaves the manager suspended, and a failed check
// after the resume fails the restore before the gate reports it up.
func TestRestoreResumeChecks(t *testing.T) {
	for _, tc := range []struct {
		failAt    string
		wantCalls string
		wantSteps string
		wantGated int
	}{
		{"begin", "suspend", "begin", 0},
		{"resumed", "suspend,resume", "begin,resumed", 0},
		{"live", "suspend,resume", "begin,resumed,live", 1},
	} {
		t.Run(tc.failAt, func(t *testing.T) {
			f := newConvFixture(t)
			writeConvFile(t, f.transcript("old.jsonl"), "old\n")
			m, found, err := f.ops.backup(context.Background(), "manual")
			if err != nil || !found {
				t.Fatalf("backup: %v %v", found, err)
			}
			f.guard.failAt = tc.failAt
			err = f.ops.restore(context.Background(), m.ID, false)
			if err == nil || !strings.Contains(err.Error(), tc.failAt+" refused") {
				t.Fatalf("err = %v, want the %s refusal", err, tc.failAt)
			}
			if got := strings.Join(f.sc.calls, ","); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
			if got := strings.Join(f.guard.steps, ","); got != tc.wantSteps || f.gated != tc.wantGated {
				t.Fatalf("checks = %q gated %d, want %q gated %d", got, f.gated, tc.wantSteps, tc.wantGated)
			}
		})
	}
	// A failed resume is checked too: the container may run all the same.
	f0 := newConvFixture(t)
	writeConvFile(t, f0.transcript("old.jsonl"), "old\n")
	m0, _, err0 := f0.ops.backup(context.Background(), "manual")
	if err0 != nil {
		t.Fatal(err0)
	}
	f0.sc.resumeErr = errors.New("hook failed")
	if err := f0.ops.restore(context.Background(), m0.ID, false); err == nil || strings.Join(f0.guard.steps, ",") != "begin,after-failure" || f0.gated != 0 {
		t.Fatalf("err = %v checks %q gated %d, want begin,after-failure", err, f0.guard.steps, f0.gated)
	}
	// The resume after a failed restore: a refused pre-check keeps the
	// manager suspended and says so.
	f := newConvFixture(t)
	f.sc.phase = scion.PhaseSuspended
	f.guard.failAt = "begin"
	err := f.ops.resumeAfterFailure(context.Background(), errors.New("cause"), "")
	if err == nil || !strings.Contains(err.Error(), "the manager stays suspended: begin refused") || len(f.sc.calls) != 0 {
		t.Fatalf("err = %v calls %v", err, f.sc.calls)
	}
	// A check after it fails: the error says the manager was resumed and
	// what the check found (the guard suspended it).
	f = newConvFixture(t)
	f.guard.failAt = "settle"
	err = f.ops.resumeAfterFailure(context.Background(), errors.New("cause"), "")
	if err == nil || !strings.Contains(err.Error(), "settle refused") || strings.Join(f.sc.calls, ",") != "resume" {
		t.Fatalf("err = %v calls %v", err, f.sc.calls)
	}
	f = newConvFixture(t)
	f.ops.beginResume = nil
	err = f.ops.resumeAfterFailure(context.Background(), errors.New("cause"), "")
	if err == nil || len(f.sc.calls) != 0 {
		t.Fatalf("err = %v calls %v, want no resume without the checks", err, f.sc.calls)
	}
}

// TestConversationEmpty: `lever up` starts a manager fresh on its own only
// when the guest shows its home holds no transcript; anything else is a
// reason, never a pass.
func TestConversationEmpty(t *testing.T) {
	f := newConvFixture(t)
	if err := f.ops.conversationEmpty(context.Background()); err != nil {
		t.Fatalf("no .claude: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.agentHome, ".claude", "projects", "-workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.ops.conversationEmpty(context.Background()); err != nil {
		t.Fatalf("empty projects: %v", err)
	}
	writeConvFile(t, f.transcript("s.jsonl"), "{}\n")
	if err := f.ops.conversationEmpty(context.Background()); err == nil || !strings.Contains(err.Error(), "holds transcripts") {
		t.Fatalf("a transcript: %v", err)
	}
	if err := os.Remove(f.transcript("s.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.home, f.transcript("l.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := f.ops.conversationEmpty(context.Background()); err == nil || !strings.Contains(err.Error(), "links") {
		t.Fatalf("a link: %v", err)
	}
	f.ops.guest.Agent = "other"
	if err := f.ops.conversationEmpty(context.Background()); !errors.Is(err, convbackup.ErrNoHome) {
		t.Fatalf("no home: %v", err)
	}
}

// TestNoConversationBackupSkipsEmpty: with no transcript content nothing is
// saved or pruned, so an agent cannot rotate the real backups out of the
// store; content is saved under its own reason.
func TestNoConversationBackupSkipsEmpty(t *testing.T) {
	f := newConvFixture(t)
	store, err := f.ops.store()
	if err != nil {
		t.Fatal(err)
	}
	writeConvFile(t, f.transcript("empty.jsonl"), "")
	var saved *convbackup.Meta
	if err := f.ops.backupBeforeNoConversationFresh(&saved)(context.Background(), &scion.Agent{Phase: scion.PhaseError}); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.List(); saved != nil || len(all) != 0 {
		t.Fatalf("an empty conversation was saved: %v %v", saved, all)
	}
	writeConvFile(t, f.transcript("s.jsonl"), "{}\n")
	if err := f.ops.backupBeforeNoConversationFresh(&saved)(context.Background(), &scion.Agent{Phase: scion.PhaseError}); err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.Reason != "up (no conversation)" {
		t.Fatalf("saved = %+v", saved)
	}
}
