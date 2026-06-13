package orbstack

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

func scriptedMachine(f *exec.FakeRunner) {
	f.Script("orb list", exec.Result{Stdout: "lever-jail running ubuntu\n"}) // machine already exists
	f.Script("orb -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", exec.Result{Stdout: "ok\n"})
}

func TestEnsureUpIsIdempotentWhenMachineExists(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail")

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/Users/x/tree", AllowedPorts: []int{3305},
	})
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	// Must NOT call `orb create` when the machine already exists.
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "create" {
			t.Fatalf("create called though machine exists: %+v", c)
		}
	}
}

func TestEnsureUpCreatesIsolatedMachineWhenAbsent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb list", exec.Result{Stdout: "\n"}) // no machines
	f.Script("orb create --isolated", exec.Result{Stdout: "created\n"})
	f.Script("orb -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	b := New(f, "lever-jail")

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: "lever-jail", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	var sawCreate bool
	for _, c := range f.Calls {
		if c.Name == "orb" && strings.Join(c.Args, " ") == "create --isolated ubuntu lever-jail" {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Fatalf("expected `orb create --isolated ubuntu lever-jail`; calls=%+v", f.Calls)
	}
}

func TestProfileDeclaresSharedKernelAndFragile(t *testing.T) {
	p := New(exec.NewFakeRunner(), "lever-jail").Profile()
	if p.SeparateKernel || !p.VersionFragile {
		t.Fatalf("orbstack profile wrong: %+v", p)
	}
}
