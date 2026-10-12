package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/state"
)

func TestCheckLastBringup(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	if r := checkLastBringup(st, now); !r.ok || !strings.Contains(r.detail, "none recorded yet") {
		t.Fatalf("no record: %+v", r)
	}
	if err := os.MkdirAll(st.BringupLogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := progress.Summary{Command: "up", Outcome: "ok", TotalMS: 61400, At: now.Add(-2 * time.Hour),
		Slowest: []progress.StepTime{{Step: "[11/11] start-manager", MS: 24000}}}
	if err := progress.WriteLast(st.LastBringup(), sum); err != nil {
		t.Fatal(err)
	}
	r := checkLastBringup(st, now)
	if !r.ok || !strings.Contains(r.detail, "up done in 1m1s (slowest: [11/11] start-manager 24.0s) (2h ago)") ||
		!strings.Contains(r.detail, filepath.Join(st.Dir, "logs")) {
		t.Fatalf("row: %+v", r)
	}
}

func TestFileTailRedactsAndBounds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broker.out.log")
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("podman run -e TOKEN=sk-ant-oat01-abcdefghijkl x\n")
	os.WriteFile(p, []byte(b.String()), 0o600)
	got := fileTail(p, 15)
	if len(got) != 15 || strings.Contains(strings.Join(got, "\n"), "abcdefghijkl") {
		t.Fatalf("tail: %q", got)
	}
	if fileTail(filepath.Join(t.TempDir(), "missing"), 15) != nil {
		t.Fatal("missing file has a tail")
	}
}

func TestUpProbeReasonNeverEmpty(t *testing.T) {
	if upProbeReason(os.ErrNotExist) == "" {
		t.Fatal("empty reason")
	}
}
