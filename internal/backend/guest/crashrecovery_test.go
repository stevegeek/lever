package guest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// crashSandbox is a temp root the crash-recovery script runs against, with
// stub sysctl, modprobe and update-grub that log their argv.
type crashSandbox struct {
	t     *testing.T
	paths crashPaths
	bin   string
	log   string
}

func newCrashSandbox(t *testing.T) *crashSandbox {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	s := &crashSandbox{t: t, bin: filepath.Join(root, "bin"), log: filepath.Join(root, "calls.log")}
	s.paths = crashPaths{
		Sysctl:     filepath.Join(root, "etc/sysctl.d/99-lever-crash-recovery.conf"),
		Modprobe:   filepath.Join(root, "etc/modprobe.d/lever-no-efi-pstore.conf"),
		GrubDropIn: filepath.Join(root, "etc/default/grub.d/99-lever-crash-recovery.cfg"),
		GrubCfg:    filepath.Join(root, "boot/grub/grub.cfg"),
		Cmdline:    filepath.Join(root, "proc/cmdline"),
		SysModule:  filepath.Join(root, "sys/module"),
		Core: corePaths{
			Handler:  filepath.Join(root, "usr/local/libexec/lever-core-handler"),
			Sysctl:   filepath.Join(root, "etc/sysctl.d/99-lever-core.conf"),
			Dir:      filepath.Join(root, "var/lib/lever/cores"),
			OwnerUID: os.Getuid(),
		},
	}
	for _, d := range []string{s.bin, filepath.Dir(s.paths.GrubCfg), filepath.Dir(s.paths.Cmdline), s.paths.SysModule} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// update-grub regenerates grub.cfg from the drop-in, as the real one does.
	s.stub("sysctl", `echo "sysctl $*" >> `+shellSingleQuote(s.log))
	s.stub("systemctl", systemctlStub(s.log, filepath.Join(root, "apport.masked")))
	s.stub("modprobe", `echo "modprobe $*" >> `+shellSingleQuote(s.log)+`; rm -rf `+shellSingleQuote(filepath.Join(s.paths.SysModule, "efi_pstore")))
	s.stub("update-grub", `echo "update-grub" >> `+shellSingleQuote(s.log)+`; sed -n 's/^GRUB_CMDLINE_LINUX=.*:-} \(.*\)"$/\1/p' `+shellSingleQuote(s.paths.GrubDropIn)+` > `+shellSingleQuote(s.paths.GrubCfg))
	s.setArch("x86_64")
	s.write(s.paths.Cmdline, "BOOT_IMAGE=/vmlinuz root=LABEL=cloudimg-rootfs ro console=ttyS0\n")
	return s
}

func (s *crashSandbox) stub(name, body string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

// setArch fakes the guest architecture: the sandbox host's own would
// decide the drop-in otherwise. "" makes uname -m fail.
func (s *crashSandbox) setArch(arch string) {
	s.t.Helper()
	if arch == "" {
		s.stub("uname", "exit 1")
		return
	}
	s.stub("uname", `if [ "$1" = -m ]; then echo `+shellSingleQuote(arch)+`; else exit 1; fi`)
}

func (s *crashSandbox) write(path, body string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *crashSandbox) run() (GuestReport, string) {
	s.t.Helper()
	_ = os.Remove(s.log)
	cmd := exec.Command("bash", "-c", crashRecoveryScript(s.paths))
	cmd.Env = []string{"PATH=" + s.bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("script failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(s.log)
	return parseGuestReport(string(out)), string(calls)
}

func TestCrashRecoveryScriptFirstRunWritesEverything(t *testing.T) {
	s := newCrashSandbox(t)
	// A loaded efi_pstore module registered as pstore's backend.
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/initstate"), "live\n")
	s.write(filepath.Join(s.paths.SysModule, "pstore/parameters/backend"), "efi_pstore\n")
	rep, calls := s.run()

	sysctl, _ := os.ReadFile(s.paths.Sysctl)
	for _, want := range []string{"kernel.panic_on_oops = 1", "kernel.panic = 10", "kernel.softlockup_panic = 1", "kernel.hung_task_panic = 0"} {
		if !strings.Contains(string(sysctl), want) {
			t.Errorf("sysctl file missing %q:\n%s", want, sysctl)
		}
		kv := strings.ReplaceAll(want, " = ", "=")
		if !strings.Contains(calls, "sysctl -q -w "+kv) {
			t.Errorf("live sysctl %s not applied; calls:\n%s", kv, calls)
		}
	}
	mp, _ := os.ReadFile(s.paths.Modprobe)
	if !strings.Contains(string(mp), "blacklist efi_pstore") || !strings.Contains(string(mp), "install efi_pstore /bin/true") {
		t.Errorf("modprobe file:\n%s", mp)
	}
	if !strings.Contains(calls, "modprobe -r efi_pstore") {
		t.Errorf("a loaded efi_pstore must be unloaded; calls:\n%s", calls)
	}
	grub, _ := os.ReadFile(s.paths.GrubDropIn)
	if !strings.Contains(string(grub), `GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX:-} efi_pstore.pstore_disable=1 no-kvmapf"`) {
		t.Errorf("grub drop-in:\n%s", grub)
	}
	if !strings.Contains(calls, "update-grub") {
		t.Errorf("a new drop-in must run update-grub; calls:\n%s", calls)
	}
	// The module was unloaded, so only the command line waits for a reboot.
	if len(rep.Reboot) != 1 || rep.Reboot[0] != "the kernel command line gets efi_pstore.pstore_disable=1 no-kvmapf at the next boot" {
		t.Errorf("reboot = %q, want the command line only", rep.Reboot)
	}
	if len(rep.Warn) != 0 {
		t.Errorf("warn = %q", rep.Warn)
	}
	if want := []string{EFIPstoreCmdline, NoKVMAPFCmdline}; !slices.Equal(rep.Cmdline, want) {
		t.Errorf("cmdline = %q, want %q", rep.Cmdline, want)
	}
}

func TestCrashRecoveryScriptIdempotent(t *testing.T) {
	s := newCrashSandbox(t)
	s.run()
	// After a reboot the command line carries the parameter.
	s.write(s.paths.Cmdline, "BOOT_IMAGE=/vmlinuz ro console=ttyS0 efi_pstore.pstore_disable=1 no-kvmapf\n")
	before := map[string]os.FileInfo{}
	for _, p := range []string{s.paths.Sysctl, s.paths.Modprobe, s.paths.GrubDropIn} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = fi
	}
	rep, calls := s.run()
	if strings.Contains(calls, "update-grub") {
		t.Errorf("an unchanged drop-in with grub.cfg current must not run update-grub; calls:\n%s", calls)
	}
	if strings.Contains(calls, "modprobe") {
		t.Errorf("no efi_pstore module: no unload; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "sysctl -q -w kernel.panic=10") {
		t.Errorf("the live sysctls apply on every run; calls:\n%s", calls)
	}
	for p, fi := range before {
		now, _ := os.Stat(p)
		if !os.SameFile(fi, now) {
			t.Errorf("%s rewritten although unchanged", p)
		}
	}
	if len(rep.Reboot) != 0 || len(rep.Warn) != 0 {
		t.Errorf("a converged guest reports nothing, got %+v", rep)
	}
}

// A write that fails must fail the script, though converge runs inside a
// || list where set -e does not reach.
func TestCrashRecoveryScriptFailedWriteFails(t *testing.T) {
	for _, which := range []string{"sysctl", "grub"} {
		t.Run(which, func(t *testing.T) {
			s := newCrashSandbox(t)
			blocker := filepath.Join(filepath.Dir(s.paths.Cmdline), "not-a-dir")
			s.write(blocker, "")
			if which == "sysctl" {
				s.paths.Sysctl = filepath.Join(blocker, "x.conf")
			} else {
				s.paths.GrubDropIn = filepath.Join(blocker, "x.cfg")
			}
			cmd := exec.Command("bash", "-c", crashRecoveryScript(s.paths))
			cmd.Env = []string{"PATH=" + s.bin + ":" + os.Getenv("PATH")}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a failed write must fail the script:\n%s", out)
			}
			if which == "grub" && strings.Contains(string(out), "update-grub") {
				t.Errorf("must stop before update-grub:\n%s", out)
			}
		})
	}
}

func TestCrashRecoveryScriptRewritesEditedFile(t *testing.T) {
	s := newCrashSandbox(t)
	s.write(s.paths.Sysctl, "kernel.panic = 0\n")
	s.run()
	got, _ := os.ReadFile(s.paths.Sysctl)
	if !strings.Contains(string(got), "kernel.panic = 10") || strings.Contains(string(got), "kernel.panic = 0\n") {
		t.Errorf("an edited sysctl file must be restored:\n%s", got)
	}
}

func TestCrashRecoveryScriptBuiltinBackendNeedsReboot(t *testing.T) {
	s := newCrashSandbox(t)
	// Built in: /sys/module/efi_pstore without initstate; no unload is tried.
	// pstore still names it as the backend, so setting pstore_disable after
	// registration did not turn it off.
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/parameters/pstore_disable"), "Y\n")
	s.write(filepath.Join(s.paths.SysModule, "pstore/parameters/backend"), "efi\n")
	rep, calls := s.run()
	if strings.Contains(calls, "modprobe") {
		t.Errorf("a built-in backend cannot be unloaded; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 2 || !strings.Contains(rep.Reboot[1], "EFI pstore backend is still registered") {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestCrashRecoveryScriptModuleInUseNeedsReboot(t *testing.T) {
	s := newCrashSandbox(t)
	s.stub("modprobe", `echo "modprobe $*" >> `+shellSingleQuote(s.log)+"; exit 1")
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/initstate"), "live\n")
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/parameters/pstore_disable"), "N\n")
	s.write(filepath.Join(s.paths.SysModule, "pstore/parameters/backend"), "efi_pstore\n")
	rep, calls := s.run()
	if !strings.Contains(calls, "modprobe -r efi_pstore") {
		t.Errorf("want an unload attempt; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 2 || !strings.Contains(rep.Reboot[1], "could not be unloaded") {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestCrashRecoveryScriptDisabledBackendIsOff(t *testing.T) {
	s := newCrashSandbox(t)
	s.write(s.paths.Cmdline, "ro efi_pstore.pstore_disable=1 no-kvmapf\n")
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/parameters/pstore_disable"), "Y\n")
	rep, _ := s.run()
	if len(rep.Reboot) != 0 {
		t.Errorf("a backend loaded with pstore_disable=Y is off: reboot = %q", rep.Reboot)
	}
}

// Only the x86 KVM guest code knows no-kvmapf.
func TestCrashRecoveryScriptNoKVMAPFOnX86Only(t *testing.T) {
	s := newCrashSandbox(t)
	s.setArch("aarch64")
	rep, _ := s.run()
	grub, _ := os.ReadFile(s.paths.GrubDropIn)
	if strings.Contains(string(grub), "no-kvmapf") || !strings.Contains(string(grub), `:-} efi_pstore.pstore_disable=1"`) {
		t.Errorf("an aarch64 drop-in must not carry no-kvmapf:\n%s", grub)
	}
	if len(rep.Reboot) != 1 || rep.Reboot[0] != "the kernel command line gets efi_pstore.pstore_disable=1 at the next boot" {
		t.Errorf("reboot = %q", rep.Reboot)
	}
	s.write(s.paths.Cmdline, "ro efi_pstore.pstore_disable=1\n")
	if rep, _ := s.run(); len(rep.Reboot) != 0 {
		t.Errorf("a converged aarch64 guest waits for no reboot, got %q", rep.Reboot)
	}
}

// A guest converged before no-kvmapf existed: the drop-in is rewritten,
// grub.cfg regenerated, and only the new parameter waits for a reboot.
func TestCrashRecoveryScriptAddsNoKVMAPFToConvergedGuest(t *testing.T) {
	s := newCrashSandbox(t)
	s.setArch("aarch64")
	s.run()
	s.write(s.paths.Cmdline, "ro efi_pstore.pstore_disable=1\n")
	s.setArch("x86_64")
	rep, calls := s.run()
	if !strings.Contains(calls, "update-grub") {
		t.Errorf("a changed drop-in must run update-grub; calls:\n%s", calls)
	}
	cfg, _ := os.ReadFile(s.paths.GrubCfg)
	if !strings.Contains(string(cfg), "no-kvmapf") {
		t.Errorf("grub.cfg = %q", cfg)
	}
	if len(rep.Reboot) != 1 || rep.Reboot[0] != "the kernel command line gets no-kvmapf at the next boot" {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestCrashRecoveryScriptUnknownArchKeepsDropIn(t *testing.T) {
	s := newCrashSandbox(t)
	s.run()
	before, _ := os.ReadFile(s.paths.GrubDropIn)
	s.setArch("")
	rep, _ := s.run()
	after, _ := os.ReadFile(s.paths.GrubDropIn)
	if string(before) != string(after) || !strings.Contains(string(after), "no-kvmapf") {
		t.Errorf("drop-in changed:\n%s\n->\n%s", before, after)
	}
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "guest architecture") {
		t.Errorf("warn = %q", rep.Warn)
	}
}

// A grub.cfg that lost a parameter (regenerated without the drop-in) is
// regenerated although the drop-in is unchanged.
func TestCrashRecoveryScriptStaleGrubCfgRunsUpdateGrub(t *testing.T) {
	s := newCrashSandbox(t)
	s.run()
	s.write(s.paths.GrubCfg, "efi_pstore.pstore_disable=1\n")
	_, calls := s.run()
	if !strings.Contains(calls, "update-grub") {
		t.Errorf("a grub.cfg without no-kvmapf must run update-grub; calls:\n%s", calls)
	}
}

// A grub.cfg without the parameters cannot be applied by a reboot, so
// none is reported as staged.
func TestCrashRecoveryScriptUpdateGrubFailureWarns(t *testing.T) {
	s := newCrashSandbox(t)
	s.stub("update-grub", "exit 1")
	rep, _ := s.run()
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "update-grub failed") {
		t.Errorf("warn = %q", rep.Warn)
	}
	if len(rep.Reboot) != 1 || len(rep.Cmdline) != 0 {
		t.Errorf("reboot = %q, cmdline = %q; want the pending line and nothing staged", rep.Reboot, rep.Cmdline)
	}
}

func TestCrashRecoveryScriptBootedWithParamsStagesNothing(t *testing.T) {
	s := newCrashSandbox(t)
	s.run()
	s.write(s.paths.Cmdline, "ro efi_pstore.pstore_disable=1 no-kvmapf\n")
	if rep, _ := s.run(); len(rep.Cmdline) != 0 || len(rep.Reboot) != 0 {
		t.Errorf("report = %+v", rep)
	}
}

// The guest's output reaches host messages and decides a reboot: only
// lever's own parameters count.
func TestParseGuestReportCmdlineKnownParamsOnly(t *testing.T) {
	rep := parseGuestReport("cmdline-staged: no-kvmapf \x1b[2Jevil no-kvmapf init=/bin/sh efi_pstore.pstore_disable=1\n")
	if want := []string{NoKVMAPFCmdline, EFIPstoreCmdline}; !slices.Equal(rep.Cmdline, want) {
		t.Errorf("cmdline = %q, want %q", rep.Cmdline, want)
	}
}

func TestEnsureCrashRecoveryUsesRootAndParses(t *testing.T) {
	shape := prefixShapes("lever-x")[1]
	f := proc.NewFakeRunner()
	f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{Stdout: "reboot: a\nwarn: b\nnoise\n"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
	rep, err := g.EnsureCrashRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || !equalPrefix(f.Calls[0].Args, append(append([]string{}, shape.rootPrefix[1:]...), "bash", "-c")) {
		t.Fatalf("want one root bash call, got %+v", f.Calls)
	}
	script := f.Calls[0].Args[len(f.Calls[0].Args)-1]
	for _, want := range []string{CrashSysctlPath, CrashModprobePath, CrashGrubPath, "/proc/cmdline"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	if strings.Contains(script, "reboot\n") || strings.Contains(script, "systemctl reboot") || strings.Contains(script, "shutdown") {
		t.Error("the script must never reboot the guest")
	}
	if len(rep.Reboot) != 1 || rep.Reboot[0] != "a" || len(rep.Warn) != 1 || rep.Warn[0] != "b" {
		t.Errorf("report = %+v", rep)
	}
}

func TestEnsureCrashRecoveryFailure(t *testing.T) {
	shape := prefixShapes("lever-x")[1]
	f := proc.NewFakeRunner()
	f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{Code: 1, Stderr: "read-only file system"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
	if _, err := g.EnsureCrashRecovery(context.Background()); err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("want the failure, got %v", err)
	}
}

// kernelEnv is one sandboxed run of guestKernelScript.
type kernelEnv struct {
	kernel, osVersion, running, def string
	dpkgHas, aptFails               bool
	// aptLockedTimes makes the first n apt-get update calls fail on a held
	// lock; egressClosed makes iptables -S show the closed catch-all DROP.
	aptLockedTimes int
	egressClosed   bool
	machineID      string
	opts           KernelOpts
	// boot, when set, lays out /boot instead of def (bootLayout).
	boot *bootLayout
}

const testMachineID = "0123456789abcdef0123456789abcdef"

// runKernelScript runs guestKernelScript with stub dpkg-query, apt-get,
// iptables, timeout, sleep and uname.
func runKernelScript(t *testing.T, kernel, osVersion, running, def string, dpkgHas bool, aptFails bool) (GuestReport, string) {
	t.Helper()
	r, calls := runKernelEnv(t, kernelEnv{kernel: kernel, osVersion: osVersion, running: running, def: def, dpkgHas: dpkgHas, aptFails: aptFails})
	return r.GuestReport, calls
}

func runKernelEnv(t *testing.T, e kernelEnv) (KernelReport, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	log := filepath.Join(root, "calls.log")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Not installed is modelled as state rc (removed, config left), which
	// dpkg -s would have reported with exit 0.
	status := "deinstall ok config-files"
	if e.dpkgHas {
		status = "install ok installed"
	}
	aptExit := "0"
	if e.aptFails {
		aptExit = "100"
	}
	lockCount := filepath.Join(root, "lock.count")
	stub("dpkg-query", `echo "dpkg-query $*" >> `+shellSingleQuote(log)+"; printf '%s' "+shellSingleQuote(status))
	stub("apt-get", `echo "apt-get $*" >> `+shellSingleQuote(log)+`
case "$*" in *update*)
  n=$(cat `+shellSingleQuote(lockCount)+` 2>/dev/null || echo 0)
  if [ "$n" -lt `+fmt.Sprint(e.aptLockedTimes)+` ]; then
    echo $((n+1)) > `+shellSingleQuote(lockCount)+`
    echo "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1234 (apt-get)" >&2
    exit 100
  fi ;;
esac
exit `+aptExit)
	chain := "-N LEVER_EGRESS\n-A LEVER_EGRESS -d 10.0.0.0/8 -j DROP"
	if e.egressClosed {
		chain += "\n-A LEVER_EGRESS -j DROP"
	}
	stub("iptables", `echo "iptables $*" >> `+shellSingleQuote(log)+"; printf '%s\\n' "+shellSingleQuote(chain))
	stub("timeout", `shift; exec "$@"`)
	stub("sleep", `:`)
	stub("uname", "echo "+e.running)
	p := kernelPaths{OSRelease: filepath.Join(root, "os-release"), Boot: filepath.Join(root, "boot"), MachineID: filepath.Join(root, "machine-id")}
	if err := os.WriteFile(p.OSRelease, []byte(`NAME="Ubuntu"`+"\nVERSION_ID=\""+e.osVersion+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mid := e.machineID
	if mid == "" {
		mid = testMachineID
	}
	if err := os.WriteFile(p.MachineID, []byte(mid+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	switch {
	case e.boot != nil:
		e.boot.write(t, p.Boot)
	case e.def != "":
		(&bootLayout{kernels: []string{e.def}, link: e.def}).write(t, p.Boot)
	}
	cmd := exec.Command("bash", "-c", guestKernelScript(e.kernel, e.opts, p))
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(log)
	return parseKernelReport(string(out)), string(calls)
}

func TestGuestKernelHWEInstallsAndReportsReboot(t *testing.T) {
	// Before the install /boot/vmlinuz already names the new kernel in the
	// stub; the real package moves it.
	rep, calls := runKernelScript(t, GuestKernelHWE, "24.04", "6.8.0-146-generic", "6.17.0-5-generic", false, false)
	if !strings.Contains(calls, "apt-get install -y -qq -o DPkg::Lock::Timeout=120 linux-generic-hwe-24.04") || !strings.Contains(calls, "apt-get -o DPkg::Lock::Timeout=120 update -qq") {
		t.Errorf("want the HWE install; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 1 || !strings.Contains(rep.Reboot[0], "runs kernel 6.8.0-146-generic; the next boot starts 6.17.0-5-generic") {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestGuestKernelHWEAlreadyInstalledAndRunning(t *testing.T) {
	rep, calls := runKernelScript(t, GuestKernelHWE, "24.04", "6.17.0-5-generic", "6.17.0-5-generic", true, false)
	if strings.Contains(calls, "apt-get") {
		t.Errorf("an installed HWE package must not run apt; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 0 || len(rep.Warn) != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestGuestKernelHWEAptFailureWarns(t *testing.T) {
	rep, _ := runKernelScript(t, GuestKernelHWE, "24.04", "6.8.0-146-generic", "6.8.0-146-generic", false, true)
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "installing linux-generic-hwe-24.04 failed") {
		t.Errorf("warn = %q", rep.Warn)
	}
	if len(rep.Reboot) != 0 {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestGuestKernelHWEOtherReleaseSkips(t *testing.T) {
	rep, calls := runKernelScript(t, GuestKernelHWE, "25.10", "6.17.0-5-generic", "6.17.0-5-generic", false, false)
	if strings.Contains(calls, "apt-get") || strings.Contains(calls, "dpkg") {
		t.Errorf("another release must install nothing; calls:\n%s", calls)
	}
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "Ubuntu 25.10, not 24.04 or 26.04") {
		t.Errorf("warn = %q", rep.Warn)
	}
}

// Each supported release installs its own HWE metapackage and reports it.
func TestGuestKernelHWEPackagePerRelease(t *testing.T) {
	for _, rel := range []string{"24.04", "26.04"} {
		want := "linux-generic-hwe-" + rel
		r, calls := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: rel, running: "7.0.0-34-generic", def: "7.0.0-38-generic"})
		if !strings.Contains(calls, "apt-get install -y -qq -o DPkg::Lock::Timeout=120 "+want+"\n") {
			t.Errorf("%s: want the install of %s; calls:\n%s", rel, want, calls)
		}
		if !strings.Contains(calls, "dpkg-query -W -f=${Status} "+want) {
			t.Errorf("%s: want the dpkg check of %s; calls:\n%s", rel, want, calls)
		}
		if r.HWE != HWEInstalled || r.HWEPackage != want {
			t.Errorf("%s: report = %+v", rel, r)
		}
		r, _ = runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: rel, running: "7.0.0-34-generic", def: "7.0.0-34-generic", egressClosed: true})
		if len(r.Warn) != 1 || !strings.Contains(r.Warn[0], want+" is not installed, and lever did not run apt: the jail's egress is closed") {
			t.Errorf("%s: closed warn = %q", rel, r.Warn)
		}
		r, _ = runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: rel, running: "7.0.0-34-generic", def: "7.0.0-34-generic", aptFails: true})
		if len(r.Warn) != 1 || !strings.Contains(r.Warn[0], "installing "+want+" failed") {
			t.Errorf("%s: failed warn = %q", rel, r.Warn)
		}
	}
}

func TestParseKernelReportIgnoresUnknownPackage(t *testing.T) {
	r := parseKernelReport("hwe-package: linux-generic-hwe-99.04\nhwe: installed\n")
	if r.HWEPackage != "" || r.HWE != HWEInstalled {
		t.Errorf("report = %+v", r)
	}
}

func TestGuestKernelGAInstallsNothing(t *testing.T) {
	rep, calls := runKernelScript(t, GuestKernelGA, "24.04", "6.8.0-146-generic", "6.8.0-150-generic", false, false)
	if calls != "" {
		t.Errorf("ga must not touch packages; calls:\n%s", calls)
	}
	// A GA kernel update (unattended-upgrades) still waits for a reboot.
	if len(rep.Reboot) != 1 {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestGuestKernelNoVmlinuzNoReboot(t *testing.T) {
	// No kernel in /boot is no default kernel, not a different one.
	rep, _ := runKernelScript(t, GuestKernelGA, "24.04", "6.8.0-146-generic", "", false, false)
	if len(rep.Reboot) != 0 {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestEnsureGuestKernelRejectsUnknown(t *testing.T) {
	g := Guest{Host: proc.NewFakeRunner(), UserPrefix: []string{"x"}, RootPrefix: []string{"x"}}
	if _, err := g.EnsureGuestKernel(context.Background(), "mainline", KernelOpts{}); err == nil {
		t.Fatal("want a refusal")
	}
}

func TestGuestKernelHWEReportsOutcomeAndMachineID(t *testing.T) {
	r, _ := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.17.0-5-generic"})
	if r.HWE != HWEInstalled || r.MachineID != testMachineID {
		t.Errorf("hwe=%q machine-id=%q", r.HWE, r.MachineID)
	}
	r, _ = runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.8.0-146-generic", aptFails: true})
	if r.HWE != HWEFailed {
		t.Errorf("hwe=%q, want failed", r.HWE)
	}
	r, _ = runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.17.0-5-generic", def: "6.17.0-5-generic", dpkgHas: true})
	if r.HWE != HWEInstalled {
		t.Errorf("hwe=%q, want installed", r.HWE)
	}
}

// apt-get update must wait out a held lock (cloud-init, apt-daily on a
// fresh VM) rather than fail at once.
func TestGuestKernelHWEUpdateWaitsForLock(t *testing.T) {
	r, calls := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.17.0-5-generic", aptLockedTimes: 2})
	if n := strings.Count(calls, "update -qq"); n != 3 {
		t.Errorf("want 3 update attempts, got %d; calls:\n%s", n, calls)
	}
	if r.HWE != HWEInstalled || len(r.Warn) != 0 {
		t.Errorf("report = %+v", r)
	}
}

// A non-lock update failure is not retried.
func TestGuestKernelHWEUpdateFailureNotRetried(t *testing.T) {
	r, calls := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.8.0-146-generic", aptFails: true})
	if n := strings.Count(calls, "update -qq"); n != 1 {
		t.Errorf("want 1 update attempt, got %d", n)
	}
	if strings.Contains(calls, "apt-get install") {
		t.Errorf("a failed update must not run the install; calls:\n%s", calls)
	}
	if r.HWE != HWEFailed {
		t.Errorf("hwe = %q", r.HWE)
	}
}

func TestGuestKernelHWEClosedEgressSkipsApt(t *testing.T) {
	r, calls := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.8.0-146-generic", egressClosed: true})
	if strings.Contains(calls, "apt-get") {
		t.Errorf("closed egress must not run apt; calls:\n%s", calls)
	}
	if r.HWE != HWESkippedClose || len(r.Warn) != 1 || !strings.Contains(r.Warn[0], "egress is closed") {
		t.Errorf("report = %+v", r)
	}
}

func TestGuestKernelHWEStampSkipsApt(t *testing.T) {
	r, calls := runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.8.0-146-generic",
		opts: KernelOpts{SkipIfMachineID: testMachineID}})
	if strings.Contains(calls, "apt-get") || strings.Contains(calls, "iptables") {
		t.Errorf("a stamped VM must not run apt; calls:\n%s", calls)
	}
	if r.HWE != HWESkippedStamp {
		t.Errorf("hwe = %q", r.HWE)
	}
	// A stamp for another VM (a recreated one) is not this VM's.
	r, calls = runKernelEnv(t, kernelEnv{kernel: GuestKernelHWE, osVersion: "24.04", running: "6.8.0-146-generic", def: "6.17.0-5-generic",
		opts: KernelOpts{SkipIfMachineID: "ffffffffffffffffffffffffffffffff"}})
	if !strings.Contains(calls, "apt-get install") || r.HWE != HWEInstalled {
		t.Errorf("another VM's stamp must not skip; hwe=%q calls:\n%s", r.HWE, calls)
	}
}
