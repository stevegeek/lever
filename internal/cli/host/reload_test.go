package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/state"
)

// reload must stop the broker before anything else so the apply spawns one on
// the edited config. With a factory that errors at once, the pid file is still
// cleared: StopBroker precedes buildApplyDeps.
func TestReloadStopsBrokerBeforeApply(t *testing.T) {
	p := writeTmpConfig(t)
	stateDir := state.ForConfig(filepath.Dir(p))
	if err := os.MkdirAll(stateDir.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A garbage pid file: StopBroker removes it, which proves it ran.
	pidPath := stateDir.PID()
	if err := os.WriteFile(pidPath, []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}

	bfErr := errors.New("backend unavailable in test")
	bf := func(string, string) (backend.Backend, error) { return nil, bfErr }

	cmd := newReloadCmd(bf)
	cmd.SetArgs([]string{p})
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("reload should surface the backend error")
	}
	if _, statErr := os.Stat(pidPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("broker pid file still present — StopBroker did not run before apply (err=%v)", err)
	}
}
