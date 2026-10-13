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

// readyGateProbe is a gateProbe that also waits for claude's session start
// (jail.AgentProbe.AwaitClaudeReady): each call takes the next scripted
// answer, and the last answer repeats.
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

// A claude that never reports is not up once the ready wait is over.
func TestReadyMarkerNeverWrittenIsNotUp(t *testing.T) {
	p := &readyGateProbe{gateProbe: &gateProbe{alive: []harnessAnswer{{alive: true}}}, ready: []readyAnswer{pending}}
	d := readyDeps(p, nil)
	d.ReadyWait = 20 * time.Millisecond
	err := WaitManagerLive(context.Background(), d, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentNotLive) || !strings.Contains(err.Error(), "did not report its session started") {
		t.Fatalf("want ErrAgentNotLive naming the missing report, got %v", err)
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
	if err := WaitManagerStarted(context.Background(), readyDeps(p, nil), "hello", "/lever", nil); err != nil {
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
