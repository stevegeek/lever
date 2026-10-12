package lima

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

// rootBashScript finds the index of the first root `bash -c` call whose
// script contains marker.
func rootBashScript(f *proc.FakeRunner, marker string) int {
	return f.CallIndex(func(c proc.Call) bool {
		if c.Name != "limactl" || len(c.Args) < 5 {
			return false
		}
		return c.Args[2] == "sudo" && c.Args[3] == "bash" && c.Args[4] == "-c" && strings.Contains(c.Args[len(c.Args)-1], marker)
	})
}

func TestEnsureUpHardensGuestBeforeProvision(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f)
	l := New(f, vm, common.Options{})
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelHWE}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	crash := rootBashScript(f, guest.CrashSysctlPath)
	kernel := rootBashScript(f, guest.HWEPackage)
	whoami := callIndex(f, "shell", vm, "whoami")
	if crash < 0 || kernel < 0 {
		t.Fatalf("want the crash-recovery and kernel scripts; calls=%+v", f.Calls)
	}
	if !(crash < kernel && kernel < whoami) {
		t.Errorf("order: crash=%d kernel=%d whoami(provision)=%d", crash, kernel, whoami)
	}
}

func TestEnsureUpGAKernelInstallsNothing(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f)
	l := New(f, vm, common.Options{})
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelGA}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if rootBashScript(f, "apt-get install") >= 0 {
		t.Error("ga must not install the HWE kernel")
	}
	if rootBashScript(f, "/boot/vmlinuz") < 0 {
		t.Error("ga still reports a pending reboot")
	}
}

// fakeGuestRunner answers the hardening scripts with canned stdout and
// delegates every other call to the scripted fake.
type fakeGuestRunner struct {
	*proc.FakeRunner
	crash, kernel proc.Result
}

func (r fakeGuestRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	if len(args) > 0 {
		s := args[len(args)-1]
		switch {
		case strings.Contains(s, guest.CrashSysctlPath):
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args})
			return r.crash, nil
		case strings.Contains(s, "/boot/vmlinuz"):
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args})
			return r.kernel, nil
		}
	}
	return r.FakeRunner.Run(ctx, env, name, args...)
}

func TestEnsureUpReportsRebootAndWarnings(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f)
	r := fakeGuestRunner{FakeRunner: f,
		crash:  proc.Result{Stdout: "reboot: the kernel command line gets efi_pstore.pstore_disable=1 at the next boot\n"},
		kernel: proc.Result{Stdout: "warn: guest_kernel hwe: installing it failed\nreboot: the guest runs kernel 6.8.0-146-generic; the default kernel is now 6.17.0-5-generic\n"},
	}
	l := New(r, vm, common.Options{})
	var lines []string
	warn := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelHWE, Warn: warn}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"jail: WARNING: guest_kernel hwe: installing it failed",
		"jail: reboot pending: the kernel command line gets efi_pstore.pstore_disable=1 at the next boot; " + RebootAdvice,
		"jail: reboot pending: the guest runs kernel 6.8.0-146-generic",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "--fresh") {
		t.Error("the reboot advice must never discard the conversation")
	}
}

func TestEnsureUpCrashRecoveryFailureFails(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f)
	r := fakeGuestRunner{FakeRunner: f, crash: proc.Result{Code: 1, Stderr: "read-only file system"}}
	l := New(r, vm, common.Options{})
	err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree})
	if err == nil || !strings.Contains(err.Error(), "guest crash recovery") {
		t.Fatalf("want the crash-recovery failure, got %v", err)
	}
}
