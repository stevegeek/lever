package host

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

// guestKernelProbe is what doctor reads about the guest kernel, as the run
// user, in one exec: the crash-recovery sysctls, the EFI pstore state, the
// running and default kernels, and the HWE metapackage.
type guestKernelProbe struct {
	sysctl         map[string]string // key (kernel.panic) -> value
	cmdline        bool              // guest.EFIPstoreCmdline is on /proc/cmdline
	efiPstore      bool              // the EFI pstore backend may be registered
	running, deflt string            // uname -r; the kernel /boot/vmlinuz names
	release        string            // VERSION_ID of /etc/os-release
	hwePkg         string            // the guest release's HWE package; "" when lever knows none
	hwe            bool              // hwePkg is installed
}

// guestKernelProbeScript prints key=value lines, empty when unreadable. Only
// fixed, non-secret paths are read.
func guestKernelProbeScript() string {
	var b strings.Builder
	for _, s := range guest.CrashSysctls {
		path := "/proc/sys/" + strings.ReplaceAll(s.Key, ".", "/")
		fmt.Fprintf(&b, "printf 'sysctl.%s=%%s\\n' \"$(cat %s 2>/dev/null)\"\n", s.Key, path)
	}
	fmt.Fprintf(&b, "if grep -qw -- '%s' /proc/cmdline 2>/dev/null; then echo cmdline=1; else echo cmdline=0; fi\n", guest.EFIPstoreCmdline)
	b.WriteString(guest.EFIPstoreActiveShell)
	b.WriteString("if efi_pstore_active /sys/module; then echo efi_pstore=active; else echo efi_pstore=off; fi\n")
	b.WriteString("echo \"running=$(uname -r)\"\n")
	// readlink -f prints a missing path as it is: only a vmlinuz-<version> target
	// names a default kernel.
	b.WriteString("d=$(readlink -f /boot/vmlinuz 2>/dev/null); case \"$d\" in */vmlinuz-*) d=${d##*/vmlinuz-} ;; *) d= ;; esac; echo \"default=$d\"\n")
	// dash exits on a "." whose file it cannot open, || or not.
	b.WriteString("VERSION_ID=; if [ -r /etc/os-release ]; then . /etc/os-release; fi\n")
	b.WriteString(guest.HWEPackageShell())
	b.WriteString("echo \"release=$VERSION_ID\"\n")
	b.WriteString("echo \"hwepkg=$pkg\"\n")
	b.WriteString("if [ -n \"$pkg\" ] && [ \"$(dpkg-query -W -f='${Status}' \"$pkg\" 2>/dev/null)\" = \"install ok installed\" ]; then echo hwe=1; else echo hwe=0; fi\n")
	return b.String()
}

// releaseRE is the shape of an Ubuntu VERSION_ID; anything else is not shown.
var releaseRE = regexp.MustCompile(`^[0-9]{2}\.[0-9]{2}$`)

func parseGuestKernelProbe(out string) guestKernelProbe {
	p := guestKernelProbe{sysctl: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(k, "sysctl."):
			p.sysctl[strings.TrimPrefix(k, "sysctl.")] = v
		case k == "cmdline":
			p.cmdline = v == "1"
		case k == "efi_pstore":
			p.efiPstore = v == "active"
		case k == "running":
			p.running = v
		case k == "default":
			p.deflt = v
		case k == "hwe":
			p.hwe = v == "1"
		case k == "release":
			if releaseRE.MatchString(v) {
				p.release = v
			}
		case k == "hwepkg":
			for _, r := range guest.HWEReleases {
				if v == guest.HWEPackage(r) {
					p.hwePkg = v
				}
			}
		}
	}
	return p
}

func readGuestKernelProbe(ctx context.Context, jr proc.Runner) (guestKernelProbe, error) {
	res, err := jr.Run(ctx, nil, "sh", "-c", guestKernelProbeScript())
	if err != nil {
		return guestKernelProbe{}, err
	}
	p := parseGuestKernelProbe(res.Stdout)
	if p.running == "" {
		return p, fmt.Errorf("unexpected probe output")
	}
	return p, nil
}

const crashRecoveryRebootFix = "run `lever apply`; reboot the VM for the kernel command line (" + lima.RebootAdvice + ")"

// checkGuestCrashRecovery checks, on a Lima guest, the sysctls that turn an
// oops or soft lockup into a reboot, and that no EFI pstore backend an oops
// could hang in is registered. A warning only: the jail works, it just does
// not recover from a kernel crash by itself.
func checkGuestCrashRecovery(backendName string, p guestKernelProbe, perr error) checkResult {
	const name = "guest crash recovery"
	if backendName != config.BackendLima {
		return checkResult{name, true, "not applicable (OrbStack: the kernel is OrbStack's, shared by every machine; lever does not change it)", ""}
	}
	if perr != nil {
		return checkResult{name, true, "not checked (the guest did not answer: " + firstLine(perr.Error()) + ")", ""}
	}
	var bad []string
	for _, s := range guest.CrashSysctls {
		if got := p.sysctl[s.Key]; got != s.Value {
			if got == "" {
				got = "unreadable"
			}
			bad = append(bad, fmt.Sprintf("%s=%s (want %s)", s.Key, got, s.Value))
		}
	}
	if len(bad) > 0 {
		return warnResult(name, "the guest kernel would hang, not reboot, on a crash: "+strings.Join(bad, ", "),
			"run `lever apply` (sets them live and in "+guest.CrashSysctlPath+")")
	}
	if p.efiPstore {
		return warnResult(name, "the EFI pstore backend is registered: a kernel oops can hang in an EFI runtime call instead of rebooting",
			crashRecoveryRebootFix)
	}
	if !p.cmdline {
		return warnResult(name, "the kernel command line lacks "+guest.EFIPstoreCmdline+" (reboot pending); the EFI pstore backend is off for this boot only",
			crashRecoveryRebootFix)
	}
	return checkResult{name, true, "panic on oops and soft lockup, reboot after 10 s; EFI pstore off", ""}
}

// checkGuestKernel reports the running guest kernel and warns when a reboot is
// pending, when guest_kernel is hwe but no known HWE kernel is installed, or
// when nested_virt runs on the 24.04 GA kernel (6.8), which a nested VM
// crashed.
func checkGuestKernel(app *config.App, p guestKernelProbe, perr error) checkResult {
	const name = "guest kernel"
	if app.Backend != config.BackendLima {
		if perr != nil || p.running == "" {
			return checkResult{name, true, "not applicable (OrbStack manages its kernel)", ""}
		}
		return checkResult{name, true, "running " + p.running + " (OrbStack manages its kernel)", ""}
	}
	if perr != nil {
		return checkResult{name, true, "not checked (the guest did not answer: " + firstLine(perr.Error()) + ")", ""}
	}
	flavour := app.EffectiveGuestKernel()
	detail := "running " + p.running + " (guest_kernel " + flavour + ")"
	if p.release != "" {
		detail = "Ubuntu " + p.release + ", " + detail
	}
	switch {
	case p.deflt != "" && p.deflt != p.running:
		return warnResult(name, detail+"; the default kernel is "+p.deflt+" (reboot pending)", lima.RebootAdvice)
	case flavour == config.GuestKernelHWE && p.hwePkg == "":
		return warnResult(name, detail+"; the guest's Ubuntu release has no HWE kernel lever installs (it knows "+strings.Join(guest.HWEReleases, " and ")+")",
			"recreate the VM on lever's current image ("+lima.RecreateAdvice+"), or set guest_kernel: ga")
	case flavour == config.GuestKernelHWE && !p.hwe:
		return warnResult(name, detail+"; "+p.hwePkg+" is not installed",
			"run `lever apply` (it installs the package; under the closed egress posture the guest may not reach the Ubuntu mirrors), then reboot the VM ("+lima.RebootAdvice+")")
	case app.NestedVirt && strings.HasPrefix(p.running, "6.8."):
		fix := "run `lever apply`, then reboot the VM (" + lima.RebootAdvice + ")"
		if app.GuestKernel == config.GuestKernelGA {
			// An explicit choice: say what it costs, do not override it.
			fix = "guest_kernel: ga is set explicitly; to move off 6.8, remove it (hwe is the default with nested_virt) or set hwe, then " + fix
		}
		return warnResult(name, detail+"; nested_virt runs on the 24.04 GA kernel (6.8), which crashed under nested VM load", fix)
	}
	if p.release != "" && p.release != lima.GuestRelease {
		// Not a warning: an older guest keeps working, and lever never upgrades a
		// guest's release in place.
		detail += "; lever creates new VMs on Ubuntu " + lima.GuestRelease + ", and this VM stays on " + p.release + " until it is recreated (" + lima.RecreateAdvice + ")"
	}
	return checkResult{name, true, detail, ""}
}
