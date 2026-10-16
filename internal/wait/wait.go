// Package wait is the one way lever waits for a condition: a probe polled
// with backoff until it reports ready, a definitive failure, or both the
// budget's time and its attempts are spent, with evidence on a timeout.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type kind int

const (
	notYet kind = iota
	ready
	fatal
)

// Result is one probe's answer.
type Result struct {
	kind  kind
	obs   string
	err   error
	again bool
}

// Ready ends the wait with success.
func Ready() Result { return Result{kind: ready} }

// NotYet keeps waiting; obs is what the probe saw, kept as evidence.
func NotYet(obs string) Result { return Result{obs: obs} }

// NotYetErr keeps waiting after a probe that could not observe; err is kept
// as evidence and never ends the wait on its own.
func NotYetErr(err error) Result { return Result{err: err} }

// Fatal ends the wait at once with err, returned as it is. Only for a
// definitive answer that the condition can never hold within this wait: a
// probe that merely failed is NotYetErr.
func Fatal(err error) Result {
	if err == nil {
		err = errors.New("wait: fatal probe result without an error")
	}
	return Result{kind: fatal, err: err}
}

// Now is r with the next pause skipped, for a probe that has already waited
// (an in-guest poll that ran its window). A probe that answers at once must
// not use it, or the wait spins.
func (r Result) Now() Result {
	r.again = true
	return r
}

// Progress is what a wait has seen so far.
type Progress struct {
	Name     string
	Budget   time.Duration
	Elapsed  time.Duration
	Attempts int
	Last     string
	LastErr  error
}

// Spec is one wait's budget. The wait gives up only once Budget has elapsed
// AND MinAttempts probes have run, so neither a slow probe nor a fast one can
// shorten it. The first pause is Interval; it doubles after each probe up to
// MaxInterval when that is larger.
type Spec struct {
	Name        string
	Budget      time.Duration
	MinAttempts int
	Interval    time.Duration
	MaxInterval time.Duration
	// Notice, when set, is called once Elapsed reaches NoticeAfter, then
	// every NoticeEvery (0: once).
	NoticeAfter time.Duration
	NoticeEvery time.Duration
	Notice      func(Progress)

	clock clock
}

// Attempts is the Spec of a fixed-interval wait that gives up after n
// probes, as a loop of n probes with interval between them did: its time
// budget is the n-1 pauses, so fast probes run exactly n times.
func Attempts(name string, n int, interval time.Duration) Spec {
	n = max(n, 1)
	return Spec{Name: name, MinAttempts: n, Budget: time.Duration(n-1) * interval, Interval: interval}
}

// Total is how long s waits at least, its pauses alone, when its probes
// take no time.
func (s Spec) Total() time.Duration {
	if s.MinAttempts <= 1 || s.MaxInterval <= s.Interval {
		return max(s.Budget, time.Duration(max(s.MinAttempts-1, 0))*s.Interval)
	}
	var total time.Duration
	iv := s.Interval
	for i := 1; i < s.MinAttempts; i++ {
		total += iv
		iv = min(iv*2, s.MaxInterval)
	}
	return max(s.Budget, total)
}

// ErrTimeout is what a TimeoutError Is.
var ErrTimeout = errors.New("wait: timed out")

// TimeoutError is a wait that spent its whole budget, with what it saw last.
// It does not wrap LastErr: a probe error can carry an inner context deadline
// while the wait's own context stayed live.
type TimeoutError struct {
	Progress
}

func (e *TimeoutError) Error() string {
	msg := fmt.Sprintf("%s: not done after %s (%d attempts)", e.Name, e.Elapsed.Round(100*time.Millisecond), e.Attempts)
	if e.Last != "" {
		msg += "; last seen: " + e.Last
	}
	if e.LastErr != nil {
		msg += fmt.Sprintf("; last error: %v", e.LastErr)
	}
	return msg
}

func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

// For polls probe under s until it is Ready (nil), Fatal (its error), the
// budget is spent (*TimeoutError) or ctx ends (ctx.Err(), unwrapped). The
// probe always runs at least once, and a timeout returns without a final
// pause. A NotYet observation replaces the last one; an error is kept until a
// probe observes again.
func For(ctx context.Context, s Spec, probe func(context.Context) Result) error {
	clk := s.clock
	if clk == nil {
		clk = realClock{}
	}
	start := clk.Now()
	p := Progress{Name: s.Name, Budget: s.Budget}
	nextNotice := s.NoticeAfter
	interval := s.Interval
	for {
		p.Attempts++
		r := probe(ctx)
		switch r.kind {
		case ready:
			return nil
		case fatal:
			return r.err
		}
		if r.err != nil {
			p.LastErr = r.err
		} else {
			p.Last, p.LastErr = r.obs, nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.Elapsed = clk.Now().Sub(start)
		if p.Attempts >= s.MinAttempts && p.Elapsed >= s.Budget {
			return &TimeoutError{Progress: p}
		}
		if s.Notice != nil && s.NoticeAfter > 0 && nextNotice >= 0 && p.Elapsed >= nextNotice {
			s.Notice(p)
			if s.NoticeEvery > 0 {
				nextNotice = p.Elapsed + s.NoticeEvery
			} else {
				nextNotice = -1
			}
		}
		if r.again {
			continue
		}
		pause := interval
		if p.Attempts >= s.MinAttempts {
			pause = min(pause, s.Budget-p.Elapsed)
		}
		if s.MaxInterval > interval {
			interval = min(interval*2, s.MaxInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(max(pause, 0)):
		}
	}
}

type clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
