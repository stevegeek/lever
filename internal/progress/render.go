package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// renderer draws the events. Every method is called with the Reporter's
// lock held, so a renderer needs no lock of its own.
type renderer interface {
	header(text string, now time.Time)
	start(s *Step, now time.Time)
	detail(s *Step, now time.Time)
	tickStep(s *Step, now time.Time)
	done(s *Step, now time.Time, src string, tail []string)
	log(line string, now time.Time)
	finish(sum Summary, now time.Time)
	// tick is how often tickStep runs; 0 = never.
	tick() time.Duration
}

// stepName is "[3/11] scion-server" for a numbered step, else the label.
func stepName(s *Step) string {
	if s.Of > 0 {
		return fmt.Sprintf("[%d/%d] %s", s.N, s.Of, s.Label)
	}
	return s.Label
}

// marker is the fixed-width word in front of a finished step.
func marker(state string) string {
	switch state {
	case "ok":
		return "ok  "
	case "skip":
		return "skip"
	case "fail":
		return "FAIL"
	}
	return "..  "
}

// tailBlock is a failure's log tail, indented under the FAIL line.
func tailBlock(src string, tail []string) string {
	if len(tail) == 0 {
		if src == "" {
			return ""
		}
		return fmt.Sprintf("      (%s has no lines to show)\n", src)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "      last lines of %s:\n", src)
	for _, l := range tail {
		fmt.Fprintf(&b, "      | %s\n", l)
	}
	return b.String()
}

// ttyRenderer keeps one live line at the bottom of the terminal: the
// running step, its sub-stage and its elapsed time, redrawn each second.
// Finished steps, log lines and the summary are written above it.
type ttyRenderer struct {
	w     io.Writer
	width int
	cur   *Step
	drawn bool
}

func (t *ttyRenderer) tick() time.Duration { return time.Second }

func (t *ttyRenderer) clear() {
	if t.drawn {
		io.WriteString(t.w, "\r\x1b[K")
		t.drawn = false
	}
}

func (t *ttyRenderer) draw(now time.Time) {
	if t.cur == nil {
		return
	}
	s := t.cur
	line := "  ..    " + stepName(s)
	if s.detail != "" {
		line += ": " + s.detail
	}
	el := round(now.Sub(s.start))
	w := t.width
	if w <= 0 {
		w = 100
	}
	// The line must not wrap: a wrapped line cannot be cleared with \r.
	max := w - len(el) - 3
	if r := []rune(line); max > 8 && len(r) > max {
		line = string(r[:max-1]) + "…"
	}
	io.WriteString(t.w, "\r\x1b[K"+line+"  "+el)
	t.drawn = true
}

func (t *ttyRenderer) header(text string, now time.Time) {
	t.clear()
	fmt.Fprintln(t.w, text)
	t.draw(now)
}

func (t *ttyRenderer) start(s *Step, now time.Time) {
	t.cur = s
	t.clear()
	t.draw(now)
}

func (t *ttyRenderer) detail(s *Step, now time.Time) {
	if s == t.cur {
		t.draw(now)
	}
}

func (t *ttyRenderer) tickStep(s *Step, now time.Time) {
	if s == t.cur {
		t.draw(now)
	}
}

func (t *ttyRenderer) done(s *Step, now time.Time, src string, tail []string) {
	t.clear()
	if t.cur == s {
		t.cur = nil
	}
	line := fmt.Sprintf("  %s  %s", marker(s.state), stepName(s))
	if s.note != "" {
		line += " (" + s.note + ")"
	}
	fmt.Fprintf(t.w, "%s  %s\n", line, round(now.Sub(s.start)))
	if s.state == "fail" {
		io.WriteString(t.w, tailBlock(src, tail))
	}
	t.draw(now)
}

func (t *ttyRenderer) log(line string, now time.Time) {
	t.clear()
	fmt.Fprintln(t.w, line)
	t.draw(now)
}

func (t *ttyRenderer) finish(sum Summary, now time.Time) {
	t.clear()
	t.cur = nil
	if sum.Outcome == "ok" {
		fmt.Fprintln(t.w, sum.Line())
	}
}

// plainRenderer writes one timestamped line per event, for a log file, a
// pipe or a terminal that cannot redraw. A step that runs past
// heartbeatEvery says so at each multiple, with its sub-stage.
type plainRenderer struct {
	w io.Writer
}

func (p *plainRenderer) tick() time.Duration { return time.Second }

func (p *plainRenderer) line(now time.Time, format string, args ...any) {
	fmt.Fprintf(p.w, "%s %s\n", now.Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (p *plainRenderer) header(text string, now time.Time) { p.line(now, "%s", text) }

func (p *plainRenderer) start(s *Step, now time.Time) { p.line(now, "%s ...", stepName(s)) }

func (p *plainRenderer) detail(*Step, time.Time) {}

func (p *plainRenderer) tickStep(s *Step, now time.Time) {
	el := now.Sub(s.start)
	n := int(el / heartbeatEvery)
	if n <= s.beats {
		return
	}
	s.beats = n
	if s.detail != "" {
		p.line(now, "%s still running (%s): %s", stepName(s), el.Round(time.Second), s.detail)
		return
	}
	p.line(now, "%s still running (%s)", stepName(s), el.Round(time.Second))
}

func (p *plainRenderer) done(s *Step, now time.Time, src string, tail []string) {
	word := map[string]string{"ok": "ok", "skip": "skipped", "fail": "FAILED"}[s.state]
	line := fmt.Sprintf("%s %s (%s)", stepName(s), word, round(now.Sub(s.start)))
	if s.note != "" {
		line += ": " + s.note
	}
	p.line(now, "%s", line)
	if s.state == "fail" {
		io.WriteString(p.w, tailBlock(src, tail))
	}
}

func (p *plainRenderer) log(line string, now time.Time) { p.line(now, "%s", line) }

func (p *plainRenderer) finish(sum Summary, now time.Time) {
	if sum.Outcome == "ok" {
		p.line(now, "%s", sum.Line())
	}
}

// jsonRenderer writes the event stream itself (Reporter.emit calls write);
// its renderer methods add only a failure's tail.
type jsonRenderer struct {
	w io.Writer
}

func (j *jsonRenderer) write(ev event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	j.w.Write(append(b, '\n'))
}

func (j *jsonRenderer) tick() time.Duration                     { return 0 }
func (j *jsonRenderer) header(string, time.Time)                {}
func (j *jsonRenderer) start(*Step, time.Time)                  {}
func (j *jsonRenderer) detail(*Step, time.Time)                 {}
func (j *jsonRenderer) tickStep(*Step, time.Time)               {}
func (j *jsonRenderer) log(string, time.Time)                   {}
func (j *jsonRenderer) finish(Summary, time.Time)               {}
func (j *jsonRenderer) done(*Step, time.Time, string, []string) {}

// quietRenderer prints only log lines and failures.
type quietRenderer struct {
	w io.Writer
}

func (q *quietRenderer) tick() time.Duration          { return 0 }
func (q *quietRenderer) header(string, time.Time)     {}
func (q *quietRenderer) start(*Step, time.Time)       {}
func (q *quietRenderer) detail(*Step, time.Time)      {}
func (q *quietRenderer) tickStep(*Step, time.Time)    {}
func (q *quietRenderer) finish(Summary, time.Time)    {}
func (q *quietRenderer) log(line string, _ time.Time) { fmt.Fprintln(q.w, line) }
func (q *quietRenderer) done(s *Step, now time.Time, src string, tail []string) {
	if s.state != "fail" {
		return
	}
	fmt.Fprintf(q.w, "FAIL  %s  %s\n", stepName(s), round(now.Sub(s.start)))
	io.WriteString(q.w, tailBlock(src, tail))
}
