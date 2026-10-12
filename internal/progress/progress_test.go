package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// manualTicker delivers a tick on demand and waits until the reporter
// took it, so a test sees the redraw before it reads the output.
type manualTicker struct {
	ch chan time.Time
}

func newManual() *manualTicker { return &manualTicker{ch: make(chan time.Time)} }

func (m *manualTicker) ticker(time.Duration) (<-chan time.Time, func()) {
	var once sync.Once
	return m.ch, func() { once.Do(func() { close(m.ch) }) }
}

func (m *manualTicker) tick() {
	m.ch <- time.Time{}
	// The loop takes the next tick only after it handled this one, so the
	// second send returns once the first redraw is written. A second tick
	// at the same time draws nothing new.
	m.ch <- time.Time{}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newRep(t *testing.T, mode Mode, rec string) (*Reporter, *syncBuf, *clock, *manualTicker) {
	t.Helper()
	out := &syncBuf{}
	c := &clock{t: time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)}
	m := newManual()
	rep := New(Options{Mode: mode, Out: out, Record: rec, Command: "up", Now: c.now, Ticker: m.ticker})
	return rep, out, c, m
}

func TestNilReporterIsANoOp(t *testing.T) {
	var rep *Reporter
	rep.Header("x")
	rep.Group("g", 2)
	s := rep.Start("step")
	s.Detail("d")
	s.Note("n")
	s.TailFrom(func() (string, []string) { return "", nil })
	s.Done(errors.New("boom"))
	s.Skip("r")
	rep.Log("l")
	rep.Command("c", 0, 0, nil)
	if rep.Active() {
		t.Fatal("nil reporter is active")
	}
	if got := rep.Finish(nil); got.Outcome != "" {
		t.Fatalf("summary %+v", got)
	}
	if From(context.Background()) != nil || From(With(context.Background(), nil)) != nil {
		t.Fatal("From without a reporter is not nil")
	}
}

func TestPlainLinesHeartbeatAndSummary(t *testing.T) {
	rep, out, c, m := newRep(t, ModePlain, "")
	rep.Header("lever up: instance \"dev\" (lima)")
	rep.Group("jail", 0)
	s := rep.Start("start VM")
	c.add(14 * time.Second)
	m.tick()
	s.Detail("waiting for ssh")
	c.add(2 * time.Second)
	m.tick()
	c.add(1 * time.Second)
	m.tick() // same 15 s window: no second heartbeat
	s.Done(nil)
	rep.Group("apply", 2)
	a := rep.Start("broker-up")
	a.Note("reused")
	c.add(300 * time.Millisecond)
	a.Done(nil)
	b := rep.Start("load-image")
	b.Skip("already loaded")
	rep.Log("lever: warning: something")
	sum := rep.Finish(nil)

	want := []string{
		`13:00:00 lever up: instance "dev" (lima)`,
		`13:00:00 start VM ...`,
		`13:00:16 start VM still running (16s): waiting for ssh`,
		`13:00:17 start VM ok (17.0s)`,
		`13:00:17 [1/2] broker-up ...`,
		`13:00:17 [1/2] broker-up ok (0.3s): reused`,
		`13:00:17 [2/2] load-image ...`,
		`13:00:17 [2/2] load-image skipped (0.0s): already loaded`,
		`13:00:17 lever: warning: something`,
		`13:00:17 up done in 17.3s (slowest: start VM 17.0s)`,
	}
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plain output:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if sum.Outcome != "ok" || len(sum.Steps) != 3 || sum.Steps[2].Outcome != "skip" {
		t.Fatalf("summary %+v", sum)
	}
}

func TestFailureShowsTheSanitizedTail(t *testing.T) {
	rep, out, c, _ := newRep(t, ModePlain, "")
	rep.Group("apply", 1)
	s := rep.Start("scion-server")
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line "+string(rune('a'+i)))
	}
	lines = append(lines, "evil \x1b]0;title\x07 end", "")
	s.TailFrom(func() (string, []string) { return "/home/u/.scion/server.log", lines })
	c.add(41 * time.Second)
	s.Done(errors.New("hub did not answer"))
	sum := rep.Finish(errors.New("hub did not answer"))
	got := out.String()
	if !strings.Contains(got, "[1/1] scion-server FAILED (41.0s)") {
		t.Fatalf("no FAIL line:\n%s", got)
	}
	if !strings.Contains(got, "last lines of /home/u/.scion/server.log:") || strings.Contains(got, "line e\n") {
		t.Fatalf("tail not bounded to 15:\n%s", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("escape reached the terminal: %q", got)
	}
	if strings.Contains(got, "done in") {
		t.Fatalf("summary printed on failure:\n%s", got)
	}
	if sum.Outcome != "fail" {
		t.Fatalf("outcome %q", sum.Outcome)
	}
}

func TestFinishFailsTheRunningStep(t *testing.T) {
	rep, out, _, _ := newRep(t, ModePlain, "")
	s := rep.Start("start-manager")
	s.TailFrom(func() (string, []string) { return "podman logs x", []string{"boom"} })
	rep.Finish(errors.New("x"))
	if !strings.Contains(out.String(), "start-manager FAILED") || !strings.Contains(out.String(), "| boom") {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestTTYLiveLineIsClearedBeforeOtherOutput(t *testing.T) {
	rep, out, c, m := newRep(t, ModeTTY, "")
	s := rep.Start("start VM")
	c.add(3 * time.Second)
	m.tick()
	s.Detail("booting")
	rep.Log("jail: reboot pending: x")
	s.Done(nil)
	rep.Finish(nil)
	got := out.String()
	if !strings.Contains(got, "\r\x1b[K  ..    start VM  3.0s") {
		t.Fatalf("no live line: %q", got)
	}
	if !strings.Contains(got, "\r\x1b[Kjail: reboot pending: x\n") {
		t.Fatalf("log line not written over a cleared line: %q", got)
	}
	if !strings.Contains(got, "  ok    start VM  3.0s\n") {
		t.Fatalf("no finished line: %q", got)
	}
	if !strings.HasSuffix(got, "up done in 3.0s (slowest: start VM 3.0s)\n") {
		t.Fatalf("live line left behind: %q", got)
	}
}

func TestTTYLiveLineDoesNotWrap(t *testing.T) {
	out := &syncBuf{}
	c := &clock{t: time.Now()}
	rep := New(Options{Mode: ModeTTY, Out: out, Width: 40, Now: c.now, Ticker: newManual().ticker})
	s := rep.Start("start-manager")
	s.Detail(strings.Repeat("waiting for claude ", 10))
	line := out.String()
	line = line[strings.LastIndex(line, "\r\x1b[K")+4:]
	if n := len([]rune(line)); n > 40 {
		t.Fatalf("live line %d runes > 40: %q", n, line)
	}
	rep.Finish(nil)
}

func TestJSONAndRecord(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "run.jsonl")
	rep, out, c, _ := newRep(t, ModeJSON, rec)
	rep.Group("apply", 1)
	s := rep.Start("broker-up")
	c.add(time.Second)
	s.TailFrom(func() (string, []string) { return "broker.out.log", []string{"bind: address in use"} })
	s.Done(errors.New("broker did not start"))
	rep.Finish(errors.New("broker did not start"))

	for name, text := range map[string]string{"stdout": out.String(), "record": readFile(t, rec)} {
		var events []event
		for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
			var ev event
			if err := json.Unmarshal([]byte(l), &ev); err != nil {
				t.Fatalf("%s: bad line %q: %v", name, l, err)
			}
			events = append(events, ev)
		}
		if len(events) != 3 || events[1].Event != "done" || events[1].Outcome != "fail" || events[1].MS != 1000 ||
			events[1].Tail[0] != "bind: address in use" || events[1].Source != "broker.out.log" || events[2].Event != "finish" {
			t.Fatalf("%s events: %+v", name, events)
		}
	}
	if fi, err := os.Stat(rec); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", fi.Mode(), err)
	}
}

func TestQuietPrintsOnlyLogsAndFailures(t *testing.T) {
	rep, out, _, _ := newRep(t, ModeQuiet, "")
	rep.Header("h")
	rep.Start("a").Done(nil)
	rep.Log("lever: note")
	rep.Start("b").Done(errors.New("x"))
	rep.Finish(errors.New("x"))
	if got := out.String(); got != "h\nlever: note\nFAIL  b  0.0s\n" {
		t.Fatalf("quiet output %q", got)
	}
	if rep.Active() {
		t.Fatal("quiet reporter claims to show steps")
	}
}

func TestTraceLinesOnlyWhenEnabled(t *testing.T) {
	rep, out, _, _ := newRep(t, ModePlain, "")
	rep.Command("limactl start …(+2)", time.Second, 0, nil)
	rep.Finish(nil)
	if strings.Contains(out.String(), "trace:") {
		t.Fatal("trace line without Trace")
	}
	out2 := &syncBuf{}
	rep2 := New(Options{Mode: ModePlain, Out: out2, Trace: true, Ticker: newManual().ticker})
	rep2.Command("limactl start …(+2)", 1500*time.Millisecond, 0, nil)
	rep2.Finish(nil)
	if !strings.Contains(out2.String(), "trace: limactl start …(+2) (1.5s, exit 0)") {
		t.Fatalf("got %q", out2.String())
	}
}

func TestParseAndAutoMode(t *testing.T) {
	for in, want := range map[string]Mode{"plain": ModePlain, "JSON": ModeJSON, "tty": ModeTTY, "quiet": ModeQuiet} {
		if m, set, err := ParseMode(in); err != nil || !set || m != want {
			t.Errorf("ParseMode(%q) = %v %v %v", in, m, set, err)
		}
	}
	if _, set, err := ParseMode("auto"); set || err != nil {
		t.Error("auto is not unset")
	}
	if _, _, err := ParseMode("fancy"); err == nil {
		t.Error("unknown mode accepted")
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if AutoMode(nil, func(io.Writer) bool { return true }, env(nil)) != ModeTTY {
		t.Error("terminal is not tty")
	}
	if AutoMode(nil, func(io.Writer) bool { return true }, env(map[string]string{"CI": "1"})) != ModePlain {
		t.Error("CI is not plain")
	}
	if AutoMode(nil, func(io.Writer) bool { return false }, env(nil)) != ModePlain {
		t.Error("pipe is not plain")
	}
}

func TestRecordPathPrunesAndLastRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		p, err := RecordPath(dir, "up", base.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(p, []byte("{}\n"), 0o600)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	m, _ := filepath.Glob(filepath.Join(dir, "bringup-*.jsonl"))
	if len(m) != keepRecords {
		t.Fatalf("%d records kept, want %d", len(m), keepRecords)
	}
	last := filepath.Join(dir, "last-bringup.json")
	if _, err := ReadLast(last); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("missing last: %v", err)
	}
	sum := Summary{Command: "up", Outcome: "ok", TotalMS: 61400, At: base, Slowest: []StepTime{{Step: "start-manager", MS: 24000}}}
	if err := WriteLast(last, sum); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLast(last)
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Describe(base.Add(90 * time.Minute)); d != "up done in 1m1s (slowest: start-manager 24.0s) (1h30m ago)" {
		t.Fatalf("Describe = %q", d)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTailIsRedactedAndSkippedOnCancel(t *testing.T) {
	rep, out, _, _ := newRep(t, ModePlain, "")
	s := rep.Start("broker-up")
	s.TailFrom(func() (string, []string) {
		return "broker.out.log", []string{"podman run -e TOKEN=sk-ant-oat01-abcdefghijkl x"}
	})
	s.Done(errors.New("Authorization: Bearer abcdefghijklmnopqrstuv"))
	called := false
	c := rep.Start("scion-server")
	c.TailFrom(func() (string, []string) { called = true; return "x", []string{"y"} })
	c.Done(context.Canceled)
	rep.Finish(context.Canceled)
	if strings.Contains(out.String(), "abcdefghijkl") {
		t.Fatalf("tail not redacted:\n%s", out.String())
	}
	if called {
		t.Fatal("tail read for an interrupted run")
	}
}

func TestTTYFollowsTheWidthAndAlwaysTruncates(t *testing.T) {
	out := &syncBuf{}
	width := 60
	rep := New(Options{Mode: ModeTTY, Out: out, WidthFunc: func() int { return width }, Ticker: newManual().ticker})
	s := rep.Start("start-manager")
	width = 12
	s.Detail(strings.Repeat("x", 50))
	line := out.String()
	line = line[strings.LastIndex(line, "\r\x1b[K")+4:]
	if n := len([]rune(line)); n > 12 {
		t.Fatalf("live line %d runes > 12: %q", n, line)
	}
	rep.Finish(nil)
}

func TestRecordIsNeverOpenedThroughALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("keep"), 0o600)
	link := filepath.Join(dir, "rec.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	rep := New(Options{Mode: ModeQuiet, Record: link, Ticker: newManual().ticker})
	rep.Finish(nil)
	if rep.RecordError() == nil {
		t.Fatal("record opened through a link")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("link target changed")
	}
}
