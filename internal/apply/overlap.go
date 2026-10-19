package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/stevegeek/lever/internal/progress"
)

// Bring-up steps that nothing before start-manager depends on (broker-up,
// load-image) run in background lanes alongside the foreground steps.
//
// Each lane runs its steps in plan order on its own goroutine, so two image
// loads never compete for the guest's disk. A step that needs a lane
// (waitsFor) waits for the whole lane, and so does the end of the run.
//
// A lane failure stops the run before the next foreground step starts; the
// foreground step already running is left to finish, since interrupting a
// hub start halfway helps nobody. A foreground failure cancels the lanes:
// their running step is listed as stopped, not failed. The error returned
// names every step that really failed.

// lane returns "" for the foreground.
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
// starts. mint-manager-bootstrap asks the broker for the enrol ticket and the
// remote proxy reaches the broker's operator socket.
func waitsFor(k StepKind) []string {
	switch k {
	case KindMintManagerBootstrap, KindRemoteProxy, KindWorkerAutostart:
		return []string{"broker"}
	case KindStartManager:
		return []string{"broker", "images"}
	}
	return nil
}

type bgLane struct {
	name   string
	steps  []Step
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason string // why the run stopped the lane; "" while it may run
	err    error  // the lane step that failed, "step <kind>: ..."; set before done closes
}

type lanes struct {
	r     *run
	order []*bgLane
	by    map[string]*bgLane
	// notify is buffered for every lane so a lane's end never blocks.
	notify chan struct{}
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
	// Each lane's first step is listed here so its number follows the plan;
	// later lane steps are numbered as they start.
	ls.notify = make(chan struct{}, len(ls.order))
	for _, l := range ls.order {
		lctx, cancel := context.WithCancel(ctx)
		l.cancel = cancel
		first := ls.startStep(ctx, l.steps[0])
		go ls.run(lctx, l, first)
	}
	return ls, fg
}

func (ls *lanes) startStep(ctx context.Context, s Step) *progress.Step {
	ps := progress.From(ctx).Background(stepLabel(s))
	if ls.r.d.StepTail != nil {
		if fn := ls.r.d.StepTail(s.Kind); fn != nil {
			ps.TailFrom(fn)
		}
	}
	return ps
}

func (ls *lanes) run(ctx context.Context, l *bgLane, first *progress.Step) {
	defer func() {
		close(l.done)
		ls.notify <- struct{}{}
		l.cancel()
	}()
	for i, s := range l.steps {
		ps := first
		if i > 0 {
			if l.stopped() != "" || ctx.Err() != nil {
				return
			}
			ps = ls.startStep(ctx, s)
		}
		err := ls.r.step(progress.WithStep(ctx, ps), s)
		if err != nil && (l.stopped() != "" || ctx.Err() != nil) {
			// Stopped by the run or an interrupt: not a failure of this step.
			reason := l.stopped()
			if reason == "" {
				reason = failedReason(ctx.Err())
			}
			ps.Skip("stopped: " + reason)
			return
		}
		if err != nil {
			// Recorded before the step is listed as failed, so a
			// foreground step that sees the FAIL line sees the failure.
			l.mu.Lock()
			l.err = fmt.Errorf("step %s: %w", s.Kind, err)
			l.mu.Unlock()
		}
		ps.Done(err)
		if err != nil {
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

func (ls *lanes) failed() error {
	for _, l := range ls.order {
		if err := l.failure(); err != nil {
			return err
		}
	}
	return nil
}

// join waits for the named lanes (every lane when all is set). A failure of
// any lane, named or not, ends the wait at once and is returned.
func (ls *lanes) join(ctx context.Context, names []string, all bool) error {
	for {
		if err := ls.failed(); err != nil {
			return err
		}
		pending := false
		for _, l := range ls.order {
			if !all && !slices.Contains(names, l.name) {
				continue
			}
			select {
			case <-l.done:
			default:
				pending = true
			}
		}
		if !pending {
			return nil
		}
		select {
		case <-ls.notify:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// stop returns only the failures of lanes that failed on their own.
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

// runSteps returns once every lane has ended, with every real failure joined.
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

func failedReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "the run was interrupted"
	}
	return "an earlier step failed"
}
