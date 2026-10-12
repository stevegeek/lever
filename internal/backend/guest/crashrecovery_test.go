package guest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	}
	for _, d := range []string{s.bin, filepath.Dir(s.paths.GrubCfg), filepath.Dir(s.paths.Cmdline), s.paths.SysModule} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// update-grub regenerates grub.cfg from the drop-in, as the real one does.
	s.stub("sysctl", `echo "sysctl $*" >> `+shellSingleQuote(s.log))
	s.stub("modprobe", `echo "modprobe $*" >> `+shellSingleQuote(s.log)+`; rm -rf `+shellSingleQuote(filepath.Join(s.paths.SysModule, "efi_pstore")))
	s.stub("update-grub", `echo "update-grub" >> `+shellSingleQuote(s.log)+`; grep -o 'efi_pstore[^"]*' `+shellSingleQuote(s.paths.GrubDropIn)+` > `+shellSingleQuote(s.paths.GrubCfg))
	s.write(s.paths.Cmdline, "BOOT_IMAGE=/vmlinuz root=LABEL=cloudimg-rootfs ro console=ttyS0\n")
	return s
}

func (s *crashSandbox) stub(name, body string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		s.t.Fatal(err)
	}
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
	s.write(filepath.Join(s.paths.SysModule, "pstore/parameters/backend"), "efi\n")
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
	if !strings.Contains(string(grub), `GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX:-} efi_pstore.pstore_disable=1"`) {
		t.Errorf("grub drop-in:\n%s", grub)
	}
	if !strings.Contains(calls, "update-grub") {
		t.Errorf("a new drop-in must run update-grub; calls:\n%s", calls)
	}
	// The module was unloaded, so only the command line waits for a reboot.
	if len(rep.Reboot) != 1 || !strings.Contains(rep.Reboot[0], "kernel command line") {
		t.Errorf("reboot = %q, want the command line only", rep.Reboot)
	}
	if len(rep.Warn) != 0 {
		t.Errorf("warn = %q", rep.Warn)
	}
}

func TestCrashRecoveryScriptIdempotent(t *testing.T) {
	s := newCrashSandbox(t)
	s.run()
	// After a reboot the command line carries the parameter.
	s.write(s.paths.Cmdline, "BOOT_IMAGE=/vmlinuz ro console=ttyS0 efi_pstore.pstore_disable=1\n")
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
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/parameters/pstore_disable"), "N\n")
	s.write(filepath.Join(s.paths.SysModule, "pstore/parameters/backend"), "efi\n")
	rep, calls := s.run()
	if strings.Contains(calls, "modprobe") {
		t.Errorf("a built-in backend cannot be unloaded; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 2 || !strings.Contains(rep.Reboot[1], "EFI pstore backend is still registered") {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestCrashRecoveryScriptDisabledBackendIsOff(t *testing.T) {
	s := newCrashSandbox(t)
	s.write(s.paths.Cmdline, "ro efi_pstore.pstore_disable=1\n")
	s.write(filepath.Join(s.paths.SysModule, "efi_pstore/parameters/pstore_disable"), "Y\n")
	rep, _ := s.run()
	if len(rep.Reboot) != 0 {
		t.Errorf("a backend loaded with pstore_disable=Y is off: reboot = %q", rep.Reboot)
	}
}

func TestCrashRecoveryScriptUpdateGrubFailureWarns(t *testing.T) {
	s := newCrashSandbox(t)
	s.stub("update-grub", "exit 1")
	rep, _ := s.run()
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "update-grub failed") {
		t.Errorf("warn = %q", rep.Warn)
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

// kernelSandbox runs guestKernelScript with stub dpkg, apt-get, timeout and
// uname.
func runKernelScript(t *testing.T, kernel, osVersion, running, def string, dpkgHas bool, aptFails bool) (GuestReport, string) {
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
	dpkgExit := "1"
	if dpkgHas {
		dpkgExit = "0"
	}
	aptExit := "0"
	if aptFails {
		aptExit = "100"
	}
	stub("dpkg", `echo "dpkg $*" >> `+shellSingleQuote(log)+"; exit "+dpkgExit)
	stub("apt-get", `echo "apt-get $*" >> `+shellSingleQuote(log)+"; exit "+aptExit)
	stub("timeout", `shift; exec "$@"`)
	stub("uname", "echo "+running)
	p := kernelPaths{OSRelease: filepath.Join(root, "os-release"), Vmlinuz: filepath.Join(root, "boot/vmlinuz")}
	if err := os.WriteFile(p.OSRelease, []byte(`NAME="Ubuntu"`+"\nVERSION_ID=\""+osVersion+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if def != "" {
		if err := os.MkdirAll(filepath.Dir(p.Vmlinuz), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "boot/vmlinuz-"+def)
		if err := os.WriteFile(target, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("vmlinuz-"+def, p.Vmlinuz); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", guestKernelScript(kernel, p))
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(log)
	return parseGuestReport(string(out)), string(calls)
}

func TestGuestKernelHWEInstallsAndReportsReboot(t *testing.T) {
	// Before the install /boot/vmlinuz already names the new kernel in the
	// stub; the real package moves it.
	rep, calls := runKernelScript(t, GuestKernelHWE, "24.04", "6.8.0-146-generic", "6.17.0-5-generic", false, false)
	if !strings.Contains(calls, "apt-get install -y -qq "+HWEPackage) {
		t.Errorf("want the HWE install; calls:\n%s", calls)
	}
	if len(rep.Reboot) != 1 || !strings.Contains(rep.Reboot[0], "runs kernel 6.8.0-146-generic; the default kernel is now 6.17.0-5-generic") {
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
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "installing "+HWEPackage+" failed") {
		t.Errorf("warn = %q", rep.Warn)
	}
	if len(rep.Reboot) != 0 {
		t.Errorf("reboot = %q", rep.Reboot)
	}
}

func TestGuestKernelHWEOtherReleaseSkips(t *testing.T) {
	rep, calls := runKernelScript(t, GuestKernelHWE, "26.04", "7.0.0-1-generic", "7.0.0-1-generic", false, false)
	if strings.Contains(calls, "apt-get") || strings.Contains(calls, "dpkg") {
		t.Errorf("another release must install nothing; calls:\n%s", calls)
	}
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "not 24.04") {
		t.Errorf("warn = %q", rep.Warn)
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

func TestEnsureGuestKernelRejectsUnknown(t *testing.T) {
	g := Guest{Host: proc.NewFakeRunner(), UserPrefix: []string{"x"}, RootPrefix: []string{"x"}}
	if _, err := g.EnsureGuestKernel(context.Background(), "mainline"); err == nil {
		t.Fatal("want a refusal")
	}
}
