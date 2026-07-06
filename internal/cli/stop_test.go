package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
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
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	f := leverexec.NewFakeRunner()
	f.Script("scion", leverexec.Result{Stdout: "ok"})
	sb := &stubBackend{runner: f}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
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
}

// TestStopDoesNotClearStagedState is the behavioral contrast with `destroy`:
// stop must preserve the staged bootstrap ticket + manifest so a following
// `lever up` can resume fast, without re-applying.
func TestStopDoesNotClearStagedState(t *testing.T) {
	dir := instanceDir(t, "demo")
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
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
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
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	f := leverexec.NewFakeRunner() // no scripts: any call would error loudly
	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up"), runner: f}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
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

// TestStopWithExplicitMachineDoesNotStopBroker mirrors destroy's --machine
// escape hatch: targeting an explicit machine must not touch the host broker
// for the current instance.
func TestStopWithExplicitMachineDoesNotStopBroker(t *testing.T) {
	sb := &stubBackend{}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
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
