package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestEnsureNestedVirtOn(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{})
			f.Script(strings.Join(shape.userPrefix, " "), proc.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
			if err := g.EnsureNestedVirt(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if len(f.Calls) != 2 {
				t.Fatalf("want 2 calls (root, user), got %d: %+v", len(f.Calls), f.Calls)
			}
			root := f.Calls[0].Args[len(f.Calls[0].Args)-1]
			for _, want := range []string{"test -c /dev/kvm", KVMUdevRulePath, `KERNEL=="kvm", MODE="0666"`, "chmod 0666 /dev/kvm"} {
				if !strings.Contains(root, want) {
					t.Errorf("root script missing %q:\n%s", want, root)
				}
			}
			if !equalPrefix(f.Calls[0].Args, shape.rootPrefix[1:]) {
				t.Errorf("call 0 must use the root prefix %v, got %v", shape.rootPrefix, f.Calls[0].Args)
			}
			if !equalPrefix(f.Calls[1].Args, shape.userPrefix[1:]) {
				t.Errorf("call 1 must use the user prefix %v, got %v", shape.userPrefix, f.Calls[1].Args)
			}
			user := f.Calls[1].Args[len(f.Calls[1].Args)-1]
			for _, want := range []string{kvmDropInPath, `devices = ["/dev/kvm"]`} {
				if !strings.Contains(user, want) {
					t.Errorf("user script missing %q:\n%s", want, user)
				}
			}
		})
	}
}

func TestEnsureNestedVirtOffRemoves(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{})
			f.Script(strings.Join(shape.userPrefix, " "), proc.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
			if err := g.EnsureNestedVirt(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if len(f.Calls) != 2 {
				t.Fatalf("want 2 calls, got %d", len(f.Calls))
			}
			root := f.Calls[0].Args[len(f.Calls[0].Args)-1]
			user := f.Calls[1].Args[len(f.Calls[1].Args)-1]
			if !strings.Contains(root, "rm -f "+KVMUdevRulePath) || strings.Contains(root, "test -c") {
				t.Errorf("off root script must only remove the rule:\n%s", root)
			}
			if !strings.Contains(user, `rm -f "`+kvmDropInPath+`"`) {
				t.Errorf("off user script must remove the drop-in:\n%s", user)
			}
		})
	}
}

func TestEnsureNestedVirtNoKVMError(t *testing.T) {
	shape := prefixShapes("lever-x")[1]
	f := proc.NewFakeRunner()
	// FakeRunner returns a nil error for a non-zero Code (proc/runner.go:246),
	// so EnsureNestedVirt must check res.Code itself. Only the root prefix is
	// scripted: the lima-shaped root key extends the user key, and FakeRunner
	// matches keys in random map order, so scripting both is ambiguous. An
	// unscripted user call would error, so a nil result cannot hide a skip.
	f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{Code: 4, Stderr: "no /dev/kvm"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
	err := g.EnsureNestedVirt(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "lever destroy") {
		t.Fatalf("want the recreate instruction, got %v", err)
	}
}
