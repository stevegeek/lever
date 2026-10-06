package broker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

func stagedCN(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "bootstrap.json"))
	if err != nil {
		return ""
	}
	// crude but sufficient: the envelope carries the CN as a JSON string value.
	s := string(raw)
	for _, cn := range []string{"scratch", "test-manager"} {
		if strings.Contains(s, `"`+cn+`"`) {
			return cn
		}
	}
	return "?"
}

// A lapsed RUNNING worker: healed by stage + Suspend + Resume.
func TestHealLapsedRunningWorker(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "running", ContainerStatus: "Up 2 minutes"}},
	}}
	b, spec, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if bs := rt.lastStaged(t, spec.Name); bs.AgentCN != "scratch" {
		t.Fatalf("staged bootstrap CN = %q, want scratch", bs.AgentCN)
	}
	if len(rt.suspend) != 1 || rt.suspend[0] != "scratch" {
		t.Fatalf("suspend calls = %v, want [scratch]", rt.suspend)
	}
	if len(rt.resumed) != 1 || rt.resumed[0] != "scratch" {
		t.Fatalf("resume calls = %v, want [scratch]", rt.resumed)
	}
}

// A lapsed ERROR-phase worker: healed via ResumeForce (scion#895), no suspend.
func TestHealLapsedErrorWorkerUsesForce(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend) != 0 {
		t.Fatalf("suspend calls = %v, want none for error phase", rt.suspend)
	}
	if len(rt.resumeForced) != 1 || rt.resumeForced[0] != "scratch" {
		t.Fatalf("resumeForce calls = %v, want [scratch]", rt.resumeForced)
	}
}

// A lapsed worker in a mid-transition phase (e.g. "starting"): NOT bounceable
// — no verb fires; the audit trail tells the operator to run `lever up`.
func TestHealUnbounceablePhaseAuditsOnly(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "starting"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend)+len(rt.resumed)+len(rt.resumeForced) != 0 {
		t.Fatalf("verbs fired for unbounceable phase: suspend %v resume %v force %v",
			rt.suspend, rt.resumed, rt.resumeForced)
	}
}

// A lapsed suspended/stopped worker: plain Resume.
func TestHealLapsedSuspendedWorkerPlainResume(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "suspended"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend) != 0 || len(rt.resumeForced) != 0 || len(rt.resumed) != 1 {
		t.Fatalf("verbs = suspend %v force %v resume %v, want plain resume only",
			rt.suspend, rt.resumeForced, rt.resumed)
	}
}

// The MANAGER lapses: ticket staged to the manager bootstrap dir under the
// manager cert CN, bounce addressed to the manager's scion SLUG.
func TestHealLapsedManager(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "appname", Phase: "running", ContainerStatus: "Up 5 minutes"}},
	}}
	b, _, managerDir := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "test-manager")

	if cn := stagedCN(t, managerDir); cn != "test-manager" {
		t.Fatalf("staged manager bootstrap CN = %q, want test-manager", cn)
	}
	if len(rt.suspend) != 1 || rt.suspend[0] != "appname" {
		t.Fatalf("suspend calls = %v, want [appname] (the scion slug, not the CN)", rt.suspend)
	}
	if len(rt.resumed) != 1 || rt.resumed[0] != "appname" {
		t.Fatalf("resume calls = %v, want [appname]", rt.resumed)
	}
}

// Gate: revoked identities are NEVER healed — expiry stays a kill-switch.
func TestHealRefusesRevoked(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, spec, _ := reenrolBroker(t, rt, "all")
	b.Revoke("scratch")
	b.healLapse(context.Background(), "scratch")

	if len(rt.staged[spec.Name]) != 0 {
		t.Fatal("revoked identity must not get a staged ticket")
	}
	if len(rt.resumed)+len(rt.resumeForced)+len(rt.suspend) != 0 {
		t.Fatal("revoked identity must not be bounced")
	}
}

// Gate: unknown CNs (not the manager, not a configured worker) are ignored.
func TestHealIgnoresUnknownCN(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "интруder")
	if len(rt.resumed)+len(rt.resumeForced)+len(rt.suspend) != 0 {
		t.Fatal("unknown CN must not trigger any runtime verb")
	}
}

// Gate: mode "manager" heals the manager but drops workers; mode "off" drops all.
func TestHealModeGate(t *testing.T) {
	mk := func(mode string) (*Broker, *fakeRuntime) {
		rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
			testInstanceProject: {
				{Slug: "scratch", Phase: "error"},
				{Slug: "appname", Phase: "error"},
			},
		}}
		b, _, _ := reenrolBroker(t, rt, mode)
		return b, rt
	}

	b, rt := mk("manager")
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != 0 {
		t.Fatal("mode=manager must not heal a worker")
	}
	b.healLapse(context.Background(), "test-manager")
	if len(rt.resumeForced) != 1 || rt.resumeForced[0] != "appname" {
		t.Fatalf("mode=manager must heal the manager, got %v", rt.resumeForced)
	}

	b, rt = mk("off")
	b.healLapse(context.Background(), "test-manager")
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced)+len(rt.resumed)+len(rt.suspend) != 0 {
		t.Fatal("mode=off must heal nothing")
	}
	if b.lapseFunc() != nil {
		t.Fatal("mode=off must not even install the lapse hook")
	}
}

// Cooldown + attempt cap: repeated FAILING heals for one CN run once per
// cooldown window, at most reenrolMaxAttempts per burst; a long-quiet CN
// starts a fresh burst; a SUCCESS resets the counter immediately.
func TestHealCooldownAndCap(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	now := time.Unix(1_700_000_000, 0)
	b.reenrolNow = func() time.Time { return now }
	rt.resumeForceErr = context.DeadlineExceeded // every heal attempt FAILS

	b.healLapse(context.Background(), "scratch")
	b.healLapse(context.Background(), "scratch") // within cooldown: dropped
	if len(rt.resumeForced) != 1 {
		t.Fatalf("attempts within cooldown = %d, want 1", len(rt.resumeForced))
	}

	for i := 0; i < 4; i++ { // each round passes the cooldown; total stays inside the reset window
		now = now.Add(reenrolCooldown + time.Minute)
		b.healLapse(context.Background(), "scratch")
	}
	if len(rt.resumeForced) != reenrolMaxAttempts {
		t.Fatalf("burst attempts = %d, want cap %d", len(rt.resumeForced), reenrolMaxAttempts)
	}

	// A long-quiet CN gets a fresh burst (the cap must not be sticky forever).
	now = now.Add(reenrolResetAfter + time.Minute)
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != reenrolMaxAttempts+1 {
		t.Fatalf("post-quiet attempts = %d, want %d", len(rt.resumeForced), reenrolMaxAttempts+1)
	}

	// A SUCCESS resets the counter: the next lapse after cooldown heals again.
	rt.resumeForceErr = nil
	now = now.Add(reenrolCooldown + time.Minute)
	b.healLapse(context.Background(), "scratch") // succeeds, resets tries
	now = now.Add(reenrolCooldown + time.Minute)
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != reenrolMaxAttempts+3 {
		t.Fatalf("post-success attempts = %d, want %d", len(rt.resumeForced), reenrolMaxAttempts+3)
	}
}

// The healer takes a worker's lifecycle lock before it stages or bounces:
// while a start, resume, wake, stop or suspend of the worker holds it, the
// heal is skipped (audited; the next lapse event retries), so it cannot
// undo a stop that lands after its phase read, or double-resume with a wake.
func TestHealSkipsAWorkerWhoseLockIsHeld(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "running", ContainerStatus: "Up 2 minutes"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	b.reenrolLockWait = 20 * time.Millisecond
	unlock, err := b.lockWorker(context.Background(), "scratch")
	if err != nil {
		t.Fatal(err)
	}
	b.healLapse(context.Background(), "scratch")
	if len(rt.staged) != 0 || len(rt.suspend) != 0 || len(rt.resumed) != 0 {
		t.Fatalf("a heal ran under another holder's lock: staged=%d suspend=%v resume=%v", len(rt.staged), rt.suspend, rt.resumed)
	}
	if !strings.Contains(buf.String(), "busy") {
		t.Fatalf("no audit line for the skipped heal:\n%s", buf.String())
	}
	b.reenrolMu.Lock()
	tries := b.reenrolTries["scratch"]
	b.reenrolMu.Unlock()
	if tries != 0 {
		t.Fatalf("a skipped heal counted as a failed attempt: %d", tries)
	}
	// Released, the next lapse heals (after the cooldown).
	unlock()
	b.reenrolMu.Lock()
	delete(b.reenrolLast, "scratch")
	b.reenrolMu.Unlock()
	b.healLapse(context.Background(), "scratch")
	if len(rt.staged) != 1 || len(rt.resumed) != 1 {
		t.Fatalf("after release: staged=%d resume=%v", len(rt.staged), rt.resumed)
	}
	// And the heal holds the lock while it bounces: a verb waits for it.
	if unlock, err := b.lockWorker(context.Background(), "scratch"); err != nil {
		t.Fatal("the heal left the lock held")
	} else {
		unlock()
	}
}

// A `lever revoke` that lands while the heal waits for the worker's lock
// still wins: the heal checks revocation again once it holds the lock.
func TestHealRechecksRevocationAfterTheLockWait(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "running", ContainerStatus: "Up 2 minutes"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	b.reenrolLockWait = 5 * time.Second
	unlock, err := b.lockWorker(context.Background(), "scratch")
	if err != nil {
		t.Fatal(err)
	}
	// While the heal waits: revoke, then release the lock.
	b.onWorkerLockWait = func(string) {
		b.Revoke("scratch")
		unlock()
	}
	b.healLapse(context.Background(), "scratch")
	if len(rt.staged) != 0 || len(rt.suspend) != 0 || len(rt.resumed) != 0 {
		t.Fatalf("a heal ran for a worker revoked during its lock wait: staged=%d suspend=%v resume=%v", len(rt.staged), rt.suspend, rt.resumed)
	}
	if !strings.Contains(buf.String(), "revoked identity") {
		t.Fatalf("no deny line for the revoked heal:\n%s", buf.String())
	}
}

// gatedRuntime makes fakeRuntime safe to share between a heal and a request
// handler, and can hold Resume open: it signals resumeEntered, then waits
// for resumeRelease to close (mu is not held while it waits).
type gatedRuntime struct {
	mu            sync.Mutex
	f             *fakeRuntime
	resumeEntered chan string
	resumeRelease chan struct{}
}

func (g *gatedRuntime) List(ctx context.Context, p string) ([]scion.Agent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.List(ctx, p)
}
func (g *gatedRuntime) Start(ctx context.Context, o scion.StartOpts) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Start(ctx, o)
}
func (g *gatedRuntime) Resume(ctx context.Context, w, p string) error {
	g.resumeEntered <- w
	<-g.resumeRelease
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Resume(ctx, w, p)
}
func (g *gatedRuntime) ResumeForce(ctx context.Context, w, p string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.ResumeForce(ctx, w, p)
}
func (g *gatedRuntime) Stop(ctx context.Context, w, p string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Stop(ctx, w, p)
}
func (g *gatedRuntime) Suspend(ctx context.Context, w, p string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Suspend(ctx, w, p)
}
func (g *gatedRuntime) EnvSet(ctx context.Context, d, k, v string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.EnvSet(ctx, d, k, v)
}
func (g *gatedRuntime) Message(ctx context.Context, o scion.MsgOpts) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Message(ctx, o)
}
func (g *gatedRuntime) Inbox(ctx context.Context, u bool, p string) ([]scion.Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.Inbox(ctx, u, p)
}
func (g *gatedRuntime) StageWorkerTicket(ctx context.Context, w string, payload []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.StageWorkerTicket(ctx, w, payload)
}

func newGatedRuntime(phase string) *gatedRuntime {
	return &gatedRuntime{
		f: &fakeRuntime{agents: map[string][]scion.Agent{
			testInstanceProject: {{Slug: "scratch", Phase: phase}},
		}},
		resumeEntered: make(chan string, 4),
		resumeRelease: make(chan struct{}),
	}
}

func reenrolTriesOf(b *Broker, cn string) int {
	b.reenrolMu.Lock()
	defer b.reenrolMu.Unlock()
	return b.reenrolTries[cn]
}

// A heal and the manager's resume of the same worker run one after the
// other, never interleaved: the resume waits for the heal's lock, then
// reads the phase the heal left (running) and answers 200 with no second
// resume, so it cannot meet the hub mid-bounce and answer "try again".
// Run with -race.
func TestHealAndManualResumeAreSerialised(t *testing.T) {
	g := newGatedRuntime("suspended")
	b, _, _ := reenrolBroker(t, g, "all")
	b.liveAttempts, b.liveInterval = 5, time.Millisecond
	waiting := make(chan string, 1)
	b.onWorkerLockWait = func(name string) { waiting <- name }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.healLapse(context.Background(), "scratch")
	}()
	<-g.resumeEntered // the heal holds the lock, in its resume
	var rec *httptest.ResponseRecorder
	go func() {
		defer wg.Done()
		rec = callWorker(t, b, "/worker/resume", `{"worker":"scratch"}`, "test-manager")
	}()
	if got := <-waiting; got != "scratch" {
		t.Fatalf("lock wait for %q, want scratch", got)
	}
	close(g.resumeRelease)
	wg.Wait()

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phase":"running"`) {
		t.Fatalf("manual resume after the heal: %d %s, want 200 running", rec.Code, rec.Body.String())
	}
	if n := len(g.f.resumed); n != 1 {
		t.Fatalf("resume calls = %d, want 1 (the manual resume must see the heal's result)", n)
	}
	if n := reenrolTriesOf(b, "scratch"); n != 0 {
		t.Fatalf("heal attempts after a healed lapse = %d, want 0", n)
	}
}

// The other order: the manager's resume holds the lock, so the heal is
// skipped without using an attempt, and the resume's success clears the
// count that earlier failed heals left. Run with -race.
func TestManualResumeDuringHealResetsTheAttemptCount(t *testing.T) {
	g := newGatedRuntime("suspended")
	b, _, _ := reenrolBroker(t, g, "all")
	b.liveAttempts, b.liveInterval = 5, time.Millisecond
	b.reenrolLockWait = 20 * time.Millisecond
	b.reenrolMu.Lock()
	b.reenrolTries["scratch"] = reenrolMaxAttempts - 1 // two failed heals before
	b.reenrolMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	var rec *httptest.ResponseRecorder
	go func() {
		defer wg.Done()
		rec = callWorker(t, b, "/worker/resume", `{"worker":"scratch"}`, "test-manager")
	}()
	<-g.resumeEntered // the manual resume holds the lock
	b.healLapse(context.Background(), "scratch")
	if n := reenrolTriesOf(b, "scratch"); n != reenrolMaxAttempts-1 {
		t.Fatalf("a heal skipped for a busy lock changed the count to %d", n)
	}
	close(g.resumeRelease)
	wg.Wait()
	if rec.Code != http.StatusOK {
		t.Fatalf("manual resume: %d %s", rec.Code, rec.Body.String())
	}
	if n := reenrolTriesOf(b, "scratch"); n != 0 {
		t.Fatalf("heal attempts after a successful manual resume = %d, want 0", n)
	}
	if len(g.f.staged["scratch"]) != 1 || len(g.f.resumed) != 1 {
		t.Fatalf("staged=%d resumed=%d, want 1 and 1 (the skipped heal must do nothing)", len(g.f.staged["scratch"]), len(g.f.resumed))
	}
}

// A reset that lands while a heal waits for the lock: the heal's skip must
// not drive the count below zero (which would give the next burst an extra
// attempt).
func TestSkippedHealNeverLeavesANegativeCount(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "suspended"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.reenrolLockWait = 20 * time.Millisecond
	unlock, err := b.lockWorker(context.Background(), "scratch")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b.onWorkerLockWait = func(name string) { b.resetReenrolTries(name) }
	b.healLapse(context.Background(), "scratch")
	if n := reenrolTriesOf(b, "scratch"); n != 0 {
		t.Fatalf("count after a reset and a skipped heal = %d, want 0", n)
	}
}
