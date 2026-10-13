package apply

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// readyGateProbe: each AwaitClaudeReady call takes the next scripted answer,
// and the last answer repeats.
type readyGateProbe struct {
	*gateProbe
	ready   []readyAnswer
	onReady func(call int)
	mu      sync.Mutex
	readyN  int
	windows []time.Duration
}

type readyAnswer struct {
	st  jail.ClaudeReady
	err error
}

func (p *readyGateProbe) AwaitClaudeReady(_ context.Context, ref string, window time.Duration) (jail.ClaudeReady, error) {
	p.mu.Lock()
	p.readyN++
	n := p.readyN
	p.windows = append(p.windows, window)
	p.mu.Unlock()
	if p.onReady != nil {
		p.onReady(n)
	}
	a := p.ready[min(n, len(p.ready))-1]
	return a.st, a.err
}

func readyDeps(p *readyGateProbe, sink *logSink) Deps {
	d := liveGateDeps(p.gateProbe, time.Minute)
	d.AgentSession = p
	d.ReadyWait = time.Hour
	if sink != nil {
		d.Log = sink.logf
	}
	return d
}

var (
	ready    = readyAnswer{st: jail.ClaudeReadyYes}
	pending  = readyAnswer{st: jail.ClaudeReadyPending}
	noHook   = readyAnswer{st: jail.ClaudeReadyNoHook}
	paneGone = readyAnswer{st: jail.ClaudeReadyPaneGone}
)

// The gate ends as soon as claude has reported its session started: no
// settle window, no second look at the pane.
func TestReadyMarkerEndsTheGateWithNoWindow(t *testing.T) {
	shortHarnessSettle(t, time.Hour)
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{ready}}
	start := time.Now()
	if err := WaitManagerLive(context.Background(), readyDeps(p, nil), "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("the gate held %s", el)
	}
	if p.aliveN != 1 || p.readyN != 1 {
		t.Fatalf("pane probes = %d, ready waits = %d; want 1 and 1", p.aliveN, p.readyN)
	}
	if p.windows[0] != readyPollWindow {
		t.Fatalf("window = %s, want %s", p.windows[0], readyPollWindow)
	}
}

// Pending answers keep the gate waiting; it ends at the first ready.
func TestReadyMarkerWaitsWhilePending(t *testing.T) {
	shortHarnessSettle(t, time.Hour)
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending, pending, ready}}
	if err := WaitManagerLive(context.Background(), readyDeps(p, nil), "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if p.readyN != 3 {
		t.Fatalf("ready waits = %d, want 3", p.readyN)
	}
}

// A claude that never reports over a pane that stays alive is not failed
// when the ready wait is over: the settle window and its second look decide,
// and a pass is up with a warning that names the missing signal.
func TestReadyMarkerNeverWrittenFallsBackToTheWindow(t *testing.T) {
	settle := 60 * time.Millisecond
	shortHarnessSettle(t, settle)
	sink := &logSink{}
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	d := readyDeps(p, sink)
	d.ReadyWait = 20 * time.Millisecond
	start := time.Now()
	if err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up after the settle window, got %v", err)
	}
	if el := time.Since(start); el < d.ReadyWait+settle {
		t.Fatalf("the gate held %s, want at least the ready wait and the %s window", el, settle)
	}
	if p.aliveN != 2 {
		t.Fatalf("pane probes = %d, want 2 (seen, then confirmed after the window)", p.aliveN)
	}
	logged := strings.Join(sink.lines, "\n")
	for _, want := range []string{"warning: the manager's claude never reported its session start", "dialog", "/tmp", "hooks disabled", "no readiness signal", "settle window"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("warning does not say %q: %q", want, sink.lines)
		}
	}
}

// After a ready wait with no report, a pane that dies during the settle
// window is a death, and the error says claude never reported either.
func TestReadyMarkerNeverWrittenThenPaneDiesIsADeath(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	sink := &logSink{}
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}, {alive: false}}, running: []runningAnswer{{up: true}}},
		ready:     []readyAnswer{pending},
	}
	d := readyDeps(p, sink)
	d.ReadyWait = 20 * time.Millisecond
	err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "died during the") || !strings.Contains(err.Error(), "had not reported its session start") {
		t.Fatalf("want ErrAgentDied during the settle window, got %v", err)
	}
	if strings.Contains(strings.Join(sink.lines, "\n"), "never reported its session start") {
		t.Fatalf("a failed gate warned as if up: %q", sink.lines)
	}
}

// The same with the container gone during the settle window.
func TestReadyMarkerNeverWrittenThenContainerStopsIsADeath(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}, {err: jail.ErrNoContainer}}},
		ready:     []readyAnswer{pending},
	}
	d := readyDeps(p, nil)
	d.ReadyWait = 20 * time.Millisecond
	err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "container stopped during the") {
		t.Fatalf("want ErrAgentDied during the settle window, got %v", err)
	}
}

// A window that passes on the container alone (the pane cannot be read
// after it) is up, and the warning says what it saw.
func TestReadyMarkerNeverWrittenWindowPassesOnTheContainer(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	sink := &logSink{}
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}, {err: &jail.ProbeExitError{Code: 1}}}, running: []runningAnswer{{up: true}}},
		ready:     []readyAnswer{pending},
	}
	d := readyDeps(p, sink)
	d.ReadyWait = 20 * time.Millisecond
	if err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up, got %v", err)
	}
	logged := strings.Join(sink.lines, "\n")
	if !strings.Contains(logged, "never reported its session start") || !strings.Contains(logged, "could not be read but its container still ran") {
		t.Fatalf("warning does not say the window passed on the container: %q", sink.lines)
	}
}

// A ready wait whose last reading could not tell whether the pane is alive
// gets the handover's warning, not the one that claims a live pane.
func TestReadyMarkerTimeoutAfterAnUnknownReadingHandsOver(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	sink := &logSink{}
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{{err: jail.ErrProbeTimedOut}}}
	d := readyDeps(p, sink)
	d.ReadyWait = time.Nanosecond
	if err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil); err != nil {
		t.Fatalf("want up after the window, got %v", err)
	}
	if p.readyN != 1 {
		t.Fatalf("ready waits = %d, want 1", p.readyN)
	}
	logged := strings.Join(sink.lines, "\n")
	if !strings.Contains(logged, "could not wait for the manager's claude") || strings.Contains(logged, "never reported its session start") {
		t.Fatalf("want the handover's warning only: %q", sink.lines)
	}
}

func TestReadyMarkerNeverWrittenWindowEndsOnDeadline(t *testing.T) {
	shortHarnessSettle(t, time.Hour)
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	d := readyDeps(p, nil)
	d.ReadyWait = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := WaitManagerLive(ctx, d, "hello", "/lever", nil)
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != context.DeadlineExceeded.Error() {
		t.Fatalf("want context.DeadlineExceeded unwrapped, got %v", err)
	}
}

// An image without the hook falls back to the settle window, and says so.
func TestReadyMarkerNoHookHoldsTheWindow(t *testing.T) {
	settle := 60 * time.Millisecond
	shortHarnessSettle(t, settle)
	sink := &logSink{}
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{noHook}}
	start := time.Now()
	if err := WaitManagerLive(context.Background(), readyDeps(p, sink), "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if el := time.Since(start); el < settle {
		t.Fatalf("the gate held %s, want at least the %s window", el, settle)
	}
	if p.aliveN != 2 {
		t.Fatalf("pane probes = %d, want 2 (seen, then confirmed after the window)", p.aliveN)
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "no claude-ready hook") {
		t.Fatalf("no note about the missing hook: %q", sink.lines)
	}
}

// A wait that cannot run in the container hands over to the settle window
// after unknownHandover tries, with a warning.
func TestReadyMarkerBrokenWaitHandsOver(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	sink := &logSink{}
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}},
		ready:     []readyAnswer{{err: jail.ErrProbeTimedOut}},
	}
	if err := WaitManagerLive(context.Background(), readyDeps(p, sink), "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if p.readyN != unknownHandover {
		t.Fatalf("ready waits = %d, want %d", p.readyN, unknownHandover)
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "could not wait for the manager's claude") {
		t.Fatalf("no warning: %q", sink.lines)
	}
}

// Claude's pane dying while lever waits for the report is a death at once.
func TestReadyMarkerPaneDiesIsADeath(t *testing.T) {
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}, {alive: false}}, running: []runningAnswer{{up: true}}},
		ready:     []readyAnswer{pending, paneGone},
	}
	err := WaitManagerLive(context.Background(), readyDeps(p, nil), "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "died before claude reported its session started") {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
}

func TestReadyMarkerContainerStopsIsADeath(t *testing.T) {
	p := &readyGateProbe{
		gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}, {err: jail.ErrNoContainer}}},
		ready:     []readyAnswer{{err: jail.ErrNoContainer}},
	}
	err := WaitManagerLive(context.Background(), readyDeps(p, nil), "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "container stopped") {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
}

// A record that leaves running while lever waits for the report is a death.
func TestReadyMarkerRecordLeavesRunningIsADeath(t *testing.T) {
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	d := readyDeps(p, nil)
	d.ReadyWait = 10 * time.Second // a backstop: the death comes first
	r := &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", initPhase: "running", initContainerStatus: "Up 2 seconds", dieAfterLiveLists: 2}
	d.Scion = scion.New(r, scion.Options{})
	err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
}

// The attach gate does not wait for the report: the operator sees claude
// start, and the look after the detach catches a death.
func TestAttachGateDoesNotWaitForTheMarker(t *testing.T) {
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	if err := attachGate(context.Background(), readyDeps(p, nil), "hello", "/lever"); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if p.readyN != 0 {
		t.Fatalf("ready waits = %d, want 0", p.readyN)
	}
}

// start-manager's post-live probes run while the gate waits for the report.
func TestPostLiveProbesRunDuringTheReadyWait(t *testing.T) {
	app, f := newObserveFirstApp(t)
	read := make(chan struct{})
	var readBeforeReady bool
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{ready}}
	p.onReady = func(int) {
		select {
		case <-read:
			readBeforeReady = true
		case <-time.After(time.Second):
		}
	}
	sink := &logSink{}
	deps, _ := gateDeps(p.gateProbe)
	deps.AgentSession = p
	deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello", liveWhenContainer: "Up 2 seconds"}, scion.Options{})
	deps.Log = sink.logf
	deps.ReadClaudeSettings = func(context.Context, string) (jail.ClaudeDelivered, error) {
		close(read)
		return jail.ClaudeDelivered{AutoCompactWindow: 200000}, nil
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !readBeforeReady {
		t.Fatal("the claude-settings probe did not run during the ready wait")
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "runs with claude settings window 200000") {
		t.Fatalf("the probe's warning was not logged: %q", sink.lines)
	}
}

// An in-container wait that sees no pane while the pane probe sees one is
// no evidence either way: after unknownHandover rounds the window decides,
// and the warning names the disagreement.
func TestReadyMarkerDisagreeingReadingsHandOver(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	sink := &logSink{}
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{paneGone}}
	if err := WaitManagerLive(context.Background(), readyDeps(p, sink), "hello", "/lever", nil); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "(the pane readings disagree)") {
		t.Fatalf("warning does not name the disagreement: %q", sink.lines)
	}
}

// onHarness runs exactly once on every path past the pane.
func TestReadyGateRunsOnHarnessOnce(t *testing.T) {
	shortHarnessSettle(t, time.Millisecond)
	for name, answers := range map[string][]readyAnswer{
		"ready":    {ready},
		"no hook":  {noHook},
		"handover": {{err: jail.ErrProbeTimedOut}},
		"silent":   {pending},
	} {
		p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: answers}
		d := readyDeps(p, nil)
		d.ReadyWait = 20 * time.Millisecond
		n := 0
		err := gateManagerLive(context.Background(), d, d.ManagerLiveRetry, "hello", "/lever", "up", nil, gateOpts{onHarness: func() { n++ }})
		if err != nil || n != 1 {
			t.Fatalf("%s: err %v, onHarness ran %d times, want once", name, err, n)
		}
	}
}

func TestReadyMarkerWaitEndsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	p.onReady = func(call int) {
		if call == 3 {
			cancel()
		}
	}
	if err := WaitManagerLive(ctx, readyDeps(p, nil), "hello", "/lever", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
