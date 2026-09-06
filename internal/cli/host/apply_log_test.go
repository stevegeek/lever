package host

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// TestApplyLogSanitizesGuestText: apply's loud lines wrap errors that carry
// guest text (scion stderr, hub responses) via %v, so the sink strips
// terminal escapes from the formatted line before it reaches stderr.
func TestApplyLogSanitizesGuestText(t *testing.T) {
	guestErr := errors.New("scion: \x1b]0;pwned\x07\x1b[2K\x1b[1;32mall good\x1b[0m")
	want := "start-manager: resume failed (scion: all good) — starting fresh\n"

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	w := &applyWiring{cmd: cmd}
	w.log("start-manager: resume failed (%v) — starting fresh", guestErr)
	if got := stderr.String(); got != want {
		t.Fatalf("applyWiring.log wrote %q, want %q", got, want)
	}

	// The nil-logFunc fallback and applyWiring.log share this one write site.
	var direct bytes.Buffer
	logLine(&direct, "start-manager: resume failed (%v) — starting fresh", guestErr)
	if got := direct.String(); got != want {
		t.Fatalf("logLine wrote %q, want %q", got, want)
	}
}
