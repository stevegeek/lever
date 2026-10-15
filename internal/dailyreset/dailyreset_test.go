package dailyreset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestParse(t *testing.T) {
	for _, ok := range []string{"00:00", "06:30", "23:59"} {
		if c, err := Parse(ok); err != nil || c.String() != ok {
			t.Errorf("Parse(%q) = %v, %v", ok, c, err)
		}
	}
	for _, bad := range []string{"", "6:30", "06:3", "24:00", "12:60", "06-30", "0630", "06:30 ", " 06:30", "+6:30", "06:3x", "６6:30"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestSlotDST(t *testing.T) {
	loc := newYork(t)
	// 2026-03-08: 02:00 EST jumps to 03:00 EDT.
	got := Clock{2, 30}.slot(date{2026, time.March, 8}, loc)
	if want := time.Date(2026, time.March, 8, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("gap slot = %v, want %v (03:00 EDT)", got.UTC(), want)
	}
	// 2026-11-01: 02:00 EDT goes back to 01:00 EST; 01:30 happens twice.
	got = Clock{1, 30}.slot(date{2026, time.November, 1}, loc)
	if want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("overlap slot = %v, want %v (the first 01:30, EDT)", got.UTC(), want)
	}
	got = Clock{6, 0}.slot(date{2026, time.November, 1}, loc)
	if want := time.Date(2026, time.November, 1, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ordinary slot = %v, want %v", got.UTC(), want)
	}
}

// harness is a Scheduler on a fake clock with a scripted run.
type harness struct {
	t       *testing.T
	s       *Scheduler
	now     time.Time
	runs    []time.Time
	answers []Outcome
}

func newHarness(t *testing.T, c Clock, start time.Time) *harness {
	dir := t.TempDir()
	h := &harness{t: t, now: start}
	h.s = &Scheduler{
		Clock:    c,
		Path:     filepath.Join(dir, "daily-reset.json"),
		LockPath: filepath.Join(dir, "daily-reset.lock"),
		Now:      func() time.Time { return h.now },
		Reset: func(context.Context) (Outcome, string) {
			h.runs = append(h.runs, h.now)
			out := OK
			if len(h.answers) > 0 {
				out, h.answers = h.answers[0], h.answers[1:]
			}
			return out, ""
		},
	}
	return h
}

// advance steps every minute up to until, as the loop would.
func (h *harness) advance(until time.Time) {
	for ; !h.now.After(until); h.now = h.now.Add(time.Minute) {
		h.s.Step(context.Background())
	}
}

func (h *harness) state() State {
	st, err := Load(h.s.Path)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func at(loc *time.Location, d, hh, mm int) time.Time {
	return time.Date(2026, time.October, d, hh, mm, 0, 0, loc)
}

func TestDueOncePerDay(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 5, 0))
	h.advance(at(loc, 10, 5, 59))
	if len(h.runs) != 0 {
		t.Fatalf("ran before the time: %v", h.runs)
	}
	h.advance(at(loc, 11, 5, 59))
	if len(h.runs) != 1 || !h.runs[0].Equal(at(loc, 10, 6, 0)) {
		t.Fatalf("runs = %v, want one at 06:00 on the 10th", h.runs)
	}
	st := h.state()
	if st.Day != "2026-10-10" || !st.Done || st.Result != ResultOK || !st.NextDue.Equal(at(loc, 11, 6, 0)) {
		t.Fatalf("state = %+v", st)
	}
	h.advance(at(loc, 11, 9, 0))
	if len(h.runs) != 2 || !h.runs[1].Equal(at(loc, 11, 6, 0)) {
		t.Fatalf("runs = %v, want the 11th's at 06:00", h.runs)
	}
}

func TestBusyRetriesWithinWindowThenGivesUp(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 5, 59))
	h.answers = []Outcome{Busy, Locked, NotRunning}
	h.advance(at(loc, 10, 6, 25))
	if len(h.runs) != 3 || !h.runs[1].Equal(at(loc, 10, 6, 10)) || !h.runs[2].Equal(at(loc, 10, 6, 20)) {
		t.Fatalf("runs = %v, want 06:00, 06:10, 06:20", h.runs)
	}
	if st := h.state(); st.Done || st.Result != ResultNotRunning || !st.RetryAt.Equal(at(loc, 10, 6, 30)) {
		t.Fatalf("state = %+v", st)
	}
	h.answers = []Outcome{OK}
	h.advance(at(loc, 10, 9, 0))
	if len(h.runs) != 4 {
		t.Fatalf("runs = %v, want a fourth at 06:30 and no more", h.runs)
	}

	h = newHarness(t, Clock{6, 0}, at(loc, 10, 5, 59))
	h.answers = make([]Outcome, 100)
	for i := range h.answers {
		h.answers[i] = Busy
	}
	h.advance(at(loc, 10, 23, 0))
	// 06:00 .. 07:50: twelve tries; the one at 07:50 would retry past 08:00.
	if len(h.runs) != 12 {
		t.Fatalf("got %d runs, want 12: %v", len(h.runs), h.runs)
	}
	if st := h.state(); !st.Done || st.Result != ResultGaveUp {
		t.Fatalf("state = %+v", st)
	}
	h.answers = nil
	h.advance(at(loc, 11, 6, 0))
	if len(h.runs) != 13 {
		t.Fatalf("the next day did not run: %v", h.runs)
	}
}

func TestFailureNoRetry(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 5, 59))
	h.answers = []Outcome{Failed}
	h.advance(at(loc, 10, 12, 0))
	if len(h.runs) != 1 {
		t.Fatalf("runs = %v", h.runs)
	}
	if st := h.state(); !st.Done || st.Result != ResultFailed {
		t.Fatalf("state = %+v", st)
	}
}

func TestInterruptedIsNotRunAgain(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 5, 59))
	h.answers = []Outcome{Interrupted}
	h.advance(at(loc, 10, 7, 0))
	if st := h.state(); len(h.runs) != 1 || !st.Done || st.Result != ResultStarted {
		t.Fatalf("runs = %v, state = %+v", h.runs, st)
	}
}

// A broker that dies mid-run leaves the day claimed: its successor (or a
// second broker) never starts the day again.
func TestClaimBeforeRun(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	var seen State
	inner := &Scheduler{Clock: h.s.Clock, Path: h.s.Path, LockPath: h.s.LockPath, Now: h.s.Now,
		Reset: func(context.Context) (Outcome, string) { t.Fatal("a second scheduler ran"); return OK, "" }}
	h.s.Reset = func(ctx context.Context) (Outcome, string) {
		h.runs = append(h.runs, h.now)
		seen = h.state()
		inner.Step(ctx) // the lock is held
		return OK, ""
	}
	h.s.Step(context.Background())
	if len(h.runs) != 1 || !seen.Done || seen.Result != ResultStarted || seen.Day != "2026-10-10" {
		t.Fatalf("runs = %v, state during the run = %+v", h.runs, seen)
	}
	// A successor that finds the claim and no result.
	if err := Save(h.s.Path, seen); err != nil {
		t.Fatal(err)
	}
	h.advance(at(loc, 10, 9, 0))
	if len(h.runs) != 1 {
		t.Fatalf("claimed day ran again: %v", h.runs)
	}
}

func TestWakeAfterSleep(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 5, 0))
	h.s.Step(context.Background())
	h.now = at(loc, 10, 7, 30) // asleep through 06:00, awake within the window
	h.advance(at(loc, 10, 9, 0))
	if len(h.runs) != 1 || !h.runs[0].Equal(at(loc, 10, 7, 30)) {
		t.Fatalf("runs = %v, want one at wake", h.runs)
	}
	h.now = at(loc, 11, 8, 0) // asleep through the whole window
	h.advance(at(loc, 12, 5, 0))
	if len(h.runs) != 1 {
		t.Fatalf("a missed window ran: %v", h.runs)
	}
	h.advance(at(loc, 12, 6, 0))
	if len(h.runs) != 2 {
		t.Fatalf("the next day did not run: %v", h.runs)
	}
}

func TestClockStepsBack(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	h.advance(at(loc, 10, 6, 5))
	h.now = at(loc, 10, 6, 0) // stepped back over today's slot
	h.advance(at(loc, 10, 7, 0))
	h.now = at(loc, 9, 6, 0) // stepped back a whole day
	h.advance(at(loc, 9, 8, 0))
	if len(h.runs) != 1 {
		t.Fatalf("runs = %v, want one", h.runs)
	}
}

func TestDSTDays(t *testing.T) {
	loc := newYork(t)
	// The gap: 02:30 does not exist on 2026-03-08; the reset runs at 03:00.
	start := time.Date(2026, time.March, 8, 0, 0, 0, 0, loc)
	h := newHarness(t, Clock{2, 30}, start)
	h.advance(start.Add(6 * time.Hour))
	if len(h.runs) != 1 || h.runs[0].In(loc).Format("15:04 MST") != "03:00 EDT" {
		t.Fatalf("gap runs = %v", h.runs)
	}
	// The overlap: 01:30 happens twice on 2026-11-01; the reset runs once,
	// at the first, even when the first was busy and a retry is due.
	start = time.Date(2026, time.November, 1, 0, 0, 0, 0, loc)
	h = newHarness(t, Clock{1, 30}, start)
	h.advance(start.Add(8 * time.Hour))
	if len(h.runs) != 1 || h.runs[0].In(loc).Format("15:04 MST") != "01:30 EDT" {
		t.Fatalf("overlap runs = %v", h.runs)
	}
	// The window runs on elapsed time across the change: busy from the
	// first 01:30, retries end 2h later, at 02:30 EST.
	h = newHarness(t, Clock{1, 30}, start)
	h.answers = make([]Outcome, 100)
	for i := range h.answers {
		h.answers[i] = Busy
	}
	h.advance(start.Add(8 * time.Hour))
	last := h.runs[len(h.runs)-1]
	if len(h.runs) != 12 || last.Sub(h.runs[0]) != 110*time.Minute {
		t.Fatalf("overlap busy runs = %v", h.runs)
	}
}

// A window that crosses midnight belongs to the day it started on.
func TestWindowAcrossMidnight(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{23, 30}, at(loc, 10, 23, 0))
	h.answers = []Outcome{Busy, Busy, Busy, Busy}
	h.advance(at(loc, 11, 1, 0))
	if len(h.runs) != 5 || h.state().Day != "2026-10-10" || h.state().Result != ResultOK {
		t.Fatalf("runs = %v, state = %+v", h.runs, h.state())
	}
	h.advance(at(loc, 11, 23, 30))
	if len(h.runs) != 6 || h.state().Day != "2026-10-11" {
		t.Fatalf("runs = %v, state = %+v", h.runs, h.state())
	}
}

func TestUnreadableStateRunsNothing(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	if err := os.WriteFile(h.s.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(at(loc, 10, 9, 0))
	if len(h.runs) != 0 {
		t.Fatalf("ran without a record: %v", h.runs)
	}
}

// A retry left pending when the broker stopped is closed when it comes back
// days later, and the current day runs.
func TestStalePendingClosed(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	if err := Save(h.s.Path, State{Day: "2026-10-05", Result: ResultBusy, RetryAt: at(loc, 5, 6, 10)}); err != nil {
		t.Fatal(err)
	}
	h.advance(at(loc, 10, 6, 5))
	if len(h.runs) != 1 || h.state().Day != "2026-10-10" {
		t.Fatalf("runs = %v, state = %+v", h.runs, h.state())
	}
}

func TestStateFileMode(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	h.s.Step(context.Background())
	fi, err := os.Stat(h.s.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", fi, err)
	}
}

// A zone that skips a whole date (Samoa, 2011-12-30) has no slot that day,
// and the next day runs once.
func TestSkippedDate(t *testing.T) {
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2011, time.December, 29, 20, 0, 0, 0, loc)
	h := newHarness(t, Clock{23, 30}, start)
	h.advance(start.Add(48 * time.Hour))
	if len(h.runs) != 2 || h.runs[1].In(loc).Day() != 31 {
		t.Fatalf("runs = %v, want the 29th and the 31st", h.runs)
	}
	if d := h.runs[1].Sub(h.runs[0]); d < 20*time.Hour {
		t.Fatalf("runs %v apart", d)
	}
}

// A day claimed under a clock set far ahead does not stop the reset.
func TestFutureClaimRecovers(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	if err := Save(h.s.Path, State{Day: "2099-01-01", Done: true, Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	h.advance(at(loc, 10, 9, 0))
	if len(h.runs) != 0 || h.state().Day != "2026-10-10" {
		t.Fatalf("runs = %v, state = %+v; today counts as run", h.runs, h.state())
	}
	h.advance(at(loc, 11, 6, 0))
	if len(h.runs) != 1 {
		t.Fatalf("runs = %v, want the 11th", h.runs)
	}
}

func TestFaultLoggedOnce(t *testing.T) {
	loc := newYork(t)
	h := newHarness(t, Clock{6, 0}, at(loc, 10, 6, 0))
	var lines []string
	h.s.Log = func(m string) { lines = append(lines, m) }
	if err := os.WriteFile(h.s.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(at(loc, 10, 7, 0))
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
}
