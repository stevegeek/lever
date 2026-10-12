package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
)

// defaultHarnessWait bounds how long the manager gate waits, after the hub
// record first reads live, for claude's tmux pane to come alive. The pane
// starts only after scion's pre-start hook (lever-agent boot: enrolment,
// settings, sidecars) has finished, so this covers the whole boot on a cold
// VM. A container that stops in the meantime fails the wait at once; only a
// container that stays up with no harness waits this long.
const defaultHarnessWait = 2 * time.Minute

// harnessSettle is the settle window after claude's own signal (lever#188):
// its tmux pane alive. The pane exists only once the pre-start hook has
// passed and claude has started, and every harness death behind lever#31
// (a task past the tmux cap, `claude --continue` with no conversation, an
// auth failure) landed within three seconds of the record reading running,
// which is before the pane exists; counted from the pane, a later and
// stronger signal, three seconds covers them. It is still probabilistic for
// a later death, as the 10 s window was; `lever doctor`'s manager row and
// `lever up`'s look after a detach catch one. The long Settle of the budget
// stays for a gate that never saw the pane (no probe, or a probe that could
// not run), which counts from the hub record instead.
//
// The hub record's activity is not that signal. claude's SessionStart hook
// reports activity "working" ("Session started"), but sciontool's own start
// report sets the same activity, and the same lastActivityEvent refresh,
// 100 ms after it starts the harness and before claude runs a line; the
// listing does not carry the message that tells the two apart. A variable
// so tests can shorten it.
var harnessSettle = 3 * time.Second

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
	// rep, when it shows steps itself (lever#185), gets the stage and the
	// hub reading as the running step's detail, and no line is logged:
	// its live line and heartbeat say the same.
	rep           *progress.Reporter
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
func StartProgress(ctx context.Context, d Deps, subject, name, stage string) *Progress {
	if rep := progress.From(ctx); rep.Active() {
		p := &Progress{rep: rep, subject: subject, name: name, stage: stage,
			stop: make(chan struct{}), done: make(chan struct{})}
		close(p.done)
		p.report()
		return p
	}
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
	p.report()
}

// report hands the stage and the reading to the step reporter, if any.
func (p *Progress) report() {
	if p.rep == nil {
		return
	}
	// After Stop the running step may be another one: say nothing.
	select {
	case <-p.stop:
		return
	default:
	}
	p.rep.Detail(p.detail())
}

// detail is the stage and the last hub reading.
func (p *Progress) detail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	reading := "no hub reading of the record yet"
	if p.seen {
		reading = fmt.Sprintf("hub phase %q, container %q", scion.PhaseLabel(p.phase), scion.ContainerLabel(p.container))
	}
	return fmt.Sprintf("%s (%s)", p.stage, reading)
}

// observe records the last hub reading of the record.
func (p *Progress) observe(phase, container string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.phase, p.container, p.seen = phase, container, true
	p.mu.Unlock()
	p.report()
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
	elapsed := p.now().Sub(p.start).Round(time.Second)
	return fmt.Sprintf("%s: manager %q still starting — %s; %s elapsed", p.subject, p.name, p.detail(), elapsed)
}

// harnessState is one in-container reading of the manager's harness.
type harnessState int

const (
	harnessUnknown       harnessState = iota // the pane could not be probed
	harnessAlive                             // claude's tmux pane is alive
	harnessNotAlive                          // the container runs, the pane is not alive (not yet, or no more)
	harnessNoServer                          // the container runs, no tmux server answers (not started yet, or gone)
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
	if errors.Is(err, jail.ErrNoTmuxServer) && rerr == nil {
		return harnessReading{state: harnessNoServer, containerUp: true, err: err}
	}
	return harnessReading{state: harnessUnknown, containerUp: rerr == nil && up, err: err}
}

// unknownHandover is how many readings in a row the pane probe may fail
// outright (not "no tmux server": an exec that timed out or exited oddly)
// over a running container before waitHarness stops waiting on it and
// hands over to the settle window: a probe that cannot run must not stall
// every start for the whole harness wait.
const unknownHandover = 5

// notAliveDeath is how many not-alive readings in a row, each with a tmux
// server answering, waitHarness takes as claude's exit. scion creates the
// agent window together with its session and runs claude in it, and the
// shell window keeps tmux (and so the container) up after claude exits: a
// server that answers with no agent window, or a dead pane, is a claude
// that has already exited (a bad --continue, an auth failure), not one
// still booting. The extra readings absorb a single odd answer.
const notAliveDeath = 3

// waitHarness polls the container until claude's tmux pane is alive, and
// reports whether it saw it (false with a nil error: the probe could not
// run and the container runs, so the settle window decides). A
// container that stops, a claude that exited (notAliveDeath not-alive
// readings in a row over an answering tmux server), or a hub record that
// leaves running is a death at once. A tmux server that never answers over
// a running container (a hung pre-start hook) is not up after wait. A probe
// that cannot run at all (unknown readings, unknownHandover in a row) over
// a container podman says is running hands over to the settle window,
// which then decides on the container alone. list re-reads the hub record
// every poll (it feeds the progress line); a list error is skipped.
func waitHarness(ctx context.Context, h harnessCheck, list func(context.Context) ([]scion.Agent, error), name string, interval, wait time.Duration, log func(string, ...any)) (bool, error) {
	if interval <= 0 {
		interval = time.Second
	}
	start := time.Now()
	var last harnessReading
	unknownRun, notAliveRun := 0, 0
	for {
		last = h.read(ctx)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		switch last.state {
		case harnessAlive:
			return true, nil
		case harnessContainerDown:
			return false, fmt.Errorf("%w: its container stopped before claude's tmux pane was seen alive, after %s — a failed pre-start hook (lever-agent boot) does this; its container log in the guest holds the last output",
				scion.ErrAgentDied, time.Since(start).Round(100*time.Millisecond))
		case harnessUnknown:
			unknownRun++
			notAliveRun = 0
		case harnessNotAlive:
			notAliveRun++
			unknownRun = 0
		default:
			unknownRun, notAliveRun = 0, 0
		}
		if notAliveRun >= notAliveDeath {
			return false, fmt.Errorf("%w: claude exited before it came up — the tmux server in its container answers but claude's pane (scion:agent) was not alive in %d readings in a row, after %s (a failed --continue, an auth failure); `lever attach` shows the session, its container log in the guest the last output",
				scion.ErrAgentDied, notAliveRun, time.Since(start).Round(100*time.Millisecond))
		}
		if list != nil {
			if agents, err := list(ctx); err == nil {
				a := scion.FindAgent(agents, name)
				if a == nil || a.Phase != scion.PhaseRunning {
					reading := "record gone from the listing"
					if a != nil {
						reading = fmt.Sprintf("phase %q, container %q", scion.PhaseLabel(a.Phase), scion.ContainerLabel(a.ContainerStatus))
					}
					return false, fmt.Errorf("%w: the hub record left running (%s) before claude's tmux pane was seen alive, after %s; its container log in the guest holds the last output",
						scion.ErrAgentDied, reading, time.Since(start).Round(100*time.Millisecond))
				}
			} else if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if time.Since(start) >= wait || (last.state == harnessUnknown && last.containerUp && unknownRun >= unknownHandover) {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
	}
	switch {
	case last.state == harnessNotAlive:
		return false, fmt.Errorf("%w: the hub record reads running and the container runs, but claude's tmux pane (scion:agent) was not alive after %s — `lever attach` shows the session, `lever doctor` the manager row",
			scion.ErrAgentNotLive, wait)
	case last.state == harnessNoServer:
		return false, fmt.Errorf("%w: the hub record reads running and the container runs, but no tmux server answered in it after %s — claude never started (a pre-start hook still running or hung); its container log in the guest holds the last output, `lever doctor` the manager row",
			scion.ErrAgentNotLive, wait)
	case last.containerUp:
		if log != nil {
			log("lever: warning: could not probe the manager's harness (%s); its container runs, so the settle window decides on the container", jail.ProbeErrorClass(last.err))
		}
		return false, nil
	}
	return false, fmt.Errorf("%w: neither claude's tmux pane nor the container state could be read (%s); run `lever doctor` for the manager row",
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
	case r.state == harnessNoServer:
		return fmt.Errorf("%w: the tmux server in its container went away during the %s settle window, over a running container; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
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
//
// The window is harnessSettle once the pane was seen (lever#188), b.Settle
// otherwise. With o.attach the gate ends once the pane is alive: the caller
// attaches the operator's terminal at once and checks the manager after
// the detach (CheckManagerAfterDetach) instead of holding the window.
func gateManagerLive(ctx context.Context, d Deps, b RetryBudget, name, project, subject string, prog *Progress, o gateOpts) error {
	err := gateManagerLiveSteps(ctx, d, b, name, project, prog, o)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: manager %q %w", subject, name, err)
}

// gateOpts are a gate's options beyond its budget.
type gateOpts struct {
	// attach ends the gate once claude's pane is alive, with no settle
	// window: `lever up` on a terminal attaches then and checks the
	// manager after the detach (lever#188). A gate that never sees the
	// pane holds the window as usual.
	attach bool
	// onHarness, when set, runs once, when the gate has seen the harness
	// (or the record, for a gate without a probe or a window) and the
	// settle window starts: start-manager runs its post-live probes
	// alongside the window (lever#188). Not run when the gate fails first.
	onHarness func()
}

func (o gateOpts) harnessSeen() {
	if o.onHarness != nil {
		o.onHarness()
	}
}

func gateManagerLiveSteps(ctx context.Context, d Deps, b RetryBudget, name, project string, prog *Progress, o gateOpts) error {
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
	prog.Stage("waiting for the hub record to read running")
	if err := scion.WaitAgentLive(ctx, list, name, scion.LiveBudget{Attempts: b.Attempts, Interval: b.Interval}); err != nil {
		return err
	}
	if b.Settle <= 0 || !probed {
		// No harness signal to count from: the record holds for the
		// whole window (none when nothing was started).
		o.harnessSeen()
		if b.Settle > 0 {
			prog.Stage(fmt.Sprintf("holding the %s settle window", b.Settle))
		}
		return scion.HoldAgentLive(ctx, list, name, b.Interval, b.Settle)
	}
	wait := d.HarnessWait
	if wait <= 0 {
		wait = defaultHarnessWait
	}
	prog.Stage("waiting for claude's tmux pane (the pre-start hook runs first)")
	alive, err := waitHarness(ctx, h, list, name, b.Interval, wait, d.Log)
	if err != nil {
		return err
	}
	settle := b.Settle
	if alive {
		settle = min(settle, harnessSettle)
		if o.attach {
			o.harnessSeen()
			return nil
		}
	}
	o.harnessSeen()
	if alive {
		prog.Stage(fmt.Sprintf("claude started; holding the %s settle window", settle))
	} else {
		prog.Stage(fmt.Sprintf("holding the %s settle window", settle))
	}
	if err := scion.HoldAgentLive(ctx, list, name, b.Interval, settle); err != nil {
		return err
	}
	return confirmHarness(ctx, h, settle)
}

// CheckManagerAfterDetach is `lever up`'s look at the manager once the
// operator's attach has ended (lever#188). An up on a terminal attaches as
// soon as claude's pane is alive (WaitManagerStarted), with no settle
// window, so a claude that dies in its first seconds is caught here: the
// attach ends with it, and this names what it finds, with the same hints as
// the start gate. It reports only positive evidence: a missing or
// not-running record, a stopped container, a dead pane or a gone tmux
// server. A probe that cannot run over a running record says nothing.
func CheckManagerAfterDetach(ctx context.Context, d Deps, name, project string) error {
	err := checkAfterDetach(ctx, d, name, project)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	err = fmt.Errorf("up: manager %q is not running after the attach ended: %w — if you did not end it yourself, it died; `lever up` starts it again (the conversation is kept), `lever doctor` shows the manager row", name, err)
	return withHarnessDeathHint(ctx, d, err, project, name)
}

func checkAfterDetach(ctx context.Context, d Deps, name, project string) error {
	if h, probed := harnessCheckFor(d, name, project); probed {
		switch r := h.read(ctx); r.state {
		case harnessAlive:
			return nil
		case harnessContainerDown:
			return fmt.Errorf("%w: its container stopped", scion.ErrAgentDied)
		case harnessNotAlive:
			return fmt.Errorf("%w: claude's tmux pane (scion:agent) is not alive over a running container", scion.ErrAgentDied)
		case harnessNoServer:
			return fmt.Errorf("%w: no tmux server answers in its container", scion.ErrAgentDied)
		}
	}
	b := d.ManagerLiveRetry.or(defaultManagerLiveRetry)
	a, err := scion.ObserveAgent(ctx, func(c context.Context) ([]scion.Agent, error) {
		return d.Scion.List(c, project)
	}, name, observeAttempts, b.Interval)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Not evidence either way.
		return nil
	case a == nil:
		return fmt.Errorf("%w: its record is gone", scion.ErrAgentDied)
	case a.Phase != scion.PhaseRunning:
		return fmt.Errorf("%w: its record is in phase %s (container %s)", scion.ErrAgentDied, scion.BoundedQuote(a.Phase), scion.BoundedQuote(a.ContainerStatus))
	case a.ContainerStatus != "" && !scion.ContainerLive(a.ContainerStatus):
		return fmt.Errorf("%w: its container is %s", scion.ErrAgentDied, scion.BoundedQuote(a.ContainerStatus))
	}
	return nil
}
