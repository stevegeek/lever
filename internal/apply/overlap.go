package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/stevegeek/lever/internal/progress"
)

// Bring-up steps that nothing before start-manager depends on run alongside
// the others (lever#189). broker-up is a host child process with its own
// health poll; load-image (with its keep-id warm-up) is podman work in the
// guest that only an agent start reads. Neither is needed by init-machine,
// config-registry, bootstrap-token, scion-server, credential,
// register-project or agent-template, which make up most of a cold apply
// (the hub's start alone is 15-20 s), so a changed image's load and warm-up
// and the broker's start hide behind them.
//
// Each lane runs its steps one after another, in plan order, on its own
// goroutine: two image loads never compete for the guest's disk. A step that
// needs a lane (waitsFor) waits for the whole lane to finish first, and so
// does the end of the run.
//
// Failures: a lane step that fails ends its lane, prints its FAIL line at
// once, and stops the run before the next foreground step starts (the
// foreground step already running is left to finish: interrupting a hub
// start halfway helps nobody). A foreground failure stops the lanes: their
// running step is cancelled and listed as stopped, not failed, and steps
// not yet started are not run. The error returned names every step that
// really failed.

// lane names the background lane a step kind runs in; "" is the foreground.
func lane(k StepKind) string {
	switch k {
	case KindBrokerUp:
		return "broker"
	case KindLoadImage:
		return "images"
	}
	return ""
}

// waitsFor names the lanes a foreground step needs finished before it
// starts. mint-manager-bootstrap asks the broker for the enrol ticket, the
// remote proxy reaches the broker's operator socket, and start-manager needs
// the broker and every image.
func waitsFor(k StepKind) []string {
	switch k {
	case KindMintManagerBootstrap, KindRemoteProxy:
		return []string{"broker"}
	case KindStartManager:
		return []string{"broker", "images"}
	}
	return nil
}

// bgLane is one background lane of a run.
type bgLane struct {
	name   string
	steps  []Step
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason string // why the run stopped the lane; "" while it may run
	err    error  // the lane step that failed, "step <kind>: ..."; set before done closes
}

// lanes runs a plan's background lanes.
type lanes struct {
	r     *run
	order []*bgLane
	by    map[string]*bgLane
}

// startLanes takes the background steps out of steps, starts their lanes,
// and returns the foreground steps in plan order.
func startLanes(ctx context.Context, r *run, steps []Step) (*lanes, []Step) {
	ls := &lanes{r: r, by: map[string]*bgLane{}}
	var fg []Step
	for _, s := range steps {
		name := lane(s.Kind)
		if name == "" {
			fg = append(fg, s)
			continue
		}
		l := ls.by[name]
		if l == nil {
			l = &bgLane{name: name, done: make(chan struct{})}
			ls.by[name] = l
			ls.order = append(ls.order, l)
		}
		l.steps = append(l.steps, s)
	}
	// Each lane's first step is listed here, in plan order, so the step
	// numbers follow the plan; a lane lists its later steps as it starts
	// them.
	for _, l := range ls.order {
		lctx, cancel := context.WithCancel(ctx)
		l.cancel = cancel
		first := ls.startStep(ctx, l.steps[0])
		go ls.run(lctx, l, first)
	}
	return ls, fg
}

// startStep lists a lane step as started.
func (ls *lanes) startStep(ctx context.Context, s Step) *progress.Step {
	ps := progress.From(ctx).Background(stepLabel(s))
	if ls.r.d.StepTail != nil {
		if fn := ls.r.d.StepTail(s.Kind); fn != nil {
			ps.TailFrom(fn)
		}
	}
	return ps
}

// run is one lane's goroutine; first is its first step's listing.
func (ls *lanes) run(ctx context.Context, l *bgLane, first *progress.Step) {
	defer close(l.done)
	for i, s := range l.steps {
		ps := first
		if i > 0 {
			if l.stopped() != "" || ctx.Err() != nil {
				return
			}
			ps = ls.startStep(ctx, s)
		}
		err := ls.r.step(progress.WithStep(ctx, ps), s)
		if reason := l.stopped(); err != nil && reason != "" {
			ps.Skip("stopped: " + reason)
			return
		}
		ps.Done(err)
		if err != nil {
			l.mu.Lock()
			l.err = fmt.Errorf("step %s: %w", s.Kind, err)
			l.mu.Unlock()
			return
		}
	}
}

func (l *bgLane) stopped() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

func (l *bgLane) failure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// failed returns the error of a lane that has already failed, without
// waiting for the others.
func (ls *lanes) failed() error {
	for _, l := range ls.order {
		select {
		case <-l.done:
			if err := l.failure(); err != nil {
				return err
			}
		default:
		}
	}
	return nil
}

// join waits for the named lanes (every lane when all is set) and returns
// the first failure among them, in plan order. ctx ends the wait (the
// lanes see the same cancellation).
func (ls *lanes) join(ctx context.Context, names []string, all bool) error {
	for _, l := range ls.order {
		if !all && !slices.Contains(names, l.name) {
			continue
		}
		select {
		case <-l.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := l.failure(); err != nil {
			return err
		}
	}
	return nil
}

// stop ends every lane still running, for reason, and waits for them all.
// It returns the failures of lanes that failed on their own, in plan order.
func (ls *lanes) stop(reason string) []error {
	for _, l := range ls.order {
		l.mu.Lock()
		if l.reason == "" {
			l.reason = reason
		}
		l.mu.Unlock()
		l.cancel()
	}
	var errs []error
	for _, l := range ls.order {
		<-l.done
		if err := l.failure(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// runSteps runs the plan: the background lanes alongside the foreground
// steps, each foreground step after the lanes it needs. It returns once
// every lane has ended, with every real failure joined.
func (r *run) runSteps(ctx context.Context, steps []Step) error {
	rep := progress.From(ctx)
	ls, fg := startLanes(ctx, r, steps)
	var runErr error
	for _, s := range fg {
		if runErr = ls.failed(); runErr != nil {
			break
		}
		if runErr = ls.join(ctx, waitsFor(s.Kind), false); runErr != nil {
			break
		}
		ps := rep.Start(stepLabel(s))
		if r.d.StepTail != nil {
			if fn := r.d.StepTail(s.Kind); fn != nil {
				ps.TailFrom(fn)
			}
		}
		err := r.step(ctx, s)
		ps.Done(err)
		if err != nil {
			runErr = fmt.Errorf("step %s: %w", s.Kind, err)
			break
		}
	}
	if runErr == nil {
		runErr = ls.join(ctx, nil, true)
	}
	if runErr == nil {
		return nil
	}
	errs := []error{runErr}
	for _, err := range ls.stop(failedReason(runErr)) {
		if err != runErr {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// failedReason is the note on a lane step the run stopped.
func failedReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "the run was interrupted"
	}
	return "an earlier step failed"
}
