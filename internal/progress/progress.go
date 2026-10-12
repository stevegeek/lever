// Package progress reports a bring-up (`lever apply`, `lever up`) as a list
// of steps: what runs now, how long each step took, what was skipped and
// why, and, when a step fails, the last lines of the log that explains it
// (lever#185, #186, #195).
//
// A Reporter is carried on the context (With, From), so code deep in a
// backend or an apply step can name its sub-stage without a new parameter.
// Every method is safe on a nil *Reporter and then does nothing, so code
// that runs without one (tests, other commands) needs no checks.
//
// The same events go to one renderer (a live line on a terminal, plain
// timestamped lines, or JSON lines) and, when a record path is set, to a
// JSON-lines record of the run that doctor and the e2e read back.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/termsafe"
)

// Mode selects the renderer.
type Mode string

const (
	ModeTTY   Mode = "tty"   // one live line, redrawn each second; finished steps scroll above it
	ModePlain Mode = "plain" // a timestamped line per event, plus a heartbeat for a long step
	ModeJSON  Mode = "json"  // one JSON object per event
	ModeQuiet Mode = "quiet" // no step lines; log lines and failures only
)

// ParseMode reads a --progress value or LEVER_PROGRESS. "" and "auto" mean
// "decide from the terminal" (AutoMode).
func ParseMode(s string) (Mode, bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return "", false, nil
	case "tty":
		return ModeTTY, true, nil
	case "plain":
		return ModePlain, true, nil
	case "json":
		return ModeJSON, true, nil
	case "quiet":
		return ModeQuiet, true, nil
	}
	return "", false, fmt.Errorf("progress: unknown mode %q (want auto, tty, plain, json or quiet)", s)
}

// AutoMode is tty when w is a terminal that can redraw a line, else plain.
func AutoMode(w io.Writer, isTerminal func(io.Writer) bool, getenv func(string) string) Mode {
	if getenv("CI") != "" || getenv("TERM") == "dumb" {
		return ModePlain
	}
	if isTerminal != nil && isTerminal(w) {
		return ModeTTY
	}
	return ModePlain
}

// heartbeatEvery is how often the plain renderer says a long step still
// runs, as the manager start line has since lever#168.
const heartbeatEvery = 15 * time.Second

// tailLines bounds a failure's log tail.
const tailLines = 15

// Options configure a Reporter.
type Options struct {
	Mode   Mode
	Out    io.Writer // the operator's terminal (stderr)
	Record string    // path of the JSON-lines record of this run; "" = none
	// Command is "apply" or "up": it names the run in the header and summary.
	Command string
	// Trace shows each external command as a log line (LEVER_TRACE).
	Trace bool
	// Width is the terminal's width for the live line (0 = 100).
	Width int

	Now    func() time.Time
	Ticker func(time.Duration) (<-chan time.Time, func())
}

// Reporter is one bring-up's progress. Use New, then Header, Start/Done per
// step, Log for warnings, and Finish.
type Reporter struct {
	opts  Options
	r     renderer
	start time.Time

	mu      sync.Mutex
	cur     *Step
	done    []*Step
	count   int // steps started, for [n/N]
	total   int // planned steps of the current numbered group (0 = unnumbered)
	group   string
	nInGrp  int
	record  *os.File
	recErr  error
	stopped bool

	stopTick func()
	tickDone chan struct{}
}

// New starts a Reporter. The caller must call Finish.
func New(opts Options) *Reporter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Ticker == nil {
		opts.Ticker = realTicker
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	rep := &Reporter{opts: opts, start: opts.Now()}
	switch opts.Mode {
	case ModeTTY:
		rep.r = &ttyRenderer{w: opts.Out, width: opts.Width}
	case ModeJSON:
		rep.r = &jsonRenderer{w: opts.Out}
	case ModeQuiet:
		rep.r = &quietRenderer{w: opts.Out}
	default:
		rep.r = &plainRenderer{w: opts.Out}
	}
	if opts.Record != "" {
		f, err := openRecord(opts.Record)
		if err != nil {
			rep.recErr = err
		} else {
			rep.record = f
		}
	}
	every := rep.r.tick()
	if every > 0 {
		ch, stop := opts.Ticker(every)
		rep.stopTick = stop
		rep.tickDone = make(chan struct{})
		go func() {
			defer close(rep.tickDone)
			for range ch {
				rep.mu.Lock()
				if rep.stopped {
					rep.mu.Unlock()
					return
				}
				if rep.cur != nil {
					rep.r.tickStep(rep.cur, rep.opts.Now())
				}
				rep.mu.Unlock()
			}
		}()
	}
	return rep
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	ch := make(chan time.Time)
	stop := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			select {
			case <-stop:
				return
			case v := <-t.C:
				select {
				case ch <- v:
				case <-stop:
					return
				}
			}
		}
	}()
	var once sync.Once
	return ch, func() { once.Do(func() { t.Stop(); close(stop) }) }
}

type ctxKey struct{}

// With returns ctx carrying rep.
func With(ctx context.Context, rep *Reporter) context.Context {
	return context.WithValue(ctx, ctxKey{}, rep)
}

// From returns the Reporter on ctx, or nil (whose methods do nothing).
func From(ctx context.Context) *Reporter {
	if ctx == nil {
		return nil
	}
	rep, _ := ctx.Value(ctxKey{}).(*Reporter)
	return rep
}

// Active reports whether rep shows step lines itself, so a caller with its
// own periodic line (the manager start's, lever#168) can stay quiet.
func (rep *Reporter) Active() bool {
	return rep != nil && rep.opts.Mode != ModeQuiet
}

// Header prints the run's first line, before any step: what runs, on what.
func (rep *Reporter) Header(text string) {
	if rep == nil {
		return
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	rep.emit(event{Event: "header", Text: text})
	rep.r.header(text, rep.opts.Now())
}

// Group starts a group of steps. total > 0 numbers its steps [n/total];
// 0 leaves them unnumbered (the jail phases, whose count depends on what
// the backend finds).
func (rep *Reporter) Group(name string, total int) {
	if rep == nil {
		return
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	rep.group, rep.total, rep.nInGrp = name, total, 0
}

// Step is one running step. Its methods are safe on nil.
type Step struct {
	rep    *Reporter
	Group  string
	Label  string
	N, Of  int
	start  time.Time
	end    time.Time
	detail string
	note   string
	state  string // "run", "ok", "skip", "fail"
	tail   func() (string, []string)
	beats  int
}

// Start begins a step labelled label in the current group. A step still
// running is ended as done first (steps do not nest).
func (rep *Reporter) Start(label string) *Step {
	if rep == nil {
		return nil
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.cur != nil {
		rep.finishLocked(rep.cur, "ok", "", nil, "", nil)
	}
	rep.count++
	s := &Step{rep: rep, Group: rep.group, Label: termsafe.Sanitize(label), start: rep.opts.Now(), state: "run"}
	if rep.total > 0 {
		rep.nInGrp++
		s.N, s.Of = rep.nInGrp, rep.total
	}
	rep.cur = s
	rep.emit(event{Event: "start", Group: s.Group, Step: s.Label, N: s.N, Of: s.Of})
	rep.r.start(s, s.start)
	return s
}

// Detail names what the step waits for now ("hub not answering yet"). It
// shows on the live line and in the plain heartbeat.
func (s *Step) Detail(text string) {
	if s == nil {
		return
	}
	s.rep.mu.Lock()
	defer s.rep.mu.Unlock()
	text = oneLine(text)
	if text == s.detail {
		return
	}
	s.detail = text
	if s.state == "run" {
		s.rep.emit(event{Event: "detail", Group: s.Group, Step: s.Label, Text: text})
		s.rep.r.detail(s, s.rep.opts.Now())
	}
}

// Note adds a short remark shown on the finished line ("unchanged",
// "cached", "already loaded").
func (s *Step) Note(text string) {
	if s == nil {
		return
	}
	s.rep.mu.Lock()
	s.note = oneLine(text)
	s.rep.mu.Unlock()
}

// TailFrom names where a failure of this step is explained: fn returns the
// source's name (a path, or "podman logs <container>") and its last lines.
// It runs only when the step fails.
func (s *Step) TailFrom(fn func() (string, []string)) {
	if s == nil {
		return
	}
	s.rep.mu.Lock()
	s.tail = fn
	s.rep.mu.Unlock()
}

// Done ends the step: ok when err is nil, failed otherwise. A failure
// prints the step, its time and the tail of its log (TailFrom); the error
// itself is left to the caller, which returns it.
func (s *Step) Done(err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.finish("fail", "", err)
		return
	}
	s.finish("ok", "", nil)
}

// Skip ends the step as skipped, with the reason.
func (s *Step) Skip(reason string) {
	if s == nil {
		return
	}
	s.finish("skip", reason, nil)
}

func (s *Step) finish(state, reason string, err error) {
	// The failure's log tail is read before the lock: it can take a guest
	// round trip, and the live line keeps ticking meanwhile.
	var src string
	var tail []string
	if state == "fail" {
		s.rep.mu.Lock()
		fn := s.tail
		s.rep.mu.Unlock()
		if fn != nil {
			src, tail = fn()
		}
	}
	s.rep.mu.Lock()
	defer s.rep.mu.Unlock()
	s.rep.finishLocked(s, state, reason, err, src, tail)
}

func (rep *Reporter) finishLocked(s *Step, state, reason string, err error, src string, tail []string) {
	if s.state != "run" {
		return
	}
	s.state = state
	s.end = rep.opts.Now()
	if reason != "" {
		s.note = oneLine(reason)
	}
	if rep.cur == s {
		rep.cur = nil
	}
	rep.done = append(rep.done, s)
	ev := event{Event: "done", Group: s.Group, Step: s.Label, N: s.N, Of: s.Of,
		MS: s.end.Sub(s.start).Milliseconds(), Outcome: state, Text: s.note}
	if err != nil {
		ev.Error = oneLine(err.Error())
	}
	src, tail = termsafe.Sanitize(src), sanitizeTail(tail)
	if state == "fail" {
		ev.Source, ev.Tail = src, tail
	}
	rep.emit(ev)
	rep.r.done(s, s.end, src, tail)
}

// Log prints a line (a warning, a notice) above the live line. Lines are
// sanitized for the terminal.
func (rep *Reporter) Log(line string) {
	if rep == nil {
		return
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	line = termsafe.Sanitize(line)
	rep.emit(event{Event: "log", Text: line})
	rep.r.log(line, rep.opts.Now())
}

// Command records one external command (LEVER_TRACE). argv is reduced to a
// safe head by the caller (proc.TraceHead).
func (rep *Reporter) Command(head string, d time.Duration, code int, err error) {
	if rep == nil || !rep.opts.Trace {
		return
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	ev := event{Event: "cmd", Text: head, MS: d.Milliseconds(), Code: code}
	if err != nil && code == 0 {
		ev.Error = oneLine(err.Error())
	}
	rep.emit(ev)
	rep.r.log(fmt.Sprintf("trace: %s (%s, exit %d)", head, round(d), code), rep.opts.Now())
}

// Finish ends the run: a step left running ends with err's outcome, the
// summary line is printed (on success), and the record is closed. It
// returns the summary for the caller's own use.
func (rep *Reporter) Finish(err error) Summary {
	if rep == nil {
		return Summary{}
	}
	rep.mu.Lock()
	cur := rep.cur
	rep.mu.Unlock()
	if cur != nil {
		if err != nil {
			cur.finish("fail", "", err)
		} else {
			cur.finish("ok", "", nil)
		}
	}
	rep.mu.Lock()
	sum := rep.summaryLocked(err)
	ev := event{Event: "finish", MS: sum.Total.Milliseconds(), Outcome: sum.Outcome, Text: sum.Line()}
	rep.emit(ev)
	rep.r.finish(sum, rep.opts.Now())
	rep.stopped = true
	if rep.record != nil {
		rep.record.Close()
		rep.record = nil
	}
	rep.mu.Unlock()
	if rep.stopTick != nil {
		rep.stopTick()
		<-rep.tickDone
	}
	return sum
}

// RecordError reports why the run record could not be opened, if it could not.
func (rep *Reporter) RecordError() error {
	if rep == nil {
		return nil
	}
	return rep.recErr
}

// Summary is a finished run.
type Summary struct {
	Command string        `json:"command"`
	Outcome string        `json:"outcome"` // ok or fail
	Total   time.Duration `json:"-"`
	TotalMS int64         `json:"total_ms"`
	Slowest []StepTime    `json:"slowest"`
	Steps   []StepTime    `json:"steps"`
	At      time.Time     `json:"at"`
}

// StepTime is one finished step.
type StepTime struct {
	Group   string `json:"group,omitempty"`
	Step    string `json:"step"`
	MS      int64  `json:"ms"`
	Outcome string `json:"outcome"`
}

// Line is the summary as the operator reads it.
func (s Summary) Line() string {
	verb := s.Command
	if verb == "" {
		verb = "bring-up"
	}
	if s.Outcome == "fail" {
		return fmt.Sprintf("%s failed after %s", verb, round(s.Total))
	}
	if len(s.Slowest) == 0 {
		return fmt.Sprintf("%s done in %s", verb, round(s.Total))
	}
	parts := make([]string, 0, len(s.Slowest))
	for _, st := range s.Slowest {
		parts = append(parts, fmt.Sprintf("%s %s", st.Step, round(time.Duration(st.MS)*time.Millisecond)))
	}
	return fmt.Sprintf("%s done in %s (slowest: %s)", verb, round(s.Total), strings.Join(parts, ", "))
}

func (rep *Reporter) summaryLocked(err error) Summary {
	sum := Summary{Command: rep.opts.Command, Outcome: "ok", Total: rep.opts.Now().Sub(rep.start), At: rep.start}
	if err != nil {
		sum.Outcome = "fail"
	}
	sum.TotalMS = sum.Total.Milliseconds()
	for _, s := range rep.done {
		sum.Steps = append(sum.Steps, StepTime{Group: s.Group, Step: s.Label, MS: s.end.Sub(s.start).Milliseconds(), Outcome: s.state})
	}
	slow := append([]StepTime(nil), sum.Steps...)
	sort.SliceStable(slow, func(i, j int) bool { return slow[i].MS > slow[j].MS })
	for _, st := range slow {
		if len(sum.Slowest) == 2 || st.MS < 1000 {
			break
		}
		sum.Slowest = append(sum.Slowest, st)
	}
	return sum
}

// event is one line of the JSON renderer and of the run record.
type event struct {
	TS      string   `json:"ts"`
	Event   string   `json:"event"`
	Group   string   `json:"group,omitempty"`
	Step    string   `json:"step,omitempty"`
	N       int      `json:"n,omitempty"`
	Of      int      `json:"of,omitempty"`
	MS      int64    `json:"ms,omitempty"`
	Outcome string   `json:"outcome,omitempty"`
	Code    int      `json:"code,omitempty"`
	Text    string   `json:"text,omitempty"`
	Error   string   `json:"error,omitempty"`
	Source  string   `json:"source,omitempty"`
	Tail    []string `json:"tail,omitempty"`
}

func (rep *Reporter) emit(ev event) {
	ev.TS = rep.opts.Now().UTC().Format(time.RFC3339Nano)
	if j, ok := rep.r.(*jsonRenderer); ok {
		j.write(ev)
	}
	if rep.record == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := rep.record.Write(append(b, '\n')); err != nil {
		rep.recErr = err
		rep.record.Close()
		rep.record = nil
	}
}

func openRecord(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("progress record: %w", err)
	}
	return f, nil
}

// oneLine keeps the first line of s, sanitized and trimmed.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return termsafe.Sanitize(strings.TrimSpace(s))
}

func sanitizeTail(lines []string) []string {
	var out []string
	for _, l := range lines {
		l = strings.TrimRight(termsafe.Sanitize(l), " \t\r")
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	if len(out) > tailLines {
		out = out[len(out)-tailLines:]
	}
	return out
}

// round formats d for a person: tenths of a second under a minute, whole
// seconds above.
func round(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// ErrNoRecord is returned by ReadLast when no run was recorded.
var ErrNoRecord = errors.New("no bring-up recorded yet")

// Current is the running step, or nil.
func (rep *Reporter) Current() *Step {
	if rep == nil {
		return nil
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	return rep.cur
}

// Detail sets the running step's sub-stage (Step.Detail), for code that
// does not hold the step: progress.From(ctx).Detail("hub not answering yet").
func (rep *Reporter) Detail(text string) { rep.Current().Detail(text) }

// Note adds a remark to the running step (Step.Note).
func (rep *Reporter) Note(text string) { rep.Current().Note(text) }
