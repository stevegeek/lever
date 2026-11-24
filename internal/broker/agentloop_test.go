package broker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// loopEnv is a spawn env with two agents, a and b, whose hub records the
// test sets, and a clock it moves.
type loopEnv struct {
	*spawnEnv
	a, b string
	now  time.Time
}

func newLoopEnv(t *testing.T, idle time.Duration) *loopEnv {
	t.Helper()
	e := &loopEnv{spawnEnv: newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.IdleSuspend = idle; t.MaxRunning = 3 })}
	e.a = e.spawn(t, `{"template":"session"}`).Name
	e.b = e.spawn(t, `{"template":"session"}`).Name
	e.now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	e.b0().clock = func() time.Time { return e.now }
	e.rt.mu.Lock()
	e.rt.staticPhases = true
	e.rt.mu.Unlock()
	return e
}

func (e *loopEnv) b0() *Broker { return e.spawnEnv.b }

// records sets the hub's records: a waiting at its prompt since lae, b
// working.
func (e *loopEnv) records(aActivity string, lae time.Time) {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	e.rt.agents[e.b0().instanceProject] = []scion.Agent{
		{Slug: e.a, Phase: "running", Activity: aActivity, ContainerStatus: "Up 5 minutes", LastActivityEvent: lae},
		{Slug: e.b, Phase: "running", Activity: scion.ActivityWorking, ContainerStatus: "Up 5 minutes"},
	}
}

func (e *loopEnv) waiting(t *testing.T, name string) *time.Time {
	t.Helper()
	for _, a := range e.b0().agentList(context.Background(), false, "", false).Agents {
		if a.Name == name {
			return a.WaitingSince
		}
	}
	t.Fatalf("%s is not listed", name)
	return nil
}

// The loop notes when an agent began waiting (from the hub's last-activity
// time), lists it, and suspends the agent only past idle_suspend; a working
// agent is never touched.
func TestAgentsLoopWaitingAndIdleSuspend(t *testing.T) {
	e := newLoopEnv(t, 10*time.Minute)
	lae := e.now.Add(-3 * time.Minute)
	e.records(scion.ActivityWaitingForInput, lae)
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w == nil || !w.Equal(lae) {
		t.Fatalf("a waiting since %v", w)
	}
	if w := e.waiting(t, e.b); w != nil {
		t.Fatalf("b (working) waiting since %v", w)
	}
	if len(e.rt.suspend) != 0 {
		t.Fatalf("suspended before idle_suspend: %v", e.rt.suspend)
	}
	e.now = e.now.Add(8 * time.Minute) // 11 minutes waiting
	e.b0().agentsPass(context.Background())
	if len(e.rt.suspend) != 1 || e.rt.suspend[0] != e.a {
		t.Fatalf("suspended %v", e.rt.suspend)
	}
	if w := e.waiting(t, e.a); w != nil {
		t.Fatalf("still waiting after the suspend: %v", w)
	}
}

// Working again clears the wait, so the next wait counts from its own start.
func TestAgentsLoopWaitEndsWhenWorking(t *testing.T) {
	e := newLoopEnv(t, 10*time.Minute)
	e.records(scion.ActivityWaitingForInput, time.Time{})
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w == nil || !w.Equal(e.now) {
		t.Fatalf("with no hub time, waiting since %v (want this pass)", w)
	}
	e.now = e.now.Add(9 * time.Minute)
	e.records(scion.ActivityWorking, time.Time{})
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w != nil {
		t.Fatalf("working, still waiting since %v", w)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.records(scion.ActivityWaitingForInput, time.Time{})
	e.b0().agentsPass(context.Background())
	if len(e.rt.suspend) != 0 {
		t.Fatalf("suspended on a wait of 0 minutes: %v", e.rt.suspend)
	}
}

// With no idle_suspend nothing is suspended, an unknown activity (stalled)
// is no wait, and a hub time from the far past or the future is not
// believed.
func TestAgentsLoopOffAndUnknown(t *testing.T) {
	e := newLoopEnv(t, 0)
	e.records(scion.ActivityWaitingForInput, e.now.Add(-30*24*time.Hour))
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w == nil || !w.Equal(e.now) {
		t.Fatalf("a month-old hub time believed: %v", w)
	}
	e.now = e.now.Add(48 * time.Hour)
	e.b0().agentsPass(context.Background())
	if len(e.rt.suspend) != 0 {
		t.Fatalf("idle_suspend off, suspended %v", e.rt.suspend)
	}
	e.records("stalled", time.Time{})
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w != nil {
		t.Fatalf("stalled read as waiting: %v", w)
	}
	e.records(scion.ActivityWaitingForInput, e.now.Add(time.Hour))
	e.b0().agentsPass(context.Background())
	if w := e.waiting(t, e.a); w == nil || !w.Equal(e.now) {
		t.Fatalf("a future hub time believed: %v", w)
	}
}

// A busy lock (a verb under way) defers the suspend; a fresh read that no
// longer shows the wait cancels it.
func TestIdleSuspendRechecksUnderTheLock(t *testing.T) {
	e := newLoopEnv(t, 10*time.Minute)
	e.records(scion.ActivityWaitingForInput, e.now.Add(-time.Hour))
	unlock, ok := e.b0().tryLockWorker(e.a)
	if !ok {
		t.Fatal("lock")
	}
	e.b0().agentsPass(context.Background())
	unlock()
	if len(e.rt.suspend) != 0 {
		t.Fatalf("suspended under another verb's lock: %v", e.rt.suspend)
	}
	// The pass saw it waiting; by the time the lock is taken it works.
	e.rt.mu.Lock()
	e.rt.listFirst = [][]scion.Agent{{{Slug: e.a, Phase: "running", Activity: scion.ActivityWorking, ContainerStatus: "Up"}}}
	e.rt.mu.Unlock()
	e.b0().idleSuspend(context.Background(), e.a)
	if len(e.rt.suspend) != 0 {
		t.Fatalf("suspended a working agent: %v", e.rt.suspend)
	}
}

// A destroyed agent is forgotten and never suspended or backed up.
func TestAgentsLoopForgetsDestroyedAgents(t *testing.T) {
	e := newLoopEnv(t, 10*time.Minute)
	var mu sync.Mutex
	var backed []string
	e.b0().dailyBackup = func(_ context.Context, agent string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		backed = append(backed, agent)
		return "20261124T120000Z", nil
	}
	e.records(scion.ActivityWaitingForInput, e.now.Add(-time.Minute))
	e.b0().agentsPass(context.Background())
	mu.Lock()
	backed = nil // the first pass backs both up
	mu.Unlock()
	if _, err := e.b0().workers.update(e.a, func(r *AgentRecord) { r.State = AgentDestroyed }); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(2 * time.Hour)
	e.b0().agentsPass(context.Background())
	if len(e.rt.suspend) != 0 {
		t.Fatalf("a destroyed agent was suspended: %v", e.rt.suspend)
	}
	if e.b0().agentWatch.waiting(e.a) != nil {
		t.Fatal("a destroyed agent is still waiting")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range backed {
		if n == e.a {
			t.Fatal("a destroyed agent was backed up")
		}
	}
}

// The daily backup is offered once an hour, only for agents seen running
// since their last backup; one taken clears the mark, one not due keeps it,
// and an error is audited, not retried at once.
func TestAgentsLoopDailyBackup(t *testing.T) {
	e := newLoopEnv(t, 0)
	var calls []string
	due := map[string]bool{}
	var fail error
	e.b0().dailyBackup = func(_ context.Context, agent string) (string, error) {
		calls = append(calls, agent)
		if fail != nil {
			return "", fail
		}
		if due[agent] {
			return "20261124T120000Z", nil
		}
		return "", nil
	}
	e.records(scion.ActivityWorking, time.Time{})
	e.b0().agentsPass(context.Background())
	if len(calls) != 2 {
		t.Fatalf("first pass: %v", calls)
	}
	// Within the hour: no call.
	calls = nil
	e.now = e.now.Add(10 * time.Minute)
	e.b0().agentsPass(context.Background())
	if len(calls) != 0 {
		t.Fatalf("within the hour: %v", calls)
	}
	// a's backup is due and taken; b is not due. Then neither runs: a is
	// not offered again, b is (it ran and still has no new backup).
	due[e.a] = true
	e.now = e.now.Add(time.Hour)
	e.b0().agentsPass(context.Background())
	e.rt.mu.Lock()
	e.rt.agents[e.b0().instanceProject] = []scion.Agent{
		{Slug: e.a, Phase: "suspended"}, {Slug: e.b, Phase: "suspended"},
	}
	e.rt.mu.Unlock()
	calls = nil
	e.now = e.now.Add(time.Hour)
	e.b0().agentsPass(context.Background())
	if len(calls) != 1 || calls[0] != e.b {
		t.Fatalf("after a's backup: %v", calls)
	}
	// A failed backup keeps the mark: b is offered again next hour.
	fail = errors.New("capture failed")
	calls = nil
	e.now = e.now.Add(time.Hour)
	e.b0().agentsPass(context.Background())
	fail = nil
	e.now = e.now.Add(time.Hour)
	e.b0().agentsPass(context.Background())
	if len(calls) != 2 || calls[0] != e.b || calls[1] != e.b {
		t.Fatalf("after a failed backup: %v", calls)
	}
}

// An archive refuses a workspace nested past archiveMaxDepth, rather than
// holding a directory open per level without end.
func TestArchiveRefusesDeepNesting(t *testing.T) {
	root := t.TempDir()
	p := root
	for range archiveMaxDepth + 2 {
		p = filepath.Join(p, "d")
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Skipf("cannot nest that deep here: %v", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, err := archiveDir(context.Background(), r, io.Discard); !errors.Is(err, errArchiveLimit) {
		t.Fatalf("deep nesting: %v", err)
	}
}
