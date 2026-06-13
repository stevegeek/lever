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
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
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
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
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

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(f, "lever-jail")

	if err := b.ApplyEgress(context.Background(), []int{3305}); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	var sawAccept, sawDrop bool
	for _, c := range f.Calls {
		j := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(j, "iptables") && strings.Contains(j, "--dport 3305") && strings.Contains(j, "ACCEPT") {
			sawAccept = true
		}
		if strings.Contains(j, "iptables") && strings.Contains(j, "0.250.250.254 -j DROP") {
			sawDrop = true
		}
	}
	if !sawAccept || !sawDrop {
		t.Fatalf("accept=%t drop=%t", sawAccept, sawDrop)
	}
}

func TestTeardownDeletesMachineWhenPresent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb list", exec.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb delete lever-jail", exec.Result{})
	if err := New(f, "lever-jail").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if f.Calls[len(f.Calls)-1].Name != "orb" || f.Calls[len(f.Calls)-1].Args[0] != "delete" {
		t.Fatalf("expected last call orb delete; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb list", exec.Result{Stdout: "\n"}) // no machines
	if err := New(f, "lever-jail").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "delete" {
			t.Fatalf("delete must NOT be called when machine absent: %+v", f.Calls)
		}
	}
}
