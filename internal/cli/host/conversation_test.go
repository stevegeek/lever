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
	"github.com/stevegeek/lever/internal/jail"
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
	// container, when set, is the container status List reports until a
	// verb changes the phase; otherwise it follows the phase.
	container string
	// deadAfterResume: a resume's container exits at once.
	deadAfterResume bool
}

// The verbs no conversation command makes.
func (f *fakeConvScion) Start(context.Context, scion.StartOpts) error {
	return errors.New("fake: no create here")
}
func (f *fakeConvScion) ResumeForce(context.Context, string, string) error {
	return errors.New("fake: no forced resume here")
}
func (f *fakeConvScion) Delete(context.Context, string, string) error {
	return errors.New("fake: no delete here")
}
func (f *fakeConvScion) ResetAuth(context.Context, string, string) error {
	return errors.New("fake: no token reset here")
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
	if f.container != "" {
		cs = f.container
	}
	return []scion.Agent{{Slug: "mgr", Phase: f.phase, Activity: f.activity, ContainerStatus: cs, Image: "lever.local/lever-claude:1"}}, nil
}

func (f *fakeConvScion) Stop(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	f.container = ""
	if f.stopErr == nil || f.stopLands {
		f.phase = scion.PhaseStopped
	}
	return f.stopErr
}

func (f *fakeConvScion) Suspend(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "suspend")
	f.container = ""
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
	f.container = ""
	if f.phase == scion.PhaseStopped {
		f.calls = append(f.calls, "resume-fresh")
	} else {
		f.calls = append(f.calls, "resume")
	}
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.phase = scion.PhaseRunning
	if f.deadAfterResume {
		f.container = "Exited (1) 1 second ago"
	}
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
// the real bash and GNU tar. The manager holds one tree mount, kb, read-only.
type convFixture struct {
	ops       convOps
	sc        *fakeConvScion
	agentHome string
	home      string
	tree      string
	logs      []string
	mounts    *fakeMountChecks
	boots     *fakeConvBoot
	roleErr   error
}

// fakeMountChecks is the guest's read-back of the manager's tree mounts: one
// self-mount, kb, and the live check's answers in turn ("ok", "notrunning",
// "elsewhere"; the last repeats, none is ok).
type fakeMountChecks struct {
	mu      sync.Mutex
	answers []string
	checks  int
}

func (m *fakeMountChecks) inspect(context.Context, string) ([]jail.Mount, error) {
	return []jail.Mount{{Source: "/lever/kb", Destination: "/workspace/kb"}}, nil
}

func (m *fakeMountChecks) check(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := "ok"
	if len(m.answers) > 0 {
		a = m.answers[min(m.checks, len(m.answers)-1)]
	}
	m.checks++
	switch a {
	case "notrunning":
		return nil, jail.ErrContainerNotRunning
	case "elsewhere":
		return []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}, nil
	}
	return nil, nil
}

// fakeConvBoot records the guest boots the manager was seen live in.
type fakeConvBoot struct{ records []string }

func (b *fakeConvBoot) Current(context.Context) (string, error) { return "boot-1", nil }
func (b *fakeConvBoot) Recorded() (string, error) {
	if len(b.records) == 0 {
		return "", nil
	}
	return b.records[len(b.records)-1], nil
}
func (b *fakeConvBoot) Record(id string) error { b.records = append(b.records, id); return nil }

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
	if err := os.Mkdir(filepath.Join(tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &convFixture{sc: &fakeConvScion{phase: scion.PhaseRunning}, agentHome: ah, home: home, tree: tree,
		mounts: &fakeMountChecks{}, boots: &fakeConvBoot{}}
	app := &config.App{Name: "mgr", Tree: tree}
	app.Manager.ReadOnly = []string{"kb"}
	log := func(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) }
	f.ops = convOps{
		app:   app,
		st:    state.State{Dir: stDir},
		guest: convbackup.Guest{R: proc.RealRunner{}, Workspace: "/lever", Agent: "mgr"},
		log:   log,
		deps: apply.Deps{
			ManagerVerbs: f.sc, JailMount: "/lever", ManagerLock: apply.HeldManagerLock, Log: log,
			VerifyAgentRole:        func(context.Context, string, string) error { return f.roleErr },
			InspectContainerMounts: f.mounts.inspect, CheckLiveMounts: f.mounts.check,
			GuestBoot:        f.boots,
			ManagerLiveRetry: apply.RetryBudget{Attempts: 3, Interval: time.Millisecond},
			ManagerPhaseWait: apply.RetryBudget{Attempts: 3, Interval: time.Millisecond},
			BrokerStartRetry: apply.RetryBudget{Attempts: 1, Interval: time.Millisecond},
		},
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
// home: the backup before the delete stores the transcripts with the
// session's skill (the lifecycle suspended the manager before it); the fresh
// record's session is moved aside, the old one restored, the restored
// session recorded with the old skill, and the manager resumed through its
// mount read-back, gated, and its guest boot recorded.
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
	if len(f.sc.calls) != 0 {
		t.Fatalf("calls = %v, want none: the lifecycle suspends, the hook only copies", f.sc.calls)
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
	if strings.Join(f.sc.calls, ",") != "suspend,resume" {
		t.Fatalf("calls = %v", f.sc.calls)
	}
	if f.mounts.checks != 1 || len(f.boots.records) != 1 {
		t.Fatalf("mount checks %d, boots %v: want the mounts read back after the resume, then the guest boot recorded", f.mounts.checks, f.boots.records)
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
	if strings.Join(f.sc.calls, ",") != "suspend,resume" {
		t.Fatalf("calls = %v, want the manager resumed after the failure", f.sc.calls)
	}
	if f.mounts.checks != 1 || len(f.boots.records) != 0 {
		t.Fatalf("mount checks %d boots %v: want the mounts read back, and no gate (no boot recorded) after that resume", f.mounts.checks, f.boots.records)
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
	f.roleErr = guard
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
// stops the fresh start; the hook changes nothing itself.
func TestFreshBackupNoHomeFailsClosed(t *testing.T) {
	f := newConvFixture(t)
	f.ops.guest.Agent = "elsewhere"
	var saved *convbackup.Meta
	err := f.ops.backupBeforeDelete(&saved)(context.Background(), &scion.Agent{Slug: "mgr", Phase: scion.PhaseRunning})
	if !errors.Is(err, convbackup.ErrNoHome) || saved != nil || len(f.sc.calls) != 0 {
		t.Fatalf("err = %v saved %v calls %v", err, saved, f.sc.calls)
	}
}

// TestRestoreResumeChecks: every resume a restore makes passes the tree-mount
// checks. A refused pre-check leaves the manager suspended, and a failed check
// after the resume, right after it or once live, suspends it and fails the
// restore before the gate reports it up.
func TestRestoreResumeChecks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noDir     bool
		answers   []string
		wantErr   string
		wantCalls string
	}{
		{name: "before the resume", noDir: true, wantErr: "the manager stays suspended", wantCalls: "suspend"},
		{name: "after the resume", answers: []string{"elsewhere"}, wantErr: "mounted from another place", wantCalls: "suspend,resume,suspend"},
		{name: "once live", answers: []string{"notrunning", "elsewhere"}, wantErr: "mounted from another place", wantCalls: "suspend,resume,suspend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConvFixture(t)
			writeConvFile(t, f.transcript("old.jsonl"), "old\n")
			m, found, err := f.ops.backup(context.Background(), "manual")
			if err != nil || !found {
				t.Fatalf("backup: %v %v", found, err)
			}
			if tc.noDir {
				if err := os.Remove(filepath.Join(f.tree, "kb")); err != nil {
					t.Fatal(err)
				}
			}
			f.mounts.answers = tc.answers
			err = f.ops.restore(context.Background(), m.ID, false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if got := strings.Join(f.sc.calls, ","); got != tc.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tc.wantCalls)
			}
			if len(f.boots.records) != 0 {
				t.Fatalf("boot recorded for a manager that failed its checks: %v", f.boots.records)
			}
		})
	}
	// A failed resume is checked too: the container may run all the same.
	f := newConvFixture(t)
	writeConvFile(t, f.transcript("old.jsonl"), "old\n")
	m, _, err := f.ops.backup(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	f.sc.resumeErr = errors.New("hook failed")
	f.mounts.answers = []string{"elsewhere"}
	if err := f.ops.restore(context.Background(), m.ID, false); err == nil || !strings.Contains(err.Error(), "mounted from another place") || f.mounts.checks != 1 {
		t.Fatalf("err = %v checks %d, want the failed resume's container checked", err, f.mounts.checks)
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
