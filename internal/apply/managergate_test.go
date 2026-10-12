package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// gateProbe scripts the in-container half of the manager gate: each
// HarnessAlive / ContainerRunning call takes the next scripted answer, and
// the last answer repeats.
type gateProbe struct {
	alive    []harnessAnswer
	running  []runningAnswer
	onAlive  func(call int) // runs before HarnessAlive answers
	aliveN   int
	runningN int
	refs     []string
}

type harnessAnswer struct {
	alive bool
	err   error
}

type runningAnswer struct {
	up  bool
	err error
}

func (p *gateProbe) HubToken(context.Context, string) (jail.HubTokenTimes, error) {
	return validToken(), nil
}

func (p *gateProbe) ReportSessionRunning(context.Context, string) error { return nil }

func (p *gateProbe) HarnessAlive(_ context.Context, ref string) (bool, error) {
	p.refs = append(p.refs, ref)
	p.aliveN++
	if p.onAlive != nil {
		p.onAlive(p.aliveN)
	}
	a := p.alive[min(p.aliveN, len(p.alive))-1]
	return a.alive, a.err
}

func (p *gateProbe) ContainerRunning(_ context.Context, ref string) (bool, error) {
	p.refs = append(p.refs, ref)
	p.runningN++
	if len(p.running) == 0 {
		return false, errors.New("no running answer scripted")
	}
	a := p.running[min(p.runningN, len(p.running))-1]
	return a.up, a.err
}

// gateDeps is a create-path start-manager whose hub record goes running/Up
// at the start and STAYS so — the hub's view in the lever#164 report, where
// the heartbeat-refreshed column read "Up" over a container that had exited.
func gateDeps(p *gateProbe) (Deps, *agentLifecycleRunner) {
	r := &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", liveWhenContainer: "Up 2 seconds"}
	return Deps{
		Scion:            scion.New(r, scion.Options{}),
		AgentSession:     p,
		ContainerRunning: p.ContainerRunning,
		HarnessWait:      30 * time.Millisecond,
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 5 * time.Millisecond},
	}, r
}

// lever#164: the container is Up briefly — the hub record reads running/Up
// for the whole settle window — and exits while the pre-start hook runs,
// before claude's pane ever exists. The gate must not report up, and must
// name it a death (the container log is worth reading).
func TestStartManagerContainerUpBrieflyThenExitedIsNotUp(t *testing.T) {
	app, f := newObserveFirstApp(t)
	p := &gateProbe{
		alive:   []harnessAnswer{{alive: false}}, // tmux: can't find session
		running: []runningAnswer{{up: true}, {up: true}, {up: false}},
	}
	deps, _ := gateDeps(p)
	deps.JailMount = "/lever"
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 2 seconds"}
	deps.Scion = scion.New(r, scion.Options{})
	err := runApply(app, deps)
	if !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
	for _, want := range []string{"start-manager", "container stopped", "pre-start hook"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if p.runningN != 3 {
		t.Fatalf("ContainerRunning calls = %d, want 3 (it fails at the first stopped reading)", p.runningN)
	}
	if want := jail.ContainerName("lever", "hello"); len(p.refs) == 0 || p.refs[0] != want {
		t.Fatalf("probe refs = %q, want %q", p.refs, want)
	}
}

// A container the runtime reports gone (podman: no such container) is the
// same death, decided by the pane probe alone.
func TestManagerGateContainerGoneIsADeath(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{err: jail.ErrNoContainer}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "up: manager") {
		t.Fatalf("want an up-prefixed ErrAgentDied, got %v", err)
	}
}

// Phase running over a live container, but claude's tmux pane is dead (or
// its window gone) with tmux answering: claude exited before it came up
// (a bad --continue, an auth failure). scion's shell window keeps tmux and
// the container up, so only the pane says so — and it is a death within
// notAliveDeath probes, not a "did not come up" after the whole wait.
func TestManagerGateClaudeDeadFromTheFirstProbeIsADeath(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{alive: false}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Hour
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || errors.Is(err, scion.ErrAgentNotLive) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
	if !strings.Contains(err.Error(), "claude exited before it came up") {
		t.Fatalf("error %q does not say claude exited", err)
	}
	if p.aliveN != notAliveDeath {
		t.Fatalf("HarnessAlive calls = %d, want %d", p.aliveN, notAliveDeath)
	}
}

// One not-alive answer between booting readings is not a death: the run
// must be consecutive.
func TestManagerGateNotAliveRunMustBeConsecutive(t *testing.T) {
	noServer := harnessAnswer{err: jail.ErrNoTmuxServer}
	dead := harnessAnswer{alive: false}
	p := &gateProbe{alive: []harnessAnswer{dead, dead, noServer, dead, dead, noServer, {alive: true}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Hour
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up, got %v", err)
	}
}

// The hub record leaving running while the gate waits for the pane (scion
// marks it error) is a death at once, not a wait to the end of the budget.
func TestManagerGateRecordLeavesRunningDuringHarnessWaitIsADeath(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{err: jail.ErrNoTmuxServer}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Hour
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	p.onAlive = func(call int) {
		if call == 2 {
			r.phase, r.containerStatus = "error", "Exited (1) 1 second ago"
		}
	}
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "left running") || !strings.Contains(err.Error(), `phase "error"`) {
		t.Fatalf("want ErrAgentDied naming the record leaving running, got %v", err)
	}
	if p.aliveN != 2 {
		t.Fatalf("HarnessAlive calls = %d, want 2 (it fails on the first listing after the flip)", p.aliveN)
	}
}

// While the gate waits for the pane it re-reads the hub record, so a
// progress line shows the current reading, not the one from before the wait.
func TestHarnessWaitProgressShowsFreshHubReadings(t *testing.T) {
	ft := newFakeTicks()
	sink := newLineSink()
	var line string
	p := &gateProbe{alive: []harnessAnswer{{err: jail.ErrNoTmuxServer}, {err: jail.ErrNoTmuxServer}, {alive: true}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Hour
	deps.Log, deps.clock, deps.progressTicker = sink.logf, ft.now, ft.ticker
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	p.onAlive = func(call int) {
		switch call {
		case 1:
			r.containerStatus = "Up 42 seconds"
		case 2:
			ft.tick()
			for line = <-sink.ch; !strings.Contains(line, "still starting"); line = <-sink.ch {
			}
		}
	}
	prog := StartProgress(deps, "up", "hello", "waiting")
	defer prog.Stop()
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", prog); err != nil {
		t.Fatalf("want up, got %v", err)
	}
	if !strings.Contains(line, `container "Up 42 seconds"`) {
		t.Fatalf("progress line %q does not show the fresh hub reading", line)
	}
}

// A pane that comes alive and dies inside the settle window is a death: the
// window starts once the harness runs and is checked again at its end.
func TestManagerGatePaneDiesDuringSettleIsADeath(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{alive: true}, {alive: false}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "settle window") {
		t.Fatalf("want ErrAgentDied during the settle window, got %v", err)
	}
}

// The healthy path: the pane is alive (after a boot that took a few polls)
// and still alive after the window.
func TestManagerGatePaneAliveIsUp(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{alive: false}, {alive: false}, {alive: true}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up, got %v", err)
	}
	if p.aliveN != 4 {
		t.Fatalf("HarnessAlive calls = %d, want 4 (three to come alive, one after the window)", p.aliveN)
	}
}

// A pane probe that cannot answer falls back to podman's own container
// state: still running after the settle window is up; when that cannot be
// read either, nothing was verified.
func TestManagerGateUnprobeablePaneFallsBackToTheContainer(t *testing.T) {
	probeErr := &jail.ProbeExitError{Code: 1}
	p := &gateProbe{alive: []harnessAnswer{{err: probeErr}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	var sink logSink
	deps.Log = sink.logf
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("container still running after the window: want up, got %v", err)
	}
	if !logged(&sink, "could not probe the manager's harness") {
		t.Fatalf("the fallback must be said: %q", sink.lines)
	}

	p = &gateProbe{alive: []harnessAnswer{{err: probeErr}}, running: []runningAnswer{{err: errors.New("podman hung")}}}
	deps, r = gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); !errors.Is(err, scion.ErrAgentUnverified) {
		t.Fatalf("nothing readable: want ErrAgentUnverified, got %v", err)
	}
}

// A path that acted on nothing (settle 0) keeps its single hub look: the
// in-container check belongs to the full gate.
func TestManagerGateZeroSettleDoesNotProbeTheHarness(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{alive: false}}, running: []runningAnswer{{up: false}}}
	deps, r := gateDeps(p)
	deps.ManagerLiveRetry.Settle = 0
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("zero settle: %v", err)
	}
	if p.aliveN != 0 {
		t.Fatalf("HarnessAlive calls = %d, want 0", p.aliveN)
	}
}

// fakeTicks is an injected ticker and clock: each tick the test sends
// advances the clock by the interval it was created with.
type fakeTicks struct {
	ch       chan time.Time
	interval atomic.Int64
	ticks    atomic.Int64
	start    time.Time
}

func newFakeTicks() *fakeTicks {
	return &fakeTicks{ch: make(chan time.Time), start: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
}

func (f *fakeTicks) ticker(d time.Duration) (<-chan time.Time, func()) {
	f.interval.Store(int64(d))
	return f.ch, func() {}
}

func (f *fakeTicks) now() time.Time {
	return f.start.Add(time.Duration(f.ticks.Load() * f.interval.Load()))
}

func (f *fakeTicks) tick() {
	f.ticks.Add(1)
	f.ch <- f.now()
}

// lineSink collects log lines from any goroutine.
type lineSink struct {
	mu    sync.Mutex
	lines []string
	ch    chan string
}

func newLineSink() *lineSink { return &lineSink{ch: make(chan string, 256)} }

func (s *lineSink) logf(format string, args ...any) {
	line := strings.TrimSpace(fmt.Sprintf(format, args...))
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	s.ch <- line
}

// lever#168: a progress line at each 15 s tick, naming the stage and the
// elapsed time, and none after Stop. No real sleeps: the ticker and the clock
// are injected.
func TestProgressPrintsAtTheIntervalWithStageAndElapsed(t *testing.T) {
	ft := newFakeTicks()
	sink := newLineSink()
	d := Deps{Log: sink.logf, clock: ft.now, progressTicker: ft.ticker}
	p := StartProgress(d, "up", "hello", "asking scion to resume the container")
	if got := time.Duration(ft.interval.Load()); got != 15*time.Second {
		t.Fatalf("ticker interval = %s, want 15s", got)
	}
	ft.tick()
	if l := <-sink.ch; !strings.Contains(l, `up: manager "hello" still starting — asking scion to resume the container`) ||
		!strings.Contains(l, "no hub reading") || !strings.HasSuffix(l, "15s elapsed") {
		t.Fatalf("first line = %q", l)
	}
	p.Stage("waiting for claude's tmux pane")
	p.observe("running", "Up 20 seconds")
	ft.tick()
	if l := <-sink.ch; !strings.Contains(l, "waiting for claude's tmux pane") ||
		!strings.Contains(l, `hub phase "running", container "Up 20 seconds"`) || !strings.HasSuffix(l, "30s elapsed") {
		t.Fatalf("second line = %q", l)
	}
	// Agent-set hub text is never echoed raw.
	p.observe("running; rm -rf", "Up and away")
	ft.tick()
	if l := <-sink.ch; strings.Contains(l, "rm -rf") || strings.Contains(l, "away") {
		t.Fatalf("hub text echoed raw: %q", l)
	}
	p.Stop()
	select {
	case ft.ch <- time.Now():
		t.Fatal("a tick was received after Stop")
	default:
	}
	p.Stop() // idempotent
	var nilP *Progress
	nilP.Stage("x")
	nilP.Stop()
}

// The progress line runs through the real start-manager gate: while the gate
// waits for claude's pane, a tick prints that stage with the hub's reading.
func TestStartManagerPrintsProgressWhileWaitingForTheHarness(t *testing.T) {
	app, f := newObserveFirstApp(t)
	ft := newFakeTicks()
	sink := newLineSink()
	var line string
	p := &gateProbe{alive: []harnessAnswer{{alive: false}, {alive: true}}, running: []runningAnswer{{up: true}}}
	p.onAlive = func(call int) {
		if call == 1 {
			ft.tick()
			for line = <-sink.ch; !strings.Contains(line, "still starting"); line = <-sink.ch {
			}
		}
	}
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 3 seconds"}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		AgentSession:     p,
		ContainerRunning: p.ContainerRunning,
		HarnessWait:      time.Second,
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 5 * time.Millisecond},
		Log:              sink.logf,
		clock:            ft.now,
		progressTicker:   ft.ticker,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{`start-manager: manager "hello" still starting`, "waiting for claude's tmux pane", `hub phase "running", container "Up 3 seconds"`, "15s elapsed"} {
		if !strings.Contains(line, want) {
			t.Fatalf("progress line %q lacks %q", line, want)
		}
	}
}

// Before claude starts, scion has no tmux server in the container yet: the
// probe answers "no tmux server" (an error, not a false). That is a boot in
// progress, so the gate keeps waiting and passes once the pane is alive.
func TestManagerGateNoTmuxServerYetThenAliveIsUp(t *testing.T) {
	noServer := harnessAnswer{err: jail.ErrNoTmuxServer}
	p := &gateProbe{alive: []harnessAnswer{noServer, noServer, noServer, noServer, noServer, noServer, {alive: true}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Second
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up after the boot, got %v", err)
	}
	if p.aliveN != 8 {
		t.Fatalf("HarnessAlive calls = %d, want 8 (seven to come alive, one after the window)", p.aliveN)
	}
}

// A pre-start hook that hangs keeps the container up with no tmux server:
// at the end of the harness wait that is not up, never the container
// fallback.
func TestManagerGateHungHookIsNotUp(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{err: jail.ErrNoTmuxServer}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentNotLive) || !strings.Contains(err.Error(), "no tmux server") {
		t.Fatalf("want ErrAgentNotLive naming the tmux server, got %v", err)
	}
}

// A probe that cannot run at all hands over to the container check after a
// few readings, not after the whole harness wait.
func TestManagerGateBrokenProbeHandsOverEarly(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{err: jail.ErrProbeTimedOut}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	deps.HarnessWait = time.Hour
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); err != nil {
		t.Fatalf("container running: want up via the fallback, got %v", err)
	}
	if p.aliveN != unknownHandover+1 {
		t.Fatalf("HarnessAlive calls = %d, want %d (handover, then one after the window)", p.aliveN, unknownHandover+1)
	}
}

// The tmux server going away after the pane was alive is a death.
func TestManagerGateTmuxServerGoneDuringSettleIsADeath(t *testing.T) {
	p := &gateProbe{alive: []harnessAnswer{{alive: true}, {err: jail.ErrNoTmuxServer}}, running: []runningAnswer{{up: true}}}
	deps, r := gateDeps(p)
	r.initPhase, r.initContainerStatus = "running", "Up 1 second"
	if err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil); !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
}
