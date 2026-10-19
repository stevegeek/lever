package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock returns from every pause at once and advances by exactly the
// pause; a probe can advance it further to model a slow probe.
type fakeClock struct {
	t      time.Time
	pauses []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.pauses = append(c.pauses, d)
	c.t = c.t.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.t
	return ch
}

func withClock(s Spec, c *fakeClock) Spec {
	s.clock = c
	return s
}

func TestReadyOnFirstProbe(t *testing.T) {
	c := &fakeClock{}
	n := 0
	err := For(context.Background(), withClock(Attempts("x", 5, time.Second), c), func(context.Context) Result {
		n++
		return Ready()
	})
	if err != nil || n != 1 || len(c.pauses) != 0 {
		t.Fatalf("err=%v probes=%d pauses=%v", err, n, c.pauses)
	}
}

// Attempts(n) probes exactly n times with fast probes and no final pause, as
// a loop of n tries did.
func TestAttemptsRunsExactlyN(t *testing.T) {
	c := &fakeClock{}
	n := 0
	err := For(context.Background(), withClock(Attempts("phase", 4, time.Second), c), func(context.Context) Result {
		n++
		return NotYet("starting")
	})
	var te *TimeoutError
	if !errors.As(err, &te) || !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want a TimeoutError", err)
	}
	if n != 4 || len(c.pauses) != 3 || te.Attempts != 4 || te.Last != "starting" || te.Elapsed != 3*time.Second {
		t.Fatalf("probes=%d pauses=%v evidence=%+v", n, c.pauses, te.Progress)
	}
	if !strings.Contains(err.Error(), "phase: not done after 3s (4 attempts); last seen: starting") {
		t.Fatalf("message: %v", err)
	}
}

// A slow probe that spends the time budget alone still gets MinAttempts.
func TestSlowProbesStillGetMinAttempts(t *testing.T) {
	c := &fakeClock{}
	n := 0
	err := For(context.Background(), withClock(Attempts("x", 3, time.Second), c), func(context.Context) Result {
		n++
		c.t = c.t.Add(time.Minute)
		return NotYet("")
	})
	if !errors.Is(err, ErrTimeout) || n != 3 {
		t.Fatalf("err=%v probes=%d, want 3 probes", err, n)
	}
}

// Fast probes still wait out the whole time budget.
func TestFastProbesStillWaitTheBudget(t *testing.T) {
	c := &fakeClock{}
	s := withClock(Spec{Name: "hub", Budget: 10 * time.Second, Interval: 3 * time.Second}, c)
	n := 0
	err := For(context.Background(), s, func(context.Context) Result { n++; return NotYet("") })
	var te *TimeoutError
	if !errors.As(err, &te) || te.Elapsed != 10*time.Second || n != 5 {
		t.Fatalf("err=%v probes=%d pauses=%v", err, n, c.pauses)
	}
	// The last pause is cut to the budget instead of overshooting it.
	if got := c.pauses[len(c.pauses)-1]; got != time.Second {
		t.Fatalf("last pause %s, want 1s", got)
	}
}

func TestBackoffDoublesToTheCap(t *testing.T) {
	c := &fakeClock{}
	s := withClock(Spec{Name: "claim", MinAttempts: 6, Interval: 5 * time.Second, MaxInterval: 30 * time.Second}, c)
	_ = For(context.Background(), s, func(context.Context) Result { return NotYet("") })
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(c.pauses) != len(want) {
		t.Fatalf("pauses %v, want %v", c.pauses, want)
	}
	for i := range want {
		if c.pauses[i] != want[i] {
			t.Fatalf("pauses %v, want %v", c.pauses, want)
		}
	}
	if s.Total() != 95*time.Second {
		t.Fatalf("Total = %s", s.Total())
	}
}

// The stop-claim budget is the 5+10+20+30*5 s it always was.
func TestStopClaimBudgetTotal(t *testing.T) {
	if got := StopClaimReleased.Total(); got != 185*time.Second {
		t.Fatalf("StopClaimReleased.Total() = %s", got)
	}
}

func TestFatalStopsAtOnceWithItsError(t *testing.T) {
	c := &fakeClock{}
	gone := errors.New("server gone")
	n := 0
	err := For(context.Background(), withClock(Attempts("x", 100, time.Second), c), func(context.Context) Result {
		n++
		if n == 3 {
			return Fatal(gone)
		}
		return NotYet("")
	})
	if err != gone || n != 3 {
		t.Fatalf("err=%v probes=%d", err, n)
	}
}

// A probe error is evidence, not an end; a later observation replaces it.
func TestProbeErrorsAreEvidence(t *testing.T) {
	c := &fakeClock{}
	boom := errors.New("list failed")
	n := 0
	err := For(context.Background(), withClock(Attempts("x", 3, time.Second), c), func(context.Context) Result {
		n++
		if n < 3 {
			return NotYetErr(boom)
		}
		return NotYet("suspended")
	})
	var te *TimeoutError
	if !errors.As(err, &te) || te.LastErr != nil || te.Last != "suspended" {
		t.Fatalf("err=%v", err)
	}
	n = 0
	err = For(context.Background(), withClock(Attempts("x", 2, time.Second), &fakeClock{}), func(context.Context) Result {
		n++
		return NotYetErr(context.DeadlineExceeded)
	})
	if !errors.As(err, &te) || te.LastErr == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a probe's own deadline must stay evidence, not read as the wait's: %v", err)
	}
}

func TestCancelledContextReturnsItsErrorUnwrapped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := For(ctx, Attempts("x", 30, time.Hour), func(context.Context) Result { n++; return NotYet("") })
	if err != context.Canceled || n != 1 {
		t.Fatalf("err=%v probes=%d", err, n)
	}
}

// A context deadline ends the wait however much budget is left.
func TestContextDeadlineEndsTheWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := For(ctx, Attempts("x", 30, time.Hour), func(context.Context) Result { return NotYet("") })
	if err != context.DeadlineExceeded || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestNowSkipsThePause(t *testing.T) {
	c := &fakeClock{}
	n := 0
	err := For(context.Background(), withClock(Attempts("x", 3, time.Hour), c), func(context.Context) Result {
		n++
		c.t = c.t.Add(time.Hour) // the probe waited itself
		if n == 3 {
			return Ready()
		}
		return NotYet("").Now()
	})
	if err != nil || len(c.pauses) != 0 {
		t.Fatalf("err=%v pauses=%v", err, c.pauses)
	}
}

func TestNotices(t *testing.T) {
	c := &fakeClock{}
	var seen []time.Duration
	s := withClock(Spec{Name: "hub", Budget: 60 * time.Second, Interval: time.Second,
		NoticeAfter: 15 * time.Second, NoticeEvery: 30 * time.Second,
		Notice: func(p Progress) { seen = append(seen, p.Elapsed) }}, c)
	_ = For(context.Background(), s, func(context.Context) Result { return NotYet("") })
	if len(seen) != 2 || seen[0] != 15*time.Second || seen[1] != 45*time.Second {
		t.Fatalf("notices at %v", seen)
	}
	seen = nil
	s.NoticeEvery = 0
	c.t = time.Time{}
	_ = For(context.Background(), s, func(context.Context) Result { return NotYet("") })
	if len(seen) != 1 {
		t.Fatalf("a one-shot notice fired %d times", len(seen))
	}
}

// Every budget is at least what the loops it replaced waited.
func TestBudgetsNeverShorterThanBefore(t *testing.T) {
	for _, tc := range []struct {
		s        Spec
		attempts int
		total    time.Duration
	}{
		{HubReady, 0, 2 * time.Minute},
		{RuntimeBrokerOnline, 30, 29 * time.Second},
		{BrokerUnavailable, 30, 29 * time.Second},
		{ManagerPhaseSettle, 60, 59 * time.Second},
		{ManagerRunning, 15, 14 * time.Second},
		{ManagerStopped, 30, 29 * time.Second},
		{StopClaimReleased, 9, 185 * time.Second},
		{ManagerMountsVerified, 30, 29 * time.Second},
		{SessionHealed, 3, 2 * time.Second},
	} {
		if tc.s.MinAttempts < tc.attempts || tc.s.Total() < tc.total {
			t.Errorf("%s: %d attempts / %s, want at least %d / %s", tc.s.Name, tc.s.MinAttempts, tc.s.Total(), tc.attempts, tc.total)
		}
	}
	if ManagerSettle < 10*time.Second || HubStartRetryPause <= 0 {
		t.Error("settle or restart pause shortened")
	}
}

// A Spec with a time budget and no interval pauses minPause, never spins.
func TestNoIntervalDoesNotSpin(t *testing.T) {
	c := &fakeClock{}
	n := 0
	_ = For(context.Background(), withClock(Spec{Name: "x", Budget: 100 * time.Millisecond}, c), func(context.Context) Result {
		n++
		return NotYet("")
	})
	if n != 11 {
		t.Fatalf("probes = %d, want 11 (one per %s over 100ms)", n, minPause)
	}
}
