package lima

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
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
	kernel := rootBashScript(f, guest.HWEPackage("26.04"))
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
	if rootBashScript(f, guest.BootKernelFunc) < 0 {
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
		case strings.Contains(s, guest.BootKernelFunc):
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
		kernel: proc.Result{Stdout: "warn: guest_kernel hwe: installing it failed\nreboot: the guest runs kernel 6.8.0-146-generic; the next boot starts 6.17.0-5-generic\n"},
	}
	l := New(r, vm, common.Options{})
	var lines []string
	warn := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelHWE, Warn: warn}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"jail: warning: guest_kernel hwe: installing it failed",
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

// What the guest scripts report reaches the operator's terminal, so each
// line must be inert and bounded however hostile the guest is.
func TestEnsureUpGuestReportPrintsInert(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f)
	hostile := "6.8.0\x1b[2J\x1b]0;lever: all ok\x07\r✓ verified\x07 \u202egnp.exe \u009b2J" + strings.Repeat("y", 5000)
	r := fakeGuestRunner{FakeRunner: f,
		crash:  proc.Result{Stdout: "warn: " + hostile + "\n"},
		kernel: proc.Result{Stdout: "reboot: the guest runs kernel " + hostile + "\n"},
	}
	l := New(r, vm, common.Options{})
	var lines []string
	warn := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelHWE, Warn: warn}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	var seen int
	for _, line := range lines {
		if !strings.Contains(line, "6.8.0") {
			continue
		}
		seen++
		for _, c := range line {
			if c < 0x20 || c == 0x7f || (c >= 0x80 && c <= 0x9f) || c == 0x202e {
				t.Errorf("%q holds %U", line, c)
			}
		}
		if len(line) > 1000 {
			t.Errorf("line of %d bytes: %q", len(line), line)
		}
	}
	if seen != 2 {
		t.Errorf("want the warning and the reboot line, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestBriefErrInertAndBounded(t *testing.T) {
	s := briefErr(fmt.Errorf("exit 1"), proc.Result{Stderr: "level=fatal \x1b]0;t\x07\r\u202e" + strings.Repeat("z", 5000) + "\nsecond"})
	if strings.ContainsAny(s, "\x1b\x07\r\n\u202e") || len([]rune(s)) > briefErrMax+1 || !strings.HasPrefix(s, "level=fatal") {
		t.Errorf("briefErr = %q", s)
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

const stampMID = "0123456789abcdef0123456789abcdef"

// kernelScriptOf returns the last kernel script the fake saw.
func kernelScriptOf(f *proc.FakeRunner) string {
	i := rootBashScript(f, guest.BootKernelFunc)
	if i < 0 {
		return ""
	}
	return f.Calls[i].Args[len(f.Calls[i].Args)-1]
}

func hardenWithKernelOut(t *testing.T, out string, cfg backend.Config) (*proc.FakeRunner, []string) {
	t.Helper()
	f := proc.NewFakeRunner()
	scriptedVM(f)
	r := fakeGuestRunner{FakeRunner: f, kernel: proc.Result{Stdout: out}}
	l := New(r, vm, common.Options{})
	var lines []string
	cfg.MachineName, cfg.ProjectTree, cfg.GuestKernel = vm, tree, guest.GuestKernelHWE
	cfg.Warn = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := l.EnsureUp(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	return f, lines
}

func TestHWEFailedInstallWritesStamp(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "state", "hwe-install-failed.json")
	hardenWithKernelOut(t, "machine-id: "+stampMID+"\nhwe: failed\nwarn: guest_kernel hwe: installing it failed\n", backend.Config{HWEFailStamp: stamp})
	fi, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode %v, want 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(stamp)
	if !strings.Contains(string(b), stampMID) {
		t.Errorf("stamp = %s", b)
	}
}

func writeStamp(t *testing.T) string {
	t.Helper()
	stamp := filepath.Join(t.TempDir(), "hwe-install-failed.json")
	if err := os.WriteFile(stamp, []byte(`{"machine_id":"`+stampMID+`","failed_at":"2026-10-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestHWEStampSkipsUnderClosedEgress(t *testing.T) {
	stamp := writeStamp(t)
	f, lines := hardenWithKernelOut(t, "machine-id: "+stampMID+"\nhwe-package: linux-generic-hwe-26.04\nhwe: skipped-stamp\n", backend.Config{HWEFailStamp: stamp, ClosedInternet: true})
	if !strings.Contains(kernelScriptOf(f), stampMID) {
		t.Error("closed egress with a stamp must pass the stamped machine id to the script")
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "delete "+stamp) || !strings.Contains(all, "2026-10-01") || !strings.Contains(all, "linux-generic-hwe-26.04 is not installed") {
		t.Errorf("want the stamp warning, got:\n%s", all)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("a skip must keep the stamp: %v", err)
	}
}

func TestHWEStampIgnoredUnderOpenEgress(t *testing.T) {
	stamp := writeStamp(t)
	f, _ := hardenWithKernelOut(t, "machine-id: "+stampMID+"\nhwe: installed\n", backend.Config{HWEFailStamp: stamp})
	if strings.Contains(kernelScriptOf(f), stampMID) {
		t.Error("the open posture must try the install again")
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Errorf("a successful install must remove the stamp: %v", err)
	}
}

// A skip without the package line still names what is missing.
func TestHWEStampSkipWithoutPackageName(t *testing.T) {
	stamp := writeStamp(t)
	_, lines := hardenWithKernelOut(t, "machine-id: "+stampMID+"\nhwe: skipped-stamp\n", backend.Config{HWEFailStamp: stamp, ClosedInternet: true})
	if all := strings.Join(lines, "\n"); !strings.Contains(all, "the HWE kernel package is not installed") {
		t.Errorf("want the generic package name, got:\n%s", all)
	}
}

// A running VM is never started, so the crash-recovery step is what brings
// no-kvmapf to a guest created before it: it must run on that path too.
func TestEnsureUpRunningVMStillGetsNoKVMAPF(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f) // "lever-x Running"
	l := New(f, vm, common.Options{})
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if f.Called(proc.Subcommand("limactl", "start")) {
		t.Fatal("a running VM must not be started")
	}
	if rootBashScript(f, "no-kvmapf") < 0 {
		t.Fatalf("the crash-recovery script with no-kvmapf did not run on a running VM; calls=%+v", f.Calls)
	}
}

// seqRunner answers the calls each rule matches with its results in turn,
// the last one repeating, and delegates every other call to the fake.
type seqRunner struct {
	*proc.FakeRunner
	rules []*seqRule
}

type seqRule struct {
	match   func(argv string) bool
	results []proc.Result
	n       int
}

func (r *seqRunner) on(match func(argv string) bool, results ...proc.Result) {
	r.rules = append(r.rules, &seqRule{match: match, results: results})
}

func (r *seqRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := proc.Call{Name: name, Args: args}.Argv()
	for _, rule := range r.rules {
		if rule.match(argv) {
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args})
			res := rule.results[min(rule.n, len(rule.results)-1)]
			rule.n++
			return res, nil
		}
	}
	return r.FakeRunner.Run(ctx, env, name, args...)
}

func contains(s string) func(string) bool {
	return func(argv string) bool { return strings.Contains(argv, s) }
}

const (
	stagedOut  = "reboot: the kernel command line gets no-kvmapf at the next boot\ncmdline-staged: no-kvmapf\n"
	newKernOut = "reboot: the guest runs kernel 6.8.0-146-generic; the next boot starts 6.17.0-5-generic\n"
)

// rebootRig is a stopped VM whose scoped start succeeds and whose first
// crash-recovery run reports no-kvmapf staged in grub.cfg.
func rebootRig(t *testing.T, list string) (*proc.FakeRunner, *seqRunner) {
	t.Helper()
	onLinux(t)
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, list)
	scriptRealizedConfig(f, vm, matchingRealizedConfigJSON(vm, tree))
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsV4)
	f.Script(lingerKey, proc.Result{Stdout: "yes\n"})
	f.Script(probeKey, proc.Result{Stdout: "0\n"})
	f.Script(scopedStart, proc.Result{})
	f.Script("limactl stop "+vm, proc.Result{})
	r := &seqRunner{FakeRunner: f}
	r.on(contains(guest.CrashSysctlPath), proc.Result{Stdout: stagedOut}, proc.Result{})
	r.on(contains(guest.BootKernelFunc), proc.Result{Stdout: newKernOut}, proc.Result{})
	return f, r
}

func ensureUpRig(r proc.Runner) ([]string, error) {
	var lines []string
	l := New(r, vm, common.Options{})
	err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestKernel: guest.GuestKernelHWE,
		Warn: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }})
	return lines, err
}

func callsMatching(f *proc.FakeRunner, s string) []int {
	var out []int
	for i, c := range f.Calls {
		if strings.Contains(c.Argv(), s) {
			out = append(out, i)
		}
	}
	return out
}

func TestEnsureUpStoppedVMRebootsOnceForStagedCmdline(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	stops, starts := callsMatching(f, "limactl stop"), callsMatching(f, "limactl start")
	if len(stops) != 1 || len(starts) != 2 {
		t.Fatalf("stops=%v starts=%v, want one stop and two starts", stops, starts)
	}
	for _, i := range starts {
		if f.Calls[i].Argv() != scopedStart {
			t.Errorf("start %q is not the scoped one", f.Calls[i].Argv())
		}
	}
	if !(starts[0] < stops[0] && stops[0] < starts[1]) {
		t.Errorf("order: starts=%v stop=%v", starts, stops)
	}
	// Nothing but hardening runs in the guest before the reboot.
	for i := starts[0] + 1; i < stops[0]; i++ {
		a := f.Calls[i].Argv()
		if !strings.Contains(a, guest.CrashSysctlPath) && !strings.Contains(a, guest.BootKernelFunc) {
			t.Errorf("call %q ran in the guest before the reboot", a)
		}
	}
	if p := callsMatching(f, "podman"); len(p) > 0 && p[0] < starts[1] {
		t.Errorf("podman ran before the reboot finished: %q", f.Calls[p[0]].Argv())
	}
	crash, kernel := callsMatching(f, guest.CrashSysctlPath), callsMatching(f, guest.BootKernelFunc)
	if len(crash) != 2 || len(kernel) != 2 || crash[1] < starts[1] {
		t.Fatalf("crash=%v kernel=%v: want both re-checked after the reboot", crash, kernel)
	}
	if after := f.Calls[kernel[1]].Argv(); strings.Contains(after, guest.HWEPackage("26.04")) {
		t.Error("the re-check after the reboot must not run the HWE install again")
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "jail: rebooted the guest once to apply no-kvmapf") {
		t.Errorf("missing the reboot report in:\n%s", all)
	}
	if strings.Contains(all, "reboot pending") {
		t.Errorf("nothing should stay pending (the new kernel shared the reboot):\n%s", all)
	}
}

// A guest that still lacks the parameter after the reboot is reported, not
// rebooted again.
func TestEnsureUpRebootsAtMostOnce(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.rules[0].results = []proc.Result{{Stdout: stagedOut}}
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if stops := callsMatching(f, "limactl stop"); len(stops) != 1 {
		t.Fatalf("stops=%v, want one", stops)
	}
	if all := strings.Join(lines, "\n"); !strings.Contains(all, "jail: reboot pending: the kernel command line gets no-kvmapf") {
		t.Errorf("want the pending line kept:\n%s", all)
	}
}

func TestEnsureUpRunningVMIsNeverRebooted(t *testing.T) {
	f, r := rebootRig(t, listRunning)
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if n := callsMatching(f, "limactl stop"); len(n) != 0 {
		t.Fatalf("a running VM was stopped: %v", n)
	}
	if n := callsMatching(f, "limactl start"); len(n) != 0 {
		t.Fatalf("a running VM was started: %v", n)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "jail: reboot pending: the kernel command line gets no-kvmapf at the next boot; "+RebootAdvice) || strings.Contains(all, "rebooted") {
		t.Errorf("want the pending line only:\n%s", all)
	}
}

func TestEnsureUpNothingStagedNoReboot(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.rules[0].results = []proc.Result{{Stdout: "reboot: the kernel command line gets no-kvmapf at the next boot\nwarn: update-grub failed\n"}}
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if n := callsMatching(f, "limactl stop"); len(n) != 0 {
		t.Fatalf("stopped without a staged parameter: %v", n)
	}
	if n := callsMatching(f, "limactl start"); len(n) != 1 {
		t.Fatalf("starts=%v, want one", n)
	}
	if all := strings.Join(lines, "\n"); !strings.Contains(all, "jail: reboot pending") || strings.Contains(all, "rebooted") {
		t.Errorf("want the pending lines and no reboot:\n%s", all)
	}
}

func TestEnsureUpFailedStopKeepsVMAndPendingMessage(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.on(contains("limactl stop"), proc.Result{Code: 1, Stderr: "level=fatal msg=\"stop timed out\"\n"})
	r.on(contains("limactl list --format"), proc.Result{Stdout: listStopped}, proc.Result{Stdout: listRunning})
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if n := callsMatching(f, "limactl start"); len(n) != 1 {
		t.Fatalf("starts=%v, want only the first", n)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"jail: warning: rebooting the guest to apply no-kvmapf failed: limactl stop: level=fatal",
		"jail: reboot pending: the kernel command line gets no-kvmapf at the next boot; " + RebootAdvice,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "rebooted the guest") {
		t.Errorf("no reboot happened:\n%s", all)
	}
}

func TestEnsureUpFailedRestartFailsOnce(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.on(contains("limactl start"), proc.Result{}, proc.Result{Code: 1, Stderr: "level=fatal msg=boom\n"})
	_, err := ensureUpRig(r)
	if err == nil || !strings.Contains(err.Error(), "rebooting the guest to apply no-kvmapf") || !strings.Contains(err.Error(), "lever up") {
		t.Fatalf("err = %v", err)
	}
	if stops, starts := callsMatching(f, "limactl stop"), callsMatching(f, "limactl start"); len(stops) != 1 || len(starts) != 2 {
		t.Fatalf("stops=%v starts=%v, want one of each after the first start", stops, starts)
	}
	if p := callsMatching(f, "podman"); len(p) != 0 {
		t.Errorf("provisioning ran after a failed reboot: %q", f.Calls[p[0]].Argv())
	}
}

// A stop that failed but left the VM stopped still gets its one start.
func TestEnsureUpFailedStopOnStoppedVMStartsIt(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.on(contains("limactl stop"), proc.Result{Code: 1, Stderr: "level=fatal msg=\"timed out\"\n"})
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if starts := callsMatching(f, "limactl start"); len(starts) != 2 {
		t.Fatalf("starts=%v, want two", starts)
	}
	if all := strings.Join(lines, "\n"); !strings.Contains(all, "jail: rebooted the guest once to apply no-kvmapf") {
		t.Errorf("missing the reboot report in:\n%s", all)
	}
}

// A stop that failed and left the VM in another state is not started again,
// and "rebooted" is never reported.
func TestEnsureUpFailedStopOnBrokenVMFails(t *testing.T) {
	f, r := rebootRig(t, listStopped)
	r.on(contains("limactl stop"), proc.Result{Code: 1, Stderr: "level=fatal msg=\"timed out\"\n"})
	r.on(contains("limactl list --format"), proc.Result{Stdout: listStopped}, proc.Result{Stdout: vm + " Broken\n"})
	lines, err := ensureUpRig(r)
	if err == nil || !strings.Contains(err.Error(), "limactl stop: level=fatal") || !strings.Contains(err.Error(), `"Broken"`) {
		t.Fatalf("err = %v", err)
	}
	if starts := callsMatching(f, "limactl start"); len(starts) != 1 {
		t.Fatalf("starts=%v, want only the first", starts)
	}
	if strings.Contains(strings.Join(lines, "\n"), "rebooted the guest") {
		t.Error("no reboot happened")
	}
}

// A warning of the run before the reboot is kept when the run after it no
// longer gives it.
func TestEnsureUpRebootKeepsFirstRunWarning(t *testing.T) {
	_, r := rebootRig(t, listStopped)
	r.rules[0].results = []proc.Result{{Stdout: stagedOut + "warn: update-grub failed\n"}, {}}
	lines, err := ensureUpRig(r)
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if n := strings.Count(strings.Join(lines, "\n"), "jail: warning: update-grub failed"); n != 1 {
		t.Errorf("update-grub warning printed %d times in:\n%s", n, strings.Join(lines, "\n"))
	}
}
