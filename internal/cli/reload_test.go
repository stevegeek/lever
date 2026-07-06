package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
)

// reload must stop the running broker BEFORE it does anything else, so the
// subsequent apply spawns a fresh broker on the edited config (the whole point
// of the verb). We prove the ordering: with a backend factory that errors
// immediately, the command still clears the broker pid file (StopBroker ran)
// before failing — StopBroker precedes buildApplyDeps.
func TestReloadStopsBrokerBeforeApply(t *testing.T) {
	p := writeTmpConfig(t)
	stateDir := brokerctl.StateDir(filepath.Dir(p))
	if err := os.MkdirAll(stateDir.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A garbage pid file: StopBroker parses it, finds it unusable, and removes
	// it (idempotent path). Its removal is our proof StopBroker executed.
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
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Fatalf("broker pid file still present — StopBroker did not run before apply (err=%v)", err)
	}
}
