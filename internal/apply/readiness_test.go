package apply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// shortHarnessSettle sets harnessSettle for one test.
func shortHarnessSettle(t *testing.T, d time.Duration) {
	t.Helper()
	was := harnessSettle
	harnessSettle = d
	t.Cleanup(func() { harnessSettle = was })
}

// liveGateDeps is a running record with a probe whose answers p scripts,
// and a long budget Settle: what a gate holds when it never sees the pane.
func liveGateDeps(p *gateProbe, settle time.Duration) Deps {
	r := &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", initPhase: "running", initContainerStatus: "Up 2 seconds"}
	return Deps{
		Scion:            scion.New(r, scion.Options{}),
		AgentSession:     p,
		ContainerRunning: p.ContainerRunning,
		HarnessWait:      time.Second,
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: settle},
		Log:              func(string, ...any) {},
	}
}

// TestSettleCountsFromClaudesPane (lever#188): once claude's pane is seen
// alive the window is the short harnessSettle, not the budget's Settle.
func TestSettleCountsFromClaudesPane(t *testing.T) {
	shortHarnessSettle(t, 10*time.Millisecond)
	p := &gateProbe{alive: []harnessAnswer{{alive: true}}}
	d := liveGateDeps(p, time.Minute)
	start := time.Now()
	if err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("the gate held %s: the budget's window, not the one after the pane", el)
	}
	if p.aliveN != 2 {
		t.Fatalf("pane probes = %d, want 2 (seen, then confirmed after the window)", p.aliveN)
	}
}

// TestNoPaneSignalHoldsTheLongWindow: a probe that cannot run hands over
// to the window, which is then the budget's Settle, counted from the
// record.
func TestNoPaneSignalHoldsTheLongWindow(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	p := &gateProbe{
		alive:   []harnessAnswer{{err: errors.New("exec timed out")}},
		running: []runningAnswer{{up: true}},
	}
	settle := 150 * time.Millisecond
	d := liveGateDeps(p, settle)
	start := time.Now()
	if err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if el := time.Since(start); el < settle {
		t.Fatalf("the gate held %s, want at least the budget's %s", el, settle)
	}
}

// TestAttachGateEndsAtThePane: for an up that attaches next the gate ends
// once the pane is alive, with no window and no second look.
func TestAttachGateEndsAtThePane(t *testing.T) {
	shortHarnessSettle(t, time.Minute)
	p := &gateProbe{alive: []harnessAnswer{{alive: true}}}
	d := liveGateDeps(p, time.Minute)
	start := time.Now()
	if err := WaitManagerStarted(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("the attach gate held a window (%s)", el)
	}
	if p.aliveN != 1 {
		t.Fatalf("pane probes = %d, want 1", p.aliveN)
	}
}

// TestAttachGateStillRefusesADeath: the attach gate is not a shortcut past
// a claude that never comes up.
func TestAttachGateStillRefusesADeath(t *testing.T) {
	p := &gateProbe{
		alive:   []harnessAnswer{{alive: false}},
		running: []runningAnswer{{up: false}},
	}
	err := WaitManagerStarted(context.Background(), liveGateDeps(p, time.Minute), "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
}

// TestPostLiveProbesRunDuringTheSettle (lever#188): start-manager's
// warnings run while the window is held: the look after the window waits
// (up to a second) for the claude-settings read to have happened.
func TestPostLiveProbesRunDuringTheSettle(t *testing.T) {
	shortHarnessSettle(t, 5*time.Millisecond)
	app, f := newObserveFirstApp(t)
	read := make(chan struct{})
	var readBeforeConfirm bool
	p := &gateProbe{alive: []harnessAnswer{{alive: true}}}
	p.onAlive = func(call int) {
		if call != 2 {
			return
		}
		select {
		case <-read:
			readBeforeConfirm = true
		case <-time.After(time.Second):
		}
	}
	sink := &logSink{}
	deps, _ := gateDeps(p)
	deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 2 seconds"}, scion.Options{})
	deps.Log = sink.logf
	deps.ReadClaudeSettings = func(context.Context, string) (jail.ClaudeDelivered, error) {
		close(read)
		return jail.ClaudeDelivered{AutoCompactWindow: 200000}, nil
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !readBeforeConfirm {
		t.Fatal("the claude-settings probe ran after the settle window, not during it")
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "runs with claude settings window 200000") {
		t.Fatalf("the probe's warning was not logged: %q", sink.lines)
	}
}

// TestPostLiveWarningsAreDroppedOnAFailedStart: a manager that dies in the
// window gets its death reported, not warnings about its settings.
func TestPostLiveWarningsAreDroppedOnAFailedStart(t *testing.T) {
	shortHarnessSettle(t, 5*time.Millisecond)
	app, f := newObserveFirstApp(t)
	p := &gateProbe{
		alive:   []harnessAnswer{{alive: true}, {alive: false}},
		running: []runningAnswer{{up: false}},
	}
	sink := &logSink{}
	deps, _ := gateDeps(p)
	deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 2 seconds"}, scion.Options{})
	deps.Log = sink.logf
	deps.ReadClaudeSettings = func(context.Context, string) (jail.ClaudeDelivered, error) {
		return jail.ClaudeDelivered{AutoCompactWindow: 200000}, nil
	}
	err := runApply(app, deps)
	if !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
	for _, l := range sink.lines {
		if strings.Contains(l, "claude settings") {
			t.Fatalf("a warning about a manager that did not come up: %q", l)
		}
	}
}

// TestPostLiveFinishRunsUnstartedProbes: a gate that never called the hook
// (one that fails nothing but starts nothing either) still gets the probes,
// in their old order, after it.
func TestPostLiveFinishRunsUnstartedProbes(t *testing.T) {
	sink := &logSink{}
	app := &config.App{Name: "assistant", Manager: config.Manager{ClaudeSettings: config.ClaudeSettings{AutoCompactWindow: 400000}}}
	r := &run{app: app, d: Deps{Log: sink.logf, ReadClaudeSettings: func(context.Context, string) (jail.ClaudeDelivered, error) {
		return jail.ClaudeDelivered{AutoCompactWindow: 200000}, nil
	}}}
	p := r.startPostLive(context.Background(), "/lever/proj")
	p.abandon() // never started: nothing runs
	if len(sink.lines) != 0 {
		t.Fatalf("abandon ran probes: %q", sink.lines)
	}
	p.finish()
	if len(sink.lines) != 1 || !strings.Contains(sink.lines[0], "window 200000") {
		t.Fatalf("finish: %q", sink.lines)
	}
}

// TestCheckManagerAfterDetach (lever#188): after an attach that skipped the
// window, a live pane is silence; a dead one, a stopped container or a
// record that left running is reported with what was seen; a probe and a
// list that both cannot answer say nothing.
func TestCheckManagerAfterDetach(t *testing.T) {
	alive := &gateProbe{alive: []harnessAnswer{{alive: true}}}
	if err := CheckManagerAfterDetach(context.Background(), liveGateDeps(alive, 0), "hello", "/lever"); err != nil {
		t.Fatalf("live: %v", err)
	}
	for name, p := range map[string]*gateProbe{
		"its container stopped": {alive: []harnessAnswer{{alive: false}}, running: []runningAnswer{{up: false}}},
		"pane":                  {alive: []harnessAnswer{{alive: false}}, running: []runningAnswer{{up: true}}},
		"no tmux server":        {alive: []harnessAnswer{{err: jail.ErrNoTmuxServer}}, running: []runningAnswer{{up: true}}},
	} {
		err := CheckManagerAfterDetach(context.Background(), liveGateDeps(p, 0), "hello", "/lever")
		if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "after the attach ended") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// No probe: the record decides.
	dead := &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", initPhase: "error", initContainerStatus: "Exited (1) 2 seconds ago"}
	d := Deps{Scion: scion.New(dead, scion.Options{}), ManagerLiveRetry: RetryBudget{Attempts: 1, Interval: time.Millisecond}}
	if err := CheckManagerAfterDetach(context.Background(), d, "hello", "/lever"); !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("error record: %v", err)
	}
	listFails := &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", listErr: errors.New("hub down")}
	d = Deps{Scion: scion.New(listFails, scion.Options{}), ManagerLiveRetry: RetryBudget{Attempts: 1, Interval: time.Millisecond}}
	if err := CheckManagerAfterDetach(context.Background(), d, "hello", "/lever"); err != nil {
		t.Fatalf("no evidence must say nothing: %v", err)
	}
}

// TestApplyAttachEndsStartManagerAtThePane: PlanOpts.Attach (an up that
// attaches next) ends start-manager's gate once the pane is alive.
func TestApplyAttachEndsStartManagerAtThePane(t *testing.T) {
	shortHarnessSettle(t, time.Minute)
	app, f := newObserveFirstApp(t)
	p := &gateProbe{alive: []harnessAnswer{{alive: true}}}
	deps, _ := gateDeps(p)
	deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 2 seconds"}, scion.Options{})
	deps.ManagerLiveRetry.Settle = time.Minute
	if err := Run(context.Background(), app, fillDeps(deps), PlanOpts{Attach: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.aliveN != 1 {
		t.Fatalf("pane probes = %d, want 1 (no window, no second look)", p.aliveN)
	}
}
