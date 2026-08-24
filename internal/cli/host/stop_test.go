package host

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// TestStopSuspendsManager verifies the happy path: with a reachable jail,
// `lever stop` SUSPENDS the manager (best-effort, via scion) before powering
// the machine off. It must be `scion suspend`, not `scion stop`: the
// conversation is durable (the agent home is a persistent bind-mount, and
// scion resume relaunches the harness with `claude --continue`, restoring
// the session — live-proven 2026-07-04), so suspend keeps the record
// resumable for the next `lever up`, while `scion stop` would REMOVE the
// container and discard the session.
func TestStopSuspendsManager(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	// Seed the controller PAT so the suspend client's HubTokenSource resolves it:
	// this guards that stop's scion client keeps its HubTokenSource wiring (a
	// dropped source would authenticate anonymously against the dev-auth-off hub).
	if err := state.ForConfig(dir).SaveControllerPAT("pat-stop-suspend"); err != nil {
		t.Fatal(err)
	}

	f := scionOKRunner()
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must call Backend.Stop")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("expected exactly one scion call (suspend), got %+v", f.Calls)
	}
	call := f.Calls[0]
	if call.Name != "scion" || len(call.Args) == 0 || call.Args[0] != "suspend" {
		t.Fatalf("expected `scion suspend ...`, got %+v", call)
	}
	if got := call.Env["SCION_HUB_TOKEN"]; got != "pat-stop-suspend" {
		t.Fatalf("suspend env SCION_HUB_TOKEN = %q, want %q (HubTokenSource dropped)", got, "pat-stop-suspend")
	}
}

// TestStopDoesNotClearStagedState is the behavioral contrast with `destroy`:
// stop must preserve the staged bootstrap ticket + manifest so a following
// `lever up` can resume fast, without re-applying.
func TestStopDoesNotClearStagedState(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	tree := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(tree, ".lever", "bootstrap.json")
	manifest := filepath.Join(tree, config.ManifestName)
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	// Jail unreachable: skips the suspend branch entirely, isolating this test
	// to the staged-state behavior.
	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up")}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must still call Backend.Stop")
	}
	if _, err := os.Stat(bootstrap); err != nil {
		t.Fatalf("bootstrap.json must survive `lever stop`, stat err = %v", err)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest must survive `lever stop`, stat err = %v", err)
	}
}

// TestStopSkipsSuspendWhenJailUnreachable covers the DECISION documented in
// stop.go: if ResolveRunUser fails (jail unreachable — already halted, or
// never came up), stop skips the best-effort suspend and still proceeds to
// power off, rather than failing the command.
func TestStopSkipsSuspendWhenJailUnreachable(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	f := proc.NewFakeRunner() // no scripts: any call would error loudly
	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up"), runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop must still succeed when the jail is unreachable: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must power off even when suspend is skipped")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("suspend must be skipped when ResolveRunUser errors, got calls: %+v", f.Calls)
	}
}

// TestStopAlsoStopsRemoteProxy proves `lever stop` tears the remote-access
// proxy down alongside the broker: a live pid recorded in remote.pid must be
// killed and the pid file removed (state.State.StopRemoteProxy mirrors
// StopBroker exactly — see its doc; the mechanism itself is unit-tested in
// internal/brokerctl, this only pins that stop.go actually calls it).
func TestStopAlsoStopsRemoteProxy(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := os.WriteFile(st.RemotePID(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(st.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.pid should be removed after stop, stat err = %v", err)
	}
	_ = cmd.Wait()
}

// TestStopWithExplicitMachineDoesNotStopBroker mirrors destroy's --machine
// escape hatch: targeting an explicit machine must not touch the host broker
// for the current instance.
func TestStopWithExplicitMachineDoesNotStopBroker(t *testing.T) {
	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop", "--machine", "lever-other"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must call Backend.Stop")
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("broker is not stopped")) {
		t.Fatalf("expected a note that the broker is not stopped, got: %q", got)
	}
}

// TestStopHostDaemonsIsQuietWhenNothingRuns: with no broker.pid and no
// remote.pid both stops are no-ops and nothing is warned about.
func TestStopHostDaemonsIsQuietWhenNothingRuns(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errOut)
	stopHostDaemons(cmd, state.ForConfig(t.TempDir()))
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", errOut.String())
	}
}
