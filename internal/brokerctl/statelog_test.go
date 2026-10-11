package brokerctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/state"
)

// TestStateLogAppender: a hidden scion line lands in broker.log (appended,
// never truncated), and a missing state directory drops it quietly.
func TestStateLogAppender(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	if err := os.WriteFile(st.Log(), []byte("earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	StateLogAppender(st)("WARN settings file contains unrecognized keys (these will be ignored) keys=[a]")
	b, err := os.ReadFile(st.Log())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, "earlier\n") || !strings.Contains(got, "settings file contains unrecognized keys") {
		t.Fatalf("broker.log = %q", got)
	}

	StateLogAppender(state.State{Dir: filepath.Join(t.TempDir(), "absent")})("x")
}
