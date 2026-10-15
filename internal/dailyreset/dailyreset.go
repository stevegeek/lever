// Package dailyreset schedules the manager's daily new conversation
// (manager.daily_reset): once a day at a local wall-clock time, the broker
// runs `lever conversation new --by daily`. The package holds the schedule,
// the host-side state file and the loop; the caller supplies the run.
//
// A day is keyed by its local date. A day's run is claimed in the state file
// before it starts, so neither a broker restart nor a second broker can start
// it again. Only an answer that changed nothing (the manager busy, another
// change to the manager holding the lock, the manager not running) reopens
// the day, for a retry within the window.
package dailyreset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/hostledger"
)

const (
	// Window is how long after the set time a day's reset may still start.
	Window = 2 * time.Hour
	// RetryEvery spaces the retries after an answer that changed nothing.
	RetryEvery = 10 * time.Minute
	// Tick is how often the loop looks at the wall clock. A timer set for
	// the due time would run on the monotonic clock, which stops while the
	// host sleeps.
	Tick = time.Minute
)

// Clock is a time of day, from manager.daily_reset.
type Clock struct{ Hour, Minute int }

// Parse reads "HH:MM" (24-hour, two digits each).
func Parse(s string) (Clock, error) {
	bad := fmt.Errorf("%q is not a 24-hour time HH:MM (for example 06:30)", s)
	if len(s) != 5 || s[2] != ':' {
		return Clock{}, bad
	}
	d := func(b byte) (int, bool) { return int(b - '0'), b >= '0' && b <= '9' }
	h1, ok1 := d(s[0])
	h2, ok2 := d(s[1])
	m1, ok3 := d(s[3])
	m2, ok4 := d(s[4])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Clock{}, bad
	}
	c := Clock{h1*10 + h2, m1*10 + m2}
	if c.Hour > 23 || c.Minute > 59 {
		return Clock{}, bad
	}
	return c, nil
}

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// date is a local calendar date.
type date struct {
	y int
	m time.Month
	d int
}

func dateOf(t time.Time) date {
	y, m, d := t.Date()
	return date{y, m, d}
}

func (d date) add(days int) date {
	return dateOf(time.Date(d.y, d.m, d.d+days, 12, 0, 0, 0, time.UTC))
}

func (d date) key() string { return fmt.Sprintf("%04d-%02d-%02d", d.y, int(d.m), d.d) }

// slot is the instant c falls on date d in loc. When the wall clock reads c
// twice that day (the clocks go back), it is the first time. When it never
// reads c (the clocks go forward over it), it is the first minute after the
// jump. A date the zone skips altogether has none (zero): time.Date would
// put it on the next day, which would then run twice.
func (c Clock) slot(d date, loc *time.Location) time.Time {
	t := time.Date(d.y, d.m, d.d, c.Hour, c.Minute, 0, 0, loc)
	var exact, after time.Time
	for k := -180; k <= 180; k++ {
		u := t.Add(time.Duration(k) * time.Minute)
		lu := u.In(loc)
		if dateOf(lu) != d {
			continue
		}
		h, m := lu.Hour(), lu.Minute()
		switch {
		case h == c.Hour && m == c.Minute:
			if exact.IsZero() {
				exact = u
			}
		case h > c.Hour || (h == c.Hour && m > c.Minute):
			if after.IsZero() {
				after = u
			}
		}
	}
	switch {
	case !exact.IsZero():
		return exact
	case !after.IsZero():
		return after
	}
	return time.Time{}
}

// Results recorded in the state file.
const (
	ResultStarted    = "started"
	ResultOK         = "ok"
	ResultFailed     = "failed"
	ResultBusy       = "busy"
	ResultLocked     = "locked"
	ResultNotRunning = "not running"
	ResultGaveUp     = "gave up"
)

// State is the host-side record of the daily reset (daily-reset.json).
type State struct {
	// Day is the local date (YYYY-MM-DD) of the last day a reset was
	// attempted for. No earlier day is ever attempted again.
	Day string `json:"day,omitempty"`
	// Done closes Day: its reset ran (or was claimed), failed, or was given
	// up. While it is false, Day waits for a retry at RetryAt.
	Done        bool      `json:"done"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	Result      string    `json:"result,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	RetryAt     time.Time `json:"retry_at,omitzero"`
	// NextDue is informational, for a reader of the file: when the loop
	// would next act, as of the last write.
	NextDue time.Time `json:"next_due,omitzero"`
}

// maxStateBytes bounds the read of the state file.
const maxStateBytes = 64 << 10

// Load reads the state file; a missing file is a zero State.
func Load(path string) (State, error) {
	var st State
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return st, err
	}
	if len(b) > maxStateBytes {
		return st, fmt.Errorf("%s is over %d bytes", path, maxStateBytes)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// Save writes the state file atomically (0600).
func Save(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// Action is what Decide asks for.
type Action int

const (
	Wait Action = iota
	// Run: start the reset for Decision.Day.
	Run
	// GiveUp: close Decision.Day, whose window ended while it waited for a
	// retry.
	GiveUp
)

// Decision is Decide's answer. End is the end of Day's window.
type Decision struct {
	Action Action
	Day    string
	End    time.Time
}

// Decide says what to do at now (in now's location, the host's local time).
// A day's reset may start from its slot until Window after it, once; a host
// that sleeps or a broker that is down through the whole window skips that
// day.
func Decide(c Clock, st State, now time.Time) Decision {
	today := dateOf(now)
	yesterday := today.add(-1)
	if !st.Done && st.Day != "" && st.Day < yesterday.key() {
		return Decision{Action: GiveUp, Day: st.Day}
	}
	for _, d := range []date{yesterday, today} {
		k := d.key()
		slot := c.slot(d, now.Location())
		end := slot.Add(Window)
		switch {
		case slot.IsZero(), k < st.Day, k == st.Day && st.Done:
			continue
		case k == st.Day:
			if !now.Before(end) {
				return Decision{Action: GiveUp, Day: k, End: end}
			}
			if now.Before(st.RetryAt) {
				return Decision{Action: Wait}
			}
			return Decision{Action: Run, Day: k, End: end}
		case now.Before(slot), !now.Before(end):
			continue
		default:
			return Decision{Action: Run, Day: k, End: end}
		}
	}
	return Decision{Action: Wait}
}

// NextDue is when the loop will next start a reset, as of now: a pending
// retry, or the slot of the next day not yet attempted whose window has not
// ended (at or before now: due at the next tick).
func NextDue(c Clock, st State, now time.Time) time.Time {
	today := dateOf(now)
	for i := -1; i <= 2; i++ {
		d := today.add(i)
		k := d.key()
		slot := c.slot(d, now.Location())
		end := slot.Add(Window)
		if slot.IsZero() || k < st.Day || (k == st.Day && st.Done) || !now.Before(end) {
			continue
		}
		if k == st.Day && slot.Before(st.RetryAt) {
			return st.RetryAt
		}
		return slot
	}
	return time.Time{}
}

// Outcome is how a run ended.
type Outcome int

const (
	OK Outcome = iota
	// Busy, Locked and NotRunning changed nothing: the day stays open for a
	// retry.
	Busy
	Locked
	NotRunning
	Failed
	// Interrupted: the loop stopped before the run ended; the run goes on in
	// its own session, and its result is in its log only.
	Interrupted
)

// Scheduler is the loop the broker runs.
type Scheduler struct {
	Clock Clock
	// Path is the state file; LockPath the flock that keeps two brokers'
	// loops apart.
	Path, LockPath string
	Now            func() time.Time
	// Reset runs `lever conversation new --by daily` and returns how it
	// ended, with a host-side detail for the state file.
	Reset func(ctx context.Context) (Outcome, string)
	Log   func(msg string)

	// lastErr keeps a lasting fault from writing the log every Tick.
	lastErr string
}

// Run steps once at once, then every Tick, until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		s.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Scheduler) log(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

func (s *Scheduler) now() time.Time { return s.Now().Round(0) }

// Step acts once on the state file. A state file it cannot read or write
// stops it: without the record it cannot keep to once a day.
func (s *Scheduler) Step(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	unlock, err := hostledger.TryLockFile(s.LockPath)
	if err != nil {
		if !errors.Is(err, hostledger.ErrLocked) {
			s.fault(fmt.Sprintf("daily reset: locking %s: %v", s.LockPath, err))
		}
		return
	}
	defer unlock()
	st, err := Load(s.Path)
	if err != nil {
		s.fault(fmt.Sprintf("daily reset: not run: reading the state file: %v", err))
		return
	}
	s.lastErr = ""
	now := s.now()
	// A day claimed past tomorrow was claimed under a clock set wrong; left
	// alone it would stop the reset until that date. Today counts as run,
	// since it may have run before the clock went wrong.
	if tomorrow := dateOf(now).add(1).key(); st.Day > tomorrow {
		s.log("daily reset: the record names %s, past tomorrow (the clock was wrong); today counts as run", st.Day)
		st.Day, st.Done, st.RetryAt = dateOf(now).key(), true, time.Time{}
		if !s.save(st, now) {
			return
		}
	}
	dec := Decide(s.Clock, st, now)
	switch dec.Action {
	case Wait:
		return
	case GiveUp:
		last := st.Result
		st.Day, st.Done, st.RetryAt = dec.Day, true, time.Time{}
		st.Result = ResultGaveUp
		st.Detail = fmt.Sprintf("the %s window ended; the last answer was %q", Window, last)
		s.save(st, now)
		s.log("daily reset for %s given up: %s", dec.Day, st.Detail)
		return
	}
	st.Day, st.Done, st.RetryAt = dec.Day, true, time.Time{}
	st.LastAttempt, st.Result, st.Detail = now, ResultStarted, ""
	if !s.save(st, now) {
		return
	}
	s.log("daily reset for %s: starting a new conversation", dec.Day)
	out, detail := s.Reset(ctx)
	now = s.now()
	st.Detail = detail
	switch out {
	case OK:
		st.Result = ResultOK
	case Busy, Locked, NotRunning:
		st.Result = map[Outcome]string{Busy: ResultBusy, Locked: ResultLocked, NotRunning: ResultNotRunning}[out]
		if retry := now.Add(RetryEvery); retry.Before(dec.End) {
			st.Done, st.RetryAt = false, retry
		} else {
			st.Detail = fmt.Sprintf("the %s window ended; the last answer was %q", Window, st.Result)
			st.Result = ResultGaveUp
		}
	case Interrupted:
		st.Detail = "the broker stopped before the run ended; its result is in the log"
	default:
		st.Result = ResultFailed
	}
	s.save(st, now)
	s.log("daily reset for %s: %s %s", dec.Day, st.Result, st.Detail)
}

func (s *Scheduler) fault(msg string) {
	if msg != s.lastErr {
		s.lastErr = msg
		s.log("%s", msg)
	}
}

func (s *Scheduler) save(st State, now time.Time) bool {
	st.NextDue = NextDue(s.Clock, st, now)
	if err := Save(s.Path, st); err != nil {
		s.log("daily reset: writing the state file: %v", err)
		return false
	}
	return true
}
