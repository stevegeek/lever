package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

// guestKernelProbe is what doctor reads about the jail guest's kernel, as
// the run user, in one exec: the crash-recovery sysctls, whether the
// command line disables the EFI pstore backend and whether it is still
// registered, the running and default kernels, and whether the HWE
// metapackage is installed (lever#179).
type guestKernelProbe struct {
	sysctl         map[string]string // key (kernel.panic) -> value
	cmdline        bool              // guest.EFIPstoreCmdline is on /proc/cmdline
	efiPstore      bool              // the EFI pstore backend may be registered
	running, deflt string            // uname -r; the kernel /boot/vmlinuz names
	hwe            bool              // guest.HWEPackage is installed
}

// guestKernelProbeScript prints key=value lines; a value it cannot read is
// empty. Only fixed paths are read, none of them secret.
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
	b.WriteString("d=$(readlink -f /boot/vmlinuz 2>/dev/null); echo \"default=${d##*/vmlinuz-}\"\n")
	fmt.Fprintf(&b, "if dpkg -s %s >/dev/null 2>&1; then echo hwe=1; else echo hwe=0; fi\n", guest.HWEPackage)
	return b.String()
}

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
		}
	}
	return p
}

// readGuestKernelProbe runs the probe through the jail transport.
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

// checkGuestCrashRecovery is the doctor row "guest crash recovery": on a
// Lima guest, the sysctls that turn an oops or a soft lockup into a reboot,
// and no EFI pstore backend that an oops could hang in (lever#179). A
// warning, never a failure: the jail works without them, it just does not
// come back by itself from a kernel crash.
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

// checkGuestKernel is the doctor row "guest kernel": the running guest
// kernel, and a warning when the default kernel differs from it (a reboot
// is pending), when guest_kernel is hwe but the HWE kernel is not
// installed, or when nested_virt runs on the 24.04 GA kernel (6.8), the
// kernel a nested VM crashed (lever#179).
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
	switch {
	case p.deflt != "" && p.deflt != p.running:
		return warnResult(name, detail+"; the default kernel is "+p.deflt+" (reboot pending)", lima.RebootAdvice)
	case flavour == config.GuestKernelHWE && !p.hwe:
		return warnResult(name, detail+"; "+guest.HWEPackage+" is not installed",
			"run `lever apply` (it installs the package; under the closed egress posture the guest may not reach the Ubuntu mirrors), then reboot the VM ("+lima.RebootAdvice+")")
	case app.NestedVirt && strings.HasPrefix(p.running, "6.8."):
		return warnResult(name, detail+"; nested_virt runs on the 24.04 GA kernel (6.8), which crashed under nested VM load",
			"set guest_kernel: hwe (the default with nested_virt), run `lever apply`, then reboot the VM ("+lima.RebootAdvice+")")
	}
	return checkResult{name, true, detail, ""}
}
