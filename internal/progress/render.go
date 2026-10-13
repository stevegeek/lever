package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// renderer methods are called with the Reporter's lock held, so a renderer
// needs no lock of its own.
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

func stepName(s *Step) string {
	if s.Of > 0 {
		return fmt.Sprintf("[%d/%d] %s", s.N, s.Of, s.Label)
	}
	return s.Label
}

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

// ttyRenderer keeps one live line at the bottom of the terminal, redrawn each
// second, and writes everything else above it. The line shows the foreground
// step (or, with none, the oldest background one) and names the others.
type ttyRenderer struct {
	w       io.Writer
	width   int
	widthFn func() int
	cur     *Step
	bg      []*Step
	drawn   bool
}

func (t *ttyRenderer) focus() (*Step, []*Step) {
	if t.cur != nil {
		return t.cur, t.bg
	}
	if len(t.bg) > 0 {
		return t.bg[0], t.bg[1:]
	}
	return nil, nil
}

func (t *ttyRenderer) tick() time.Duration { return time.Second }

func (t *ttyRenderer) clear() {
	if t.drawn {
		io.WriteString(t.w, "\r\x1b[K")
		t.drawn = false
	}
}

func (t *ttyRenderer) draw(now time.Time) {
	s, others := t.focus()
	if s == nil {
		return
	}
	line := "  ..    " + stepName(s)
	if len(others) > 0 {
		names := make([]string, 0, len(others))
		for _, o := range others {
			names = append(names, o.Label)
		}
		line += " (+ " + strings.Join(names, ", ") + ")"
	}
	if s.detail != "" {
		line += ": " + s.detail
	}
	el := round(now.Sub(s.start))
	w := t.width
	if t.widthFn != nil {
		if n := t.widthFn(); n > 0 {
			w = n
		}
	}
	if w <= 0 {
		w = 100
	}
	// The line must not wrap: a wrapped line cannot be cleared with \r.
	// Runes stand in for columns; lever's text is ASCII but for a dash.
	max := w - len(el) - 3
	if max < 1 {
		max = 1
	}
	if r := []rune(line); len(r) > max {
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
	if s.bg {
		t.bg = append(t.bg, s)
	} else {
		t.cur = s
	}
	t.clear()
	t.draw(now)
}

func (t *ttyRenderer) detail(s *Step, now time.Time) {
	if f, _ := t.focus(); s == f {
		t.draw(now)
	}
}

func (t *ttyRenderer) tickStep(s *Step, now time.Time) {
	if f, _ := t.focus(); s == f {
		t.draw(now)
	}
}

func (t *ttyRenderer) done(s *Step, now time.Time, src string, tail []string) {
	t.clear()
	if t.cur == s {
		t.cur = nil
	}
	if i := slices.Index(t.bg, s); i >= 0 {
		t.bg = slices.Delete(t.bg, i, i+1)
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
	t.cur, t.bg = nil, nil
	if sum.Outcome == "ok" {
		fmt.Fprintln(t.w, sum.Line())
	}
}

// plainRenderer writes one timestamped line per event, for a log, a pipe or
// a terminal that cannot redraw. A step running past heartbeatEvery says so
// at each multiple.
type plainRenderer struct {
	w io.Writer
}

func (p *plainRenderer) tick() time.Duration { return time.Second }

func (p *plainRenderer) line(now time.Time, format string, args ...any) {
	fmt.Fprintf(p.w, "%s %s\n", now.Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (p *plainRenderer) header(text string, now time.Time) { p.line(now, "%s", text) }

func (p *plainRenderer) start(s *Step, now time.Time) {
	if s.bg {
		p.line(now, "%s ... (alongside the next steps)", stepName(s))
		return
	}
	p.line(now, "%s ...", stepName(s))
}

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

// jsonRenderer: Reporter.emit writes the event stream; its renderer methods
// add only a failure's tail.
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

func (q *quietRenderer) tick() time.Duration { return 0 }

// The header is kept: it states the backend's containment posture.
func (q *quietRenderer) header(text string, _ time.Time) { fmt.Fprintln(q.w, text) }
func (q *quietRenderer) start(*Step, time.Time)          {}
func (q *quietRenderer) detail(*Step, time.Time)         {}
func (q *quietRenderer) tickStep(*Step, time.Time)       {}
func (q *quietRenderer) finish(Summary, time.Time)       {}
func (q *quietRenderer) log(line string, _ time.Time)    { fmt.Fprintln(q.w, line) }
func (q *quietRenderer) done(s *Step, now time.Time, src string, tail []string) {
	if s.state != "fail" {
		return
	}
	fmt.Fprintf(q.w, "FAIL  %s  %s\n", stepName(s), round(now.Sub(s.start)))
	io.WriteString(q.w, tailBlock(src, tail))
}
