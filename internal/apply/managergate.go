package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// defaultHarnessWait bounds how long the manager gate waits, after the hub
// record first reads live, for claude's tmux pane to come alive. The pane
// starts only after scion's pre-start hook (lever-agent boot: enrolment,
// settings, sidecars) has finished, so this covers the whole boot on a cold
// VM. A container that stops in the meantime fails the wait at once; only a
// container that stays up with no harness waits this long.
const defaultHarnessWait = 2 * time.Minute

// progressEvery is how often a manager start that is still in progress
// prints a line (lever#168): a container start, the pre-start hook, claude's
// boot and the settle window can together run over a minute, and a silent
// minute cannot be told from a hang.
const progressEvery = 15 * time.Second

// Progress prints a line about every progressEvery while a manager start
// runs: what lever is waiting for (the stage), the last hub reading of the
// record, and the time since the start. A nil *Progress is valid and prints
// nothing, so the gate can run without one.
type Progress struct {
	log           func(string, ...any)
	subject, name string
	now           func() time.Time
	start         time.Time

	mu               sync.Mutex
	stage            string
	phase, container string
	seen             bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// StartProgress starts the progress line for the manager name; subject
// prefixes it ("start-manager", "up"). The caller must call Stop. A Deps
// without Log yields a nil *Progress.
func StartProgress(d Deps, subject, name, stage string) *Progress {
	if d.Log == nil {
		return nil
	}
	now := d.clock
	if now == nil {
		now = time.Now
	}
	tick := d.progressTicker
	if tick == nil {
		tick = realTicker
	}
	p := &Progress{log: d.Log, subject: subject, name: name, now: now, start: now(), stage: stage,
		stop: make(chan struct{}), done: make(chan struct{})}
	ch, stopTick := tick(progressEvery)
	go func() {
		defer close(p.done)
		defer stopTick()
		for {
			select {
			case <-p.stop:
				return
			case <-ch:
				p.log("%s", p.line())
			}
		}
	}()
	return p
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Stage names what the start is waiting for now.
func (p *Progress) Stage(stage string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stage = stage
	p.mu.Unlock()
}

// observe records the last hub reading of the record.
func (p *Progress) observe(phase, container string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.phase, p.container, p.seen = phase, container, true
	p.mu.Unlock()
}

// Stop ends the progress line and waits for its goroutine: no line is
// printed after Stop returns.
func (p *Progress) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}

// line is one progress line. Phase and container are hub text an agent can
// set about itself, so they pass through the display labels.
func (p *Progress) line() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	reading := "no hub reading of the record yet"
	if p.seen {
		reading = fmt.Sprintf("hub phase %q, container %q", scion.PhaseLabel(p.phase), scion.ContainerLabel(p.container))
	}
	return fmt.Sprintf("%s: manager %q still starting — %s (%s); %s elapsed",
		p.subject, p.name, p.stage, reading, p.now().Sub(p.start).Round(time.Second))
}

// harnessState is one in-container reading of the manager's harness.
type harnessState int

const (
	harnessUnknown       harnessState = iota // the pane could not be probed
	harnessAlive                             // claude's tmux pane is alive
	harnessNotAlive                          // the container runs, the pane is not alive (not yet, or no more)
	harnessContainerDown                     // the container is gone or not running
)

// harnessReading is a harnessState with what backs it.
type harnessReading struct {
	state harnessState
	// containerUp: podman itself (not the hub's heartbeat column) said the
	// container runs. Only meaningful for harnessUnknown.
	containerUp bool
	err         error
}

// harnessCheck is the in-container half of the manager gate. The hub record
// is not enough (lever#164): scion marks it running when the container
// launches, its container column is refreshed by a heartbeat, and the
// pre-start hook runs after both — so a hook that fails ten seconds in left
// the record reading running over an "Up" container for the whole settle
// window, and `up --fresh` printed "is up." over a container that exited a
// moment later. The tmux pane exists only once the hook has passed and
// claude has started.
type harnessCheck struct {
	probe   AgentSessionProbe
	running func(ctx context.Context, ref string) (bool, error)
	ref     string
}

// harnessCheckFor is the check for the manager, or ok=false when Deps has
// no in-container probe (the gate then reads the hub record alone).
func harnessCheckFor(d Deps, name, project string) (harnessCheck, bool) {
	if d.AgentSession == nil {
		return harnessCheck{}, false
	}
	return harnessCheck{probe: d.AgentSession, running: d.ContainerRunning, ref: jail.ContainerName(path.Base(project), name)}, true
}

func (h harnessCheck) read(ctx context.Context) harnessReading {
	alive, err := h.probe.HarnessAlive(ctx, h.ref)
	if err == nil && alive {
		return harnessReading{state: harnessAlive}
	}
	if errors.Is(err, jail.ErrNoContainer) {
		return harnessReading{state: harnessContainerDown, err: err}
	}
	up, rerr := false, errors.New("no container state check")
	if h.running != nil {
		up, rerr = h.running(ctx, h.ref)
	}
	if rerr == nil && !up {
		return harnessReading{state: harnessContainerDown}
	}
	if err == nil {
		return harnessReading{state: harnessNotAlive}
	}
	return harnessReading{state: harnessUnknown, containerUp: rerr == nil && up, err: err}
}

// waitHarness polls the container until claude's tmux pane is alive. A
// container that stops is a death at once. A pane that never comes alive
// over a running container is not up. A probe that cannot answer at all
// (every reading unknown) over a container podman says is running hands
// over to the settle window, which then decides on the container alone.
func waitHarness(ctx context.Context, h harnessCheck, interval, wait time.Duration, log func(string, ...any)) error {
	if interval <= 0 {
		interval = time.Second
	}
	start := time.Now()
	var last harnessReading
	for {
		last = h.read(ctx)
		switch last.state {
		case harnessAlive:
			return nil
		case harnessContainerDown:
			return fmt.Errorf("%w: its container stopped before claude's tmux pane was seen alive, after %s — a failed pre-start hook (lever-agent boot) does this; its container log in the guest holds the last output",
				scion.ErrAgentDied, time.Since(start).Round(100*time.Millisecond))
		}
		if time.Since(start) >= wait {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	switch {
	case last.state == harnessNotAlive:
		return fmt.Errorf("%w: the hub record reads running and the container runs, but claude's tmux pane (scion:agent) was not alive after %s — `lever attach` shows the session, `lever doctor` the manager row",
			scion.ErrAgentNotLive, wait)
	case last.containerUp:
		if log != nil {
			log("WARNING: could not probe the manager's harness (%s); its container runs, so the settle window decides on the container", jail.ProbeErrorClass(last.err))
		}
		return nil
	}
	return fmt.Errorf("%w: neither claude's tmux pane nor the container state could be read (%s); run `lever doctor` for the manager row",
		scion.ErrAgentUnverified, jail.ProbeErrorClass(last.err))
}

// confirmHarness is the look after the settle window: claude's pane still
// alive, or — when the pane cannot be probed — the container still running.
func confirmHarness(ctx context.Context, h harnessCheck, settle time.Duration) error {
	r := h.read(ctx)
	switch {
	case r.state == harnessAlive:
		return nil
	case r.state == harnessContainerDown:
		return fmt.Errorf("%w: its container stopped during the %s settle window; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
	case r.state == harnessNotAlive:
		return fmt.Errorf("%w: claude's tmux pane (scion:agent) died during the %s settle window, over a running container; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
	case r.containerUp:
		return nil
	}
	return fmt.Errorf("%w: after the %s settle window neither claude's tmux pane nor the container state could be read (%s); run `lever doctor` for the manager row",
		scion.ErrAgentUnverified, settle, jail.ProbeErrorClass(r.err))
}

// gateManagerLive is the manager liveness gate every bring-up path shares: the
// start-manager step here and `lever up`'s own resume/no-op paths (WaitManagerLive
// / ObserveManagerLive), which act on the manager WITHOUT going through Run and
// used to print "is up." with no observation at all (lever#31). subject
// prefixes the exhaustion/death error; ctx errors pass through unwrapped.
//
// With a settle window (a path that acted) and an in-container probe, up
// means: the hub record live, then claude's tmux pane alive (waitHarness),
// then the record held live for the settle window, then the pane (or, when
// it cannot be probed, the container) still alive (lever#164). The window
// starts once the harness runs, so a claude that dies seconds after its
// start is inside it however long the pre-start hook took. Without a probe
// or a window it is the hub-record gate alone. prog may be nil.
func gateManagerLive(ctx context.Context, d Deps, b RetryBudget, name, project, subject string, prog *Progress) error {
	err := gateManagerLiveSteps(ctx, d, b, name, project, prog)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: manager %q %w", subject, name, err)
}

func gateManagerLiveSteps(ctx context.Context, d Deps, b RetryBudget, name, project string, prog *Progress) error {
	list := func(c context.Context) ([]scion.Agent, error) {
		agents, err := d.Scion.List(c, project)
		if err == nil {
			if a := scion.FindAgent(agents, name); a != nil {
				prog.observe(a.Phase, a.ContainerStatus)
			} else {
				prog.observe("", "")
			}
		}
		return agents, err
	}
	h, probed := harnessCheckFor(d, name, project)
	if b.Settle <= 0 || !probed {
		prog.Stage("waiting for the hub record to read running")
		return scion.WaitAgentLive(ctx, list, name, scion.LiveBudget{Attempts: b.Attempts, Interval: b.Interval, Settle: b.Settle})
	}
	prog.Stage("waiting for the hub record to read running")
	if err := scion.WaitAgentLive(ctx, list, name, scion.LiveBudget{Attempts: b.Attempts, Interval: b.Interval}); err != nil {
		return err
	}
	wait := d.HarnessWait
	if wait <= 0 {
		wait = defaultHarnessWait
	}
	prog.Stage("waiting for claude's tmux pane (the pre-start hook runs first)")
	if err := waitHarness(ctx, h, b.Interval, wait, d.Log); err != nil {
		return err
	}
	prog.Stage(fmt.Sprintf("holding the %s settle window", b.Settle))
	if err := scion.HoldAgentLive(ctx, list, name, b.Interval, b.Settle); err != nil {
		return err
	}
	return confirmHarness(ctx, h, b.Settle)
}
