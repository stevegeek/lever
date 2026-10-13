package host

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

const hardenedProbe = `sysctl.kernel.panic_on_oops=1
sysctl.kernel.panic=10
sysctl.kernel.softlockup_panic=1
sysctl.kernel.hung_task_panic=0
cmdline=1
efi_pstore=off
running=6.17.0-5-generic
default=6.17.0-5-generic
release=26.04
hwepkg=linux-generic-hwe-26.04
hwe=1
`

func probeWith(edits ...string) guestKernelProbe {
	out := hardenedProbe
	for i := 0; i+1 < len(edits); i += 2 {
		out = strings.Replace(out, edits[i], edits[i+1], 1)
	}
	return parseGuestKernelProbe(out)
}

func TestParseGuestKernelProbe(t *testing.T) {
	p := parseGuestKernelProbe(hardenedProbe + "garbage\n")
	if p.sysctl["kernel.panic"] != "10" || !p.cmdline || p.efiPstore || p.running != "6.17.0-5-generic" || p.deflt != p.running || !p.hwe ||
		p.release != "26.04" || p.hwePkg != "linux-generic-hwe-26.04" {
		t.Fatalf("parsed %+v", p)
	}
}

func TestCheckGuestCrashRecovery(t *testing.T) {
	cases := []struct {
		name      string
		p         guestKernelProbe
		perr      error
		backend   string
		wantClean bool // ok with no fix
		want      string
		wantFix   string
	}{
		{"hardened", probeWith(), nil, config.BackendLima, true, "EFI pstore off", ""},
		{"orbstack", guestKernelProbe{}, nil, config.BackendOrbstack, true, "not applicable", ""},
		{"no answer", guestKernelProbe{}, errors.New("vm down"), config.BackendLima, true, "not checked", ""},
		{"sysctl missing", probeWith("panic_on_oops=1", "panic_on_oops=0", "softlockup_panic=1", "softlockup_panic="), nil, config.BackendLima, false,
			"kernel.panic_on_oops=0 (want 1), kernel.softlockup_panic=unreadable (want 1)", "lever apply"},
		{"efi registered", probeWith("efi_pstore=off", "efi_pstore=active"), nil, config.BackendLima, false, "EFI pstore backend is registered", "reboot the VM"},
		{"cmdline pending", probeWith("cmdline=1", "cmdline=0"), nil, config.BackendLima, false, "reboot pending", "reboot the VM for the kernel command line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkGuestCrashRecovery(c.backend, c.p, c.perr)
			if r.name != "guest crash recovery" || !r.ok {
				t.Fatalf("the row is a warning at worst: %+v", r)
			}
			if c.wantClean != (r.fix == "") {
				t.Errorf("clean=%t, got fix %q", c.wantClean, r.fix)
			}
			if !strings.Contains(r.detail, c.want) || !strings.Contains(r.fix, c.wantFix) {
				t.Errorf("got %+v, want detail %q fix %q", r, c.want, c.wantFix)
			}
			if strings.Contains(r.fix, "--fresh") {
				t.Errorf("the fix must keep the conversation: %q", r.fix)
			}
		})
	}
}

func TestCheckGuestKernel(t *testing.T) {
	lima := func(nested bool, kernel string) *config.App {
		return &config.App{Backend: config.BackendLima, NestedVirt: nested, GuestKernel: kernel}
	}
	cases := []struct {
		name    string
		app     *config.App
		p       guestKernelProbe
		want    string
		wantFix string
	}{
		{"hwe running", lima(true, ""), probeWith(), "Ubuntu 26.04, running 6.17.0-5-generic (guest_kernel hwe)", ""},
		{"older release", lima(false, ""), probeWith("release=26.04", "release=24.04", "hwe-26.04", "hwe-24.04"),
			"Ubuntu 24.04, running 6.17.0-5-generic (guest_kernel ga); lever creates new VMs on Ubuntu 26.04, and this VM stays on 24.04 until it is recreated (`lever conversation backup`", ""},
		{"hwe on an unknown release", lima(true, ""), probeWith("release=26.04", "release=25.10", "hwepkg=linux-generic-hwe-26.04", "hwepkg="),
			"the guest's Ubuntu release has no HWE kernel lever installs (it knows 24.04 and 26.04)", "guest_kernel: ga"},
		{"unknown package name ignored", lima(true, ""), probeWith("hwepkg=linux-generic-hwe-26.04", "hwepkg=linux-evil"),
			"no HWE kernel lever installs", "recreate the VM"},
		{"ga no nested", lima(false, ""), probeWith("6.17.0-5-generic", "6.8.0-146-generic", "6.17.0-5-generic", "6.8.0-146-generic", "hwe=1", "hwe=0"),
			"running 6.8.0-146-generic (guest_kernel ga)", ""},
		{"reboot pending", lima(true, ""), probeWith("running=6.17.0-5-generic", "running=6.8.0-146-generic"),
			"the default kernel is 6.17.0-5-generic (reboot pending)", "lever stop"},
		{"hwe missing", lima(true, ""), probeWith("hwe=1", "hwe=0"), "linux-generic-hwe-26.04 is not installed", "lever apply"},
		{"nested on ga", lima(true, "ga"), probeWith("6.17.0-5-generic", "6.8.0-146-generic", "6.17.0-5-generic", "6.8.0-146-generic"),
			"nested_virt runs on the 24.04 GA kernel", "guest_kernel: ga is set explicitly"},
		{"nested on 6.8 by default", lima(true, ""), probeWith("6.17.0-5-generic", "6.8.0-146-generic", "6.17.0-5-generic", "6.8.0-146-generic"),
			"nested_virt runs on the 24.04 GA kernel", "run `lever apply`, then reboot"},
		{"orbstack", &config.App{Backend: config.BackendOrbstack}, probeWith(), "running 6.17.0-5-generic (OrbStack manages its kernel)", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkGuestKernel(c.app, c.p, nil)
			if r.name != "guest kernel" || !r.ok {
				t.Fatalf("the row is a warning at worst: %+v", r)
			}
			if !strings.Contains(r.detail, c.want) {
				t.Errorf("detail %q, want %q", r.detail, c.want)
			}
			if (c.wantFix == "") != (r.fix == "") || !strings.Contains(r.fix, c.wantFix) {
				t.Errorf("fix %q, want %q", r.fix, c.wantFix)
			}
		})
	}
}

func TestReadGuestKernelProbeUsesJail(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("sh -c", proc.Result{Stdout: hardenedProbe})
	p, err := readGuestKernelProbe(context.Background(), f)
	if err != nil || p.running != "6.17.0-5-generic" {
		t.Fatalf("got %+v, %v", p, err)
	}
	script := f.Calls[0].Args[len(f.Calls[0].Args)-1]
	for _, want := range []string{"/proc/sys/kernel/panic_on_oops", "/proc/sys/kernel/softlockup_panic", "/proc/cmdline", "/sys/module", "uname -r", "/boot/vmlinuz", "/etc/os-release", guest.HWEPackage("24.04"), guest.HWEPackage("26.04")} {
		if !strings.Contains(script, want) {
			t.Errorf("probe missing %q", want)
		}
	}
	f2 := proc.NewFakeRunner()
	f2.Script("sh -c", proc.Result{Stdout: "nothing\n"})
	if _, err := readGuestKernelProbe(context.Background(), f2); err == nil {
		t.Error("output without a running kernel must be an error")
	}
}

// TestGuestKernelProbeScriptRuns runs the probe on this (Linux) host: it
// only reads, and it proves the script parses and prints every key.
func TestGuestKernelProbeScriptRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads Linux /proc")
	}
	out, err := exec.Command("sh", "-c", guestKernelProbeScript()).Output()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	p := parseGuestKernelProbe(string(out))
	if p.running == "" {
		t.Fatalf("no running kernel in:\n%s", out)
	}
	if p.deflt != "" && strings.Contains(p.deflt, "/") {
		t.Errorf("default kernel %q is a path, not a version", p.deflt)
	}
	for _, s := range guest.CrashSysctls {
		if _, ok := p.sysctl[s.Key]; !ok {
			t.Errorf("no %s in:\n%s", s.Key, out)
		}
	}
	for _, k := range []string{"release=", "hwepkg=", "hwe="} {
		if !strings.Contains(string(out), "\n"+k) {
			t.Errorf("no %s line in:\n%s", k, out)
		}
	}
}
