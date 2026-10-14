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

// defaultHarnessWait bounds the wait, after the hub record first reads live,
// for claude's tmux pane. The pane starts only after scion's pre-start hook
// (lever-agent boot) has finished, so this covers a whole cold-VM boot.
const defaultHarnessWait = 2 * time.Minute

// harnessSettle is the settle window after claude's pane is seen alive, used
// only when claude does not report its session start (no claude-ready hook,
// or a probe that cannot wait for it). Known harness deaths (a task past the
// tmux cap, --continue with no conversation, an auth failure) land within
// three seconds of the pane; a later death is caught by `lever doctor` and
// `lever up`'s look after a detach.
//
// The hub record's activity is no start signal: sciontool's own start report
// sets the same activity as claude's SessionStart hook, before claude runs,
// and the listing cannot tell the two apart. Hence lever's own claude-ready
// marker. A variable so tests can shorten it.
var harnessSettle = 3 * time.Second

// progressEvery paces the progress line: a manager start can run over a
// minute, and a silent minute cannot be told from a hang.
const progressEvery = 15 * time.Second

// Progress prints a line about every progressEvery while a manager start
// runs: the stage, the last hub reading of the record, and the elapsed time.
// A nil *Progress is valid and prints nothing.
type Progress struct {
	// rep, when active, gets the stage and reading as the running step's
	// detail instead of a logged line.
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
// prefixes it. The caller must call Stop. A Deps without Log yields a nil
// *Progress.
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

func (p *Progress) detail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	reading := "no hub reading of the record yet"
	if p.seen {
		reading = fmt.Sprintf("hub phase %q, container %q", scion.PhaseLabel(p.phase), scion.ContainerLabel(p.container))
	}
	return fmt.Sprintf("%s (%s)", p.stage, reading)
}

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

// line: phase and container are hub text an agent can set about itself, so
// they pass through the display labels.
func (p *Progress) line() string {
	elapsed := p.now().Sub(p.start).Round(time.Second)
	return fmt.Sprintf("%s: manager %q still starting — %s; %s elapsed", p.subject, p.name, p.detail(), elapsed)
}

type harnessState int

const (
	harnessUnknown       harnessState = iota // the pane could not be probed
	harnessAlive                             // claude's tmux pane is alive
	harnessNotAlive                          // the container runs, the pane is not alive (not yet, or no more)
	harnessNoServer                          // the container runs, no tmux server answers (not started yet, or gone)
	harnessContainerDown                     // the container is gone or not running
)

type harnessReading struct {
	state harnessState
	// containerUp: podman itself (not the hub's heartbeat column) said the
	// container runs. Only meaningful for harnessUnknown.
	containerUp bool
	err         error
}

// harnessCheck is the in-container half of the manager gate. The hub record
// is not enough: scion marks it running when the container launches and the
// pre-start hook runs after that, so a failed hook can leave the record
// running over an "Up" container. The tmux pane exists only once the hook
// has passed and claude has started.
type harnessCheck struct {
	probe   AgentSessionProbe
	running func(ctx context.Context, ref string) (bool, error)
	ref     string
}

// harnessCheckFor returns ok=false when Deps has no in-container probe; the
// gate then reads the hub record alone.
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

// unknownHandover is how many outright probe failures in a row (not "no tmux
// server") over a running container hand over to the settle window: a probe
// that cannot run must not stall every start for the whole harness wait.
const unknownHandover = 5

// notAliveDeath is how many not-alive readings in a row, with a tmux server
// answering, count as claude's exit. scion creates the agent window with its
// session, and the shell window keeps tmux (and the container) up after
// claude exits, so an answering server with no live agent pane is a claude
// that already exited, not one still booting. The extra readings absorb a
// single odd answer.
const notAliveDeath = 3

// waitHarness polls until claude's tmux pane is alive and reports whether it
// saw it; false with a nil error means the probe could not run over a
// running container, and the settle window decides. A stopped container, a
// claude that exited, or a record that leaves running fails at once. A list
// error is skipped.
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
// alive, or, when the pane cannot be probed, the container still running.
func confirmHarness(ctx context.Context, h harnessCheck, settle time.Duration) error {
	_, err := confirmHarnessPane(ctx, h, settle)
	return err
}

// confirmHarnessPane returns false with a nil error when the pane could not
// be read but the container runs.
func confirmHarnessPane(ctx context.Context, h harnessCheck, settle time.Duration) (bool, error) {
	r := h.read(ctx)
	switch {
	case r.state == harnessAlive:
		return true, nil
	case r.state == harnessContainerDown:
		return false, fmt.Errorf("%w: its container stopped during the %s settle window; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
	case r.state == harnessNoServer:
		return false, fmt.Errorf("%w: the tmux server in its container went away during the %s settle window, over a running container; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
	case r.state == harnessNotAlive:
		return false, fmt.Errorf("%w: claude's tmux pane (scion:agent) died during the %s settle window, over a running container; its container log in the guest holds the last output", scion.ErrAgentDied, settle)
	case r.containerUp:
		return false, nil
	}
	return false, fmt.Errorf("%w: after the %s settle window neither claude's tmux pane nor the container state could be read (%s); run `lever doctor` for the manager row",
		scion.ErrAgentUnverified, settle, jail.ProbeErrorClass(r.err))
}

// gateManagerLive is the manager liveness gate every bring-up path shares,
// including `lever up`'s resume/no-op paths that bypass Run. subject
// prefixes the error; ctx errors pass through unwrapped. prog may be nil.
//
// With a settle window and an in-container probe, up means: the hub record
// live, then claude's pane alive, then claude's claude-ready marker
// (waitClaudeReady). Without a marker, the record is held live for the
// settle window (counted from the pane, so a slow pre-start hook does not
// eat it) and the pane or container is checked once more. Without a probe or
// a window it is the hub-record gate alone. With o.attach the gate ends once
// the pane is alive; the caller checks via CheckManagerAfterDetach.
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

type gateOpts struct {
	// attach ends the gate once claude's pane is alive. Only for a manager
	// the run created: a resume's claude --continue exits soon after its
	// pane comes up when it finds no conversation, so a resume must wait for
	// claude's session start.
	attach bool
	// onHarness runs once, when the gate has seen the harness (or the record,
	// without a probe or window) and the readiness wait begins. Not run when
	// the gate fails first.
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
		if rp, ok := h.probe.(claudeReadyProbe); ok {
			o.harnessSeen()
			prog.Stage("claude started; waiting for its session start (lever's claude-ready hook)")
			readyWait := d.ReadyWait
			if readyWait <= 0 {
				readyWait = defaultReadyWait
			}
			outcome, err := waitClaudeReady(ctx, h, rp, list, name, b.Interval, readyWait, settle, d.Log)
			if err != nil || outcome == readyUp {
				return err
			}
			if outcome == readySilent {
				// The pane stayed alive but claude never reported: the
				// settle window decides, and a pass is up with a warning.
				prog.Stage(fmt.Sprintf("claude never reported its session start; holding the %s settle window", settle))
				paneSeen, err := holdAndConfirmPane(ctx, h, list, name, b.Interval, settle)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return err
					}
					return fmt.Errorf("%w (and claude had not reported its session start in the %s before it)", err, readyWait)
				}
				if d.Log != nil {
					after := "claude's pane was still alive after it"
					if !paneSeen {
						after = "after it claude's pane could not be read but its container still ran"
					}
					d.Log("lever: warning: the manager's claude never reported its session start to lever (lever's claude-ready hook) in %s, though its tmux pane stayed alive — likely a dialog that waits for a key before claude starts its session (a trust, login or update prompt), a /tmp in the container claude cannot write, or hooks disabled in its settings; with no readiness signal, lever held the %s settle window instead, %s, and the manager counts as up. `lever attach` shows the session, `lever doctor` the manager row", readyWait, settle, after)
				}
				return nil
			}
			prog.Stage(fmt.Sprintf("claude started; holding the %s settle window", settle))
			return holdAndConfirm(ctx, h, list, name, b.Interval, settle)
		}
	}
	o.harnessSeen()
	if alive {
		prog.Stage(fmt.Sprintf("claude started; holding the %s settle window", settle))
	} else {
		prog.Stage(fmt.Sprintf("holding the %s settle window", settle))
	}
	return holdAndConfirm(ctx, h, list, name, b.Interval, settle)
}

func holdAndConfirm(ctx context.Context, h harnessCheck, list func(context.Context) ([]scion.Agent, error), name string, interval, settle time.Duration) error {
	if err := scion.HoldAgentLive(ctx, list, name, interval, settle); err != nil {
		return err
	}
	return confirmHarness(ctx, h, settle)
}

func holdAndConfirmPane(ctx context.Context, h harnessCheck, list func(context.Context) ([]scion.Agent, error), name string, interval, settle time.Duration) (bool, error) {
	if err := scion.HoldAgentLive(ctx, list, name, interval, settle); err != nil {
		return false, err
	}
	return confirmHarnessPane(ctx, h, settle)
}

// defaultReadyWait bounds the wait, once the pane is alive, for claude's
// claude-ready marker. Claude runs SessionStart hooks after loading the
// conversation, which takes seconds; one that has not reported in two
// minutes is likely stuck on a key-wait dialog, an unwritable /tmp or
// disabled hooks. That is no death: the gate falls back to the settle
// window and warns.
const defaultReadyWait = 2 * time.Minute

// readyPollWindow is how long one in-container wait for the marker polls.
// The script counts polls, not time, and each poll forks a few programs, so
// the window stays well under the guest's 8 s exec deadline, with room for
// podman exec's own start on a busy VM. A variable so tests can shorten it.
var readyPollWindow = 3 * time.Second

// claudeReadyProbe is the optional part of AgentSessionProbe that waits for
// claude's session-start marker. A probe without it keeps the settle window.
type claudeReadyProbe interface {
	AwaitClaudeReady(ctx context.Context, ref string, window time.Duration) (jail.ClaudeReady, error)
}

type readyOutcome int

const (
	readyUp readyOutcome = iota + 1
	// readyNoSignal: no marker to wait for (no hook, or a wait that cannot
	// run); the settle window decides.
	readyNoSignal
	// readySilent: the full wait passed with no marker and no death, the
	// last reading a live pane; the settle window decides, with a warning.
	readySilent
)

// waitClaudeReady returns readyUp once the pane is alive and the
// claude-ready marker names this container run and that pane. No settle
// window follows: every known early death (failed pre-start hook, tmux cap,
// "No conversation found to continue") happens before SessionStart. A death
// while waiting fails at once.
//
// The marker is the agent's word: an agent that writes it itself claims
// only that claude started, which a live pane already claims. lever acts on
// nothing in it beyond its equality with what the probe computes.
func waitClaudeReady(ctx context.Context, h harnessCheck, rp claudeReadyProbe, list func(context.Context) ([]scion.Agent, error), name string, interval, wait, settle time.Duration, log func(string, ...any)) (readyOutcome, error) {
	if interval <= 0 {
		interval = time.Second
	}
	start := time.Now()
	since := func() time.Duration { return time.Since(start).Round(100 * time.Millisecond) }
	unknownRun := 0
	var lastErr error
	for {
		call := time.Now()
		st, err := rp.AwaitClaudeReady(ctx, h.ref, readyPollWindow)
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		pace := true
		switch {
		case err == nil && st == jail.ClaudeReadyYes:
			if reading, left := recordLeftRunning(ctx, list, name); left {
				return 0, fmt.Errorf("%w: claude reported its session started, but the hub record left running (%s); its container log in the guest holds the last output", scion.ErrAgentDied, reading)
			}
			return readyUp, ctx.Err()
		case err == nil && st == jail.ClaudeReadyNoHook:
			if log != nil {
				log("lever: note: the manager's claude does not report its session start to lever (no claude-ready hook in its managed settings: an image built by an older lever, or a boot that could not write the file); the %s settle window decides", settle)
			}
			return readyNoSignal, nil
		case err == nil && st == jail.ClaudeReadyPending:
			unknownRun = 0
			// An in-container wait that came back early (its sleep could
			// not run) must not turn into a busy loop.
			pace = time.Since(call) < readyPollWindow/2
		default:
			// No live pane in the in-container wait, or the wait could not
			// run: the pane probe decides.
			if err != nil {
				lastErr = err
			}
			switch r := h.read(ctx); r.state {
			case harnessContainerDown:
				return 0, fmt.Errorf("%w: its container stopped after claude's tmux pane came up, before claude reported its session started, after %s; its container log in the guest holds the last output",
					scion.ErrAgentDied, since())
			case harnessNotAlive:
				return 0, fmt.Errorf("%w: claude's tmux pane (scion:agent) died before claude reported its session started, after %s, over a running container (a failed --continue exits so); `lever attach` shows the session, its container log in the guest the last output",
					scion.ErrAgentDied, since())
			case harnessNoServer:
				return 0, fmt.Errorf("%w: the tmux server in its container went away before claude reported its session started, after %s, over a running container; its container log in the guest holds the last output",
					scion.ErrAgentDied, since())
			case harnessAlive:
				// The two readings disagree: neither is evidence.
				unknownRun++
				if lastErr == nil {
					lastErr = errReadingsDisagree
				}
			default:
				unknownRun++
				if lastErr == nil {
					lastErr = r.err
				}
			}
		}
		if unknownRun >= unknownHandover {
			if log != nil {
				log("lever: warning: could not wait for the manager's claude to report its session start (%s); the %s settle window decides", readyErrClass(lastErr), settle)
			}
			return readyNoSignal, nil
		}
		if reading, left := recordLeftRunning(ctx, list, name); left {
			return 0, fmt.Errorf("%w: the hub record left running (%s) before claude reported its session started, after %s; its container log in the guest holds the last output",
				scion.ErrAgentDied, reading, since())
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if time.Since(start) >= wait {
			// Only a last reading that saw the pane alive is readySilent.
			if unknownRun > 0 {
				if log != nil {
					log("lever: warning: could not wait for the manager's claude to report its session start (%s), and it had not reported in %s; the %s settle window decides", readyErrClass(lastErr), wait, settle)
				}
				return readyNoSignal, nil
			}
			return readySilent, nil
		}
		if !pace {
			continue
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
}

var errReadingsDisagree = errors.New("the pane readings disagree")

func readyErrClass(err error) string {
	if errors.Is(err, errReadingsDisagree) {
		return err.Error()
	}
	return jail.ProbeErrorClass(err)
}

// recordLeftRunning: a list error is not evidence either way.
func recordLeftRunning(ctx context.Context, list func(context.Context) ([]scion.Agent, error), name string) (string, bool) {
	if list == nil {
		return "", false
	}
	agents, err := list(ctx)
	if err != nil {
		return "", false
	}
	a := scion.FindAgent(agents, name)
	if a == nil {
		return "record gone from the listing", true
	}
	if a.Phase != scion.PhaseRunning {
		return fmt.Sprintf("phase %q, container %q", scion.PhaseLabel(a.Phase), scion.ContainerLabel(a.ContainerStatus)), true
	}
	return "", false
}

// CheckManagerAfterDetach is `lever up`'s look at the manager once the
// operator's attach has ended; it catches a claude that died in its first
// seconds. It reports only positive evidence of death; a probe that cannot
// run over a running record says nothing.
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
