package guest

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/egress"
)

const egressChain = egress.Chain

// Guest crash recovery. A jail guest whose kernel takes an oops must reboot,
// not hang:
//
//   - sysctls: panic on an oops and on a soft lockup, and reboot 10 s after
//     a panic (the kernel's default panic=0 waits forever);
//   - no EFI pstore: the oops and panic paths run kmsg_dump, and the EFI
//     pstore backend's runtime call can hang there in a spinlock. It is
//     unloaded now when possible, blacklisted in modprobe.d, and disabled on
//     the kernel command line (efi_pstore.pstore_disable=1) from the next
//     boot;
//   - no KVM async page faults on an x86_64 guest (no-kvmapf, from the next
//     boot): with them a task that touches guest memory the host swapped
//     out sleeps until the host says the page is back, and a lost notice
//     leaves it in D state for good. Without them the vCPU waits for the
//     page instead. The parameter exists only in the x86 KVM guest code, so
//     other guests do not get it.
//
// kernel.hung_task_panic stays 0, written explicitly: a task blocked in D
// state is what slow guest disk I/O looks like, and panicking on it would
// reboot a healthy guest.
//
// Lima only: kernel.* sysctls are not namespaced, and an OrbStack machine
// shares OrbStack's one kernel (the caller gates). Nothing here reboots the
// guest; the caller reports what waits for a reboot.

const (
	CrashSysctlPath   = "/etc/sysctl.d/99-lever-crash-recovery.conf"
	CrashModprobePath = "/etc/modprobe.d/lever-no-efi-pstore.conf"
	CrashGrubPath     = "/etc/default/grub.d/99-lever-crash-recovery.cfg"
)

// EFIPstoreCmdline keeps the EFI pstore backend from registering, built in or
// a module.
const EFIPstoreCmdline = "efi_pstore.pstore_disable=1"

// NoKVMAPFCmdline turns KVM async page faults off in an x86_64 guest.
const NoKVMAPFCmdline = "no-kvmapf"

// NoKVMAPFArch is the uname -m of the guests that get NoKVMAPFCmdline.
const NoKVMAPFArch = "x86_64"

// crashCmdline is what the GRUB drop-in adds to the kernel command line on a
// guest whose uname -m is arch.
func crashCmdline(arch string) []string {
	if arch == NoKVMAPFArch {
		return []string{EFIPstoreCmdline, NoKVMAPFCmdline}
	}
	return []string{EFIPstoreCmdline}
}

// CrashSysctls are the sysctls lever sets in a Lima guest, in file order.
var CrashSysctls = []struct{ Key, Value string }{
	{"kernel.panic_on_oops", "1"},
	{"kernel.panic", "10"},
	{"kernel.softlockup_panic", "1"},
	{"kernel.hung_task_panic", "0"},
}

// GuestReport is what a guest convergence step found that the operator
// should hear about but that does not fail the bring-up: Reboot lists what
// takes effect only at the next guest boot, Warn anything else.
type GuestReport struct {
	Reboot []string
	Warn   []string
}

func parseGuestReport(out string) GuestReport {
	var r GuestReport
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "reboot: "):
			r.Reboot = append(r.Reboot, strings.TrimPrefix(line, "reboot: "))
		case strings.HasPrefix(line, "warn: "):
			r.Warn = append(r.Warn, strings.TrimPrefix(line, "warn: "))
		}
	}
	return r
}

// crashPaths is a test seam.
type crashPaths struct {
	Sysctl, Modprobe, GrubDropIn, GrubCfg, Cmdline, SysModule string
	// Core is converged in the same root call.
	Core corePaths
}

var realCrashPaths = crashPaths{
	Sysctl:     CrashSysctlPath,
	Modprobe:   CrashModprobePath,
	GrubDropIn: CrashGrubPath,
	GrubCfg:    "/boot/grub/grub.cfg",
	Cmdline:    "/proc/cmdline",
	SysModule:  "/sys/module",
	Core:       realCorePaths,
}

func crashSysctlFile() string {
	var b strings.Builder
	b.WriteString("# Written by lever (guest crash recovery, lever#179). Do not edit: lever apply rewrites it.\n")
	b.WriteString("# Panic on an oops or a soft lockup and reboot 10 s later, instead of hanging.\n")
	b.WriteString("# hung_task_panic stays 0: slow disk I/O must not reboot a healthy guest.\n")
	for i, s := range CrashSysctls {
		b.WriteString(s.Key + " = " + s.Value)
		if i < len(CrashSysctls)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

const crashModprobeFile = `# Written by lever (guest crash recovery, lever#179). Do not edit: lever apply rewrites it.
# An oops or panic must not run an EFI runtime call (kmsg_dump -> efi_pstore).
blacklist efi_pstore
install efi_pstore /bin/true`

func crashGrubFile(arch string) string {
	var b strings.Builder
	b.WriteString("# Written by lever (guest crash recovery, lever#179). Do not edit: lever apply rewrites it.\n")
	b.WriteString("# " + EFIPstoreCmdline + ": keeps the EFI pstore backend from registering at boot, built in or a module.\n")
	if arch == NoKVMAPFArch {
		b.WriteString("# " + NoKVMAPFCmdline + ": no KVM async page faults, so a lost page-ready notice from the host cannot hang a task.\n")
	}
	b.WriteString(`GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX:-} ` + strings.Join(crashCmdline(arch), " ") + `"`)
	return b.String()
}

// EFIPstoreActiveShell is the shell function both the provisioning script and
// doctor's probe use to tell whether the EFI pstore backend may still be
// registered. Unknown reads as active. The backend's name is "efi_pstore" on
// Ubuntu's 6.8 and 7.0 kernels and "efi" on older kernels.
const EFIPstoreActiveShell = `efi_pstore_active() {
  [ -d "$1/efi_pstore" ] || return 1
  b=$(cat "$1/pstore/parameters/backend" 2>/dev/null || true)
  case "$b" in efi|efi_pstore) return 0 ;; esac
  d=$(cat "$1/efi_pstore/parameters/pstore_disable" 2>/dev/null || true)
  [ "$d" = Y ] && return 1
  return 0
}
`

// convergeShell defines converge PATH CONTENT: it writes only when PATH does
// not hold CONTENT already, and returns 0 when it wrote. A failed write exits
// the script itself because set -e does not reach a function called from a
// || list.
const convergeShell = `converge() {
  if [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; then return 1; fi
  mkdir -p "$(dirname "$1")" || exit 1
  printf '%s\n' "$2" > "$1.lever-tmp" || exit 1
  chmod 0644 "$1.lever-tmp" || exit 1
  mv -f "$1.lever-tmp" "$1" || exit 1
  return 0
}
`

// crashRecoveryScript: a failed write exits the script with 1; a failed
// sysctl, unload or update-grub is a warning.
func crashRecoveryScript(p crashPaths) string {
	q := shellSingleQuote
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(convergeShell)
	b.WriteString(EFIPstoreActiveShell)
	fmt.Fprintf(&b, "converge %s %s || true\n", q(p.Sysctl), q(crashSysctlFile()))
	for _, s := range CrashSysctls {
		kv := s.Key + "=" + s.Value
		fmt.Fprintf(&b, "sysctl -q -w %s >/dev/null 2>&1 || echo %s\n", q(kv), q("warn: could not set "+kv+" in the running guest kernel"))
	}
	fmt.Fprintf(&b, "converge %s %s || true\n", q(p.Modprobe), q(crashModprobeFile))
	fmt.Fprintf(&b, "if [ -e %s ]; then modprobe -r efi_pstore >/dev/null 2>&1 || true; fi\n", q(p.SysModule+"/efi_pstore/initstate"))
	// The guest's own uname decides, not the host's architecture.
	fmt.Fprintf(&b, "case \"$(uname -m 2>/dev/null || true)\" in\n  %s) grub_file=%s; params=%s ;;\n  *) grub_file=%s; params=%s ;;\nesac\n",
		q(NoKVMAPFArch), q(crashGrubFile(NoKVMAPFArch)), q(strings.Join(crashCmdline(NoKVMAPFArch), " ")),
		q(crashGrubFile("")), q(strings.Join(crashCmdline(""), " ")))
	fmt.Fprintf(&b, "grub_changed=0\nconverge %s \"$grub_file\" && grub_changed=1\n", q(p.GrubDropIn))
	fmt.Fprintf(&b, `grub_stale=0
for p in $params; do grep -qwF -- "$p" %[1]s 2>/dev/null || grub_stale=1; done
if [ "$grub_changed" = 1 ] || [ "$grub_stale" = 1 ]; then
  if command -v update-grub >/dev/null 2>&1; then
    update-grub >/dev/null 2>&1 || echo "warn: update-grub failed; the kernel command line will not get $params (run sudo update-grub in the guest)"
  else
    echo "warn: the guest has no update-grub; the kernel command line will not get $params"
  fi
fi
missing=
for p in $params; do grep -qwF -- "$p" %[2]s 2>/dev/null || missing="${missing:+$missing }$p"; done
if [ -n "$missing" ]; then echo "reboot: the kernel command line gets $missing at the next boot"; fi
`, q(p.GrubCfg), q(p.Cmdline))
	fmt.Fprintf(&b, "if efi_pstore_active %s; then echo %s; fi\n", q(p.SysModule),
		q("reboot: the EFI pstore backend is still registered (built in, or efi_pstore could not be unloaded); it is off from the next boot"))
	// The tmux-core capture is diagnostics: run in a subshell, its failure is
	// a warning and never fails the bring-up.
	fmt.Fprintf(&b, "(\n%s) || echo %s\n", coreCaptureScript(p.Core), q("warn: could not set up the tmux server core capture ("+CoreDir+"); a tmux crash leaves no core"))
	return b.String()
}

// EnsureCrashRecovery converges the guest's crash-recovery settings and its
// tmux-core capture, and reports what waits for a reboot. Idempotent. Lima
// only: the caller gates it.
func (g Guest) EnsureCrashRecovery(ctx context.Context) (GuestReport, error) {
	res, err := g.RootRun(ctx, "bash", "-c", crashRecoveryScript(realCrashPaths))
	if err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		return GuestReport{}, fmt.Errorf("guest crash recovery (sysctls, EFI pstore off): %w", err)
	}
	return parseGuestReport(res.Stdout), nil
}

// Guest kernel flavours (config guest_kernel, Lima only).
const (
	GuestKernelGA  = "ga"
	GuestKernelHWE = "hwe"
)

// HWEReleases are the Ubuntu LTS releases whose hardware-enablement kernel
// guest_kernel hwe installs.
var HWEReleases = []string{"24.04", "26.04"}

// HWEPackage is the hardware-enablement kernel metapackage of release.
func HWEPackage(release string) string { return "linux-generic-hwe-" + release }

func hwePackageFor(release string) string {
	for _, r := range HWEReleases {
		if r == release {
			return HWEPackage(r)
		}
	}
	return ""
}

// HWEPackageShell sets pkg to the HWE metapackage of the release in
// $VERSION_ID, or to "". Shared with doctor's probe so both name the same
// package.
func HWEPackageShell() string {
	var b strings.Builder
	b.WriteString("case \"$VERSION_ID\" in\n")
	for _, r := range HWEReleases {
		fmt.Fprintf(&b, "  %s) pkg=%s ;;\n", r, HWEPackage(r))
	}
	b.WriteString("  *) pkg= ;;\nesac\n")
	return b.String()
}

// kernelPaths is a test seam.
type kernelPaths struct {
	OSRelease, Boot, MachineID string
}

var realKernelPaths = kernelPaths{OSRelease: "/etc/os-release", Boot: "/boot", MachineID: "/etc/machine-id"}

// HWE install outcomes, read into KernelReport.HWE.
const (
	HWEInstalled    = "installed"      // the package is installed (now or before)
	HWEFailed       = "failed"         // apt-get update or install failed
	HWESkippedClose = "skipped-egress" // the guest's egress is closed; apt not tried
	HWESkippedStamp = "skipped-stamp"  // an earlier install failed on this VM; apt not tried
)

// KernelOpts tunes EnsureGuestKernel.
type KernelOpts struct {
	// SkipIfMachineID skips the HWE install, without running apt, when the
	// guest's /etc/machine-id equals it. "" never skips.
	SkipIfMachineID string
}

// KernelReport is EnsureGuestKernel's GuestReport plus what the host needs
// to keep its failed-install stamp. Empty fields mean not applicable or
// unreadable.
type KernelReport struct {
	GuestReport
	HWE        string
	HWEPackage string
	MachineID  string
}

// machineIDRE validates the guest-reported id before it reaches a host file
// or a guest script.
var machineIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func parseKernelReport(out string) KernelReport {
	r := KernelReport{GuestReport: parseGuestReport(out)}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "hwe: "):
			switch v := strings.TrimPrefix(line, "hwe: "); v {
			case HWEInstalled, HWEFailed, HWESkippedClose, HWESkippedStamp:
				r.HWE = v
			}
		case strings.HasPrefix(line, "hwe-package: "):
			for _, rel := range HWEReleases {
				if v := strings.TrimPrefix(line, "hwe-package: "); v == HWEPackage(rel) {
					r.HWEPackage = v
				}
			}
		case strings.HasPrefix(line, "machine-id: "):
			if v := strings.TrimPrefix(line, "machine-id: "); machineIDRE.MatchString(v) {
				r.MachineID = v
			}
		}
	}
	return r
}

// AptLockWait is how long (seconds) apt waits for the apt and dpkg locks: on
// a fresh VM cloud-init or apt-daily may hold them, and that first apply is
// the one with open egress.
const AptLockWait = 120

// guestKernelScript does not try apt when the host's stamp names this VM or
// when the egress chain ends in the closed posture's catch-all DROP: the
// mirrors are unreachable, and apt would only wait out its timeout. update is
// retried on a held lock for an apt that does not apply DPkg::Lock::Timeout
// to the lists lock. update is bounded by a timeout; the install is never
// killed midway. It never removes a kernel.
func guestKernelScript(kernel string, opts KernelOpts, p kernelPaths) string {
	q := shellSingleQuote
	var b strings.Builder
	b.WriteString("set -u\n")
	if kernel == GuestKernelHWE {
		fmt.Fprintf(&b, `VERSION_ID=
. %[1]s 2>/dev/null || true
mid=$(cat %[2]s 2>/dev/null || true)
echo "machine-id: $mid"
%[3]s[ -z "$pkg" ] || echo "hwe-package: $pkg"
if [ -z "$pkg" ]; then
  echo "warn: guest_kernel hwe: the guest is Ubuntu ${VERSION_ID:-unknown}, not %[10]s; lever installed no kernel"
elif [ "$(dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null)" = "install ok installed" ]; then
  echo "hwe: installed"
elif [ -n %[4]s ] && [ "$mid" = %[4]s ]; then
  echo "hwe: skipped-stamp"
elif iptables -S %[5]s 2>/dev/null | grep -qx -- %[6]s; then
  echo "hwe: skipped-egress"
  printf %[7]s "$pkg"
else
  export DEBIAN_FRONTEND=noninteractive
  ok=0
  end=$(( $(date +%%s) + %[8]d ))
  while :; do
    err=$(timeout %[8]d apt-get -o DPkg::Lock::Timeout=%[8]d update -qq 2>&1 >/dev/null) && { ok=1; break; }
    case "$err" in *"Could not get lock"*|*"Unable to lock"*) ;; *) break ;; esac
    [ "$(date +%%s)" -lt "$end" ] || break
    sleep 5
  done
  if [ "$ok" = 1 ] && apt-get install -y -qq -o DPkg::Lock::Timeout=%[8]d "$pkg" >/dev/null 2>&1; then
    echo "hwe: installed"
  else
    echo "hwe: failed"
    printf %[9]s "$pkg"
  fi
fi
`, q(p.OSRelease), q(p.MachineID), HWEPackageShell(), q(opts.SkipIfMachineID), q(egressChain), q("-A "+egressChain+" -j DROP"),
			q("warn: guest_kernel hwe: %s is not installed, and lever did not run apt: the jail's egress is closed (the closed posture's catch-all DROP is in place), so the Ubuntu mirrors are unreachable; run lever apply once with the open posture, or install it by hand in the guest\n"),
			AptLockWait,
			q("warn: guest_kernel hwe: installing %s failed (the guest may not reach the Ubuntu mirrors); run lever apply again with the open posture, or install it by hand in the guest\n"),
			strings.Join(HWEReleases, " or "))
	}
	b.WriteString(BootKernelShell(p.Boot))
	b.WriteString(`running=$(uname -r)
default=$(` + BootKernelFunc + `)
if [ -n "$default" ] && [ "$default" != "$running" ]; then
  echo "reboot: the guest runs kernel $running; the next boot starts $default"
fi
`)
	return b.String()
}

// EnsureGuestKernel converges the guest kernel flavour: hwe installs the
// release's HWE package, ga changes nothing (it does not remove an installed
// HWE kernel). It reports a reboot when the kernel GRUB boots next
// (BootKernelShell) is not the running one. "" only reports.
func (g Guest) EnsureGuestKernel(ctx context.Context, kernel string, opts KernelOpts) (KernelReport, error) {
	switch kernel {
	case "", GuestKernelGA, GuestKernelHWE:
	default:
		return KernelReport{}, fmt.Errorf("guest_kernel %q: want %q or %q", kernel, GuestKernelHWE, GuestKernelGA)
	}
	if opts.SkipIfMachineID != "" && !machineIDRE.MatchString(opts.SkipIfMachineID) {
		opts.SkipIfMachineID = ""
	}
	res, err := g.RootRun(ctx, "bash", "-c", guestKernelScript(kernel, opts, realKernelPaths))
	if err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		return KernelReport{}, fmt.Errorf("guest kernel (%s): %w", kernel, err)
	}
	return parseKernelReport(res.Stdout), nil
}
