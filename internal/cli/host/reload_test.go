package host

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// reload runs apply's pre-bring-up checks before it stops the broker: a
// contact login the contact gate refuses (here: no agent has lever's skills
// yet) is refused by reload as by apply, and the running broker is left
// alone (its pid file, garbage here, is still there).
func TestReloadRefusesWhereApplyRefusesOnTheContactGate(t *testing.T) {
	dir := writeInstance(t, remoteBase+grantsUsers)
	p := filepath.Join(dir, config.CanonicalName)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pidPath := st.PID()
	if err := os.WriteFile(pidPath, []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	bf := func(string, string) (backend.Backend, error) {
		t.Error("the backend was built: the contact gate did not stop the command")
		return nil, errors.New("unreachable")
	}
	for name, cmd := range map[string]*cobra.Command{"apply": newApplyCmd(bf), "reload": newReloadCmd(bf)} {
		cmd.SetArgs([]string{p})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SilenceErrors = true
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "contact logins need") {
			t.Fatalf("%s: err = %v, want the contact gate's refusal", name, err)
		}
		if _, statErr := os.Stat(pidPath); statErr != nil {
			t.Fatalf("%s: the broker was stopped before the refusal (pid file: %v)", name, statErr)
		}
	}
}

// reload restates the remote-exposure warnings, as apply and up do.
func TestReloadPrintsTheRemoteWarnings(t *testing.T) {
	dir := writeInstance(t, remoteBase)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	bf := func(string, string) (backend.Backend, error) { return nil, errors.New("backend unavailable in test") }
	cmd := newReloadCmd(bf)
	cmd.SetArgs([]string{filepath.Join(dir, config.CanonicalName)})
	var stderr bytes.Buffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	_ = cmd.Execute()
	if !strings.Contains(stderr.String(), "remote.allowed_users is empty") {
		t.Fatalf("reload did not print the remote warnings; stderr:\n%s", stderr.String())
	}
}

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
