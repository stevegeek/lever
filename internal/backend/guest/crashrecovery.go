package guest

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/egress"
)

// egressChain is lever's egress chain in the guest (egress.Chain).
const egressChain = egress.Chain

// Guest crash recovery (lever#179). A jail guest whose kernel takes an oops
// must reboot, not hang. The incident behind it: a Lima guest running a
// nested VM took a page fault on an idle CPU, the oops path faulted again,
// and the oops path's kmsg_dump ran the EFI pstore backend, whose EFI
// runtime call hung in a spinlock. CPU after CPU soft-locked for a day
// until Lima killed QEMU. Two things make the guest recover instead:
//
//   - sysctls: panic on an oops and on a soft lockup, and reboot 10 s after
//     a panic (the kernel's default panic=0 waits forever);
//   - no EFI pstore: the panic path runs kmsg_dump too, so with the EFI
//     backend still registered a panic can hang in the same EFI call. The
//     backend goes three ways: it is unloaded now when it is a module that
//     can be unloaded, a modprobe.d file stops it loading again, and the
//     kernel command line gets efi_pstore.pstore_disable=1 (honoured by a
//     built-in backend and by the module) from the next boot on.
//
// kernel.hung_task_panic stays 0, written explicitly so the file says so: a
// task blocked in D state for two minutes is what slow guest disk I/O looks
// like (a first podman layer copy on a grow-only qcow2, a stalled host
// mount), and panicking on it would reboot a healthy guest. A hung task
// does not wedge the kernel; the soft-lockup and oops paths cover the
// failure that does.
//
// Lima only: an OrbStack machine shares OrbStack's one kernel with every
// other machine and container on the host, and kernel.* sysctls are not
// namespaced, so lever does not change it (the caller gates).
//
// Nothing here reboots the guest. The command line takes effect at the next
// boot; the caller reports it (CrashRecoveryReport.Reboot).

// Guest paths of the crash-recovery files lever writes.
const (
	CrashSysctlPath   = "/etc/sysctl.d/99-lever-crash-recovery.conf"
	CrashModprobePath = "/etc/modprobe.d/lever-no-efi-pstore.conf"
	CrashGrubPath     = "/etc/default/grub.d/99-lever-crash-recovery.cfg"
)

// EFIPstoreCmdline is the kernel command-line parameter that keeps the EFI
// pstore backend from registering, built in or a module.
const EFIPstoreCmdline = "efi_pstore.pstore_disable=1"

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

// parseGuestReport reads the script protocol: "reboot: <why>" and
// "warn: <what>" lines on stdout; anything else is ignored.
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

// crashPaths are the paths the crash-recovery script reads and writes; the
// production values are the real guest paths, a test points them into a
// temp dir.
type crashPaths struct {
	Sysctl, Modprobe, GrubDropIn, GrubCfg, Cmdline, SysModule string
	// Core are the tmux-core capture paths (corecapture.go), converged in
	// the same root call.
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

const crashGrubFile = `# Written by lever (guest crash recovery, lever#179). Do not edit: lever apply rewrites it.
# Keeps the EFI pstore backend from registering at boot, built in or a module.
GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX:-} ` + EFIPstoreCmdline + `"`

// EFIPstoreActiveShell is the shell function both the provisioning script and
// doctor's probe use to tell whether the EFI pstore backend may still be
// registered: the module (or built-in) is present, and either pstore names
// it as its backend or it was not loaded with pstore_disable. Unknown reads
// as active. The backend's name is "efi_pstore" on Ubuntu's 6.8 and 7.0
// kernels (read from 24.04 and 26.04 Lima guests, where efi_pstore is a
// loadable module with no holders) and "efi" on older kernels.
const EFIPstoreActiveShell = `efi_pstore_active() {
  [ -d "$1/efi_pstore" ] || return 1
  b=$(cat "$1/pstore/parameters/backend" 2>/dev/null || true)
  case "$b" in efi|efi_pstore) return 0 ;; esac
  d=$(cat "$1/efi_pstore/parameters/pstore_disable" 2>/dev/null || true)
  [ "$d" = Y ] && return 1
  return 0
}
`

// convergeShell defines converge PATH CONTENT for the guest scripts: it
// writes CONTENT (plus a newline) to PATH, mode 0644, only when PATH does
// not hold it already, and returns 0 when it wrote. A failed write exits the
// script with 1 (set -e does not reach a function called from a || list).
const convergeShell = `converge() {
  if [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; then return 1; fi
  mkdir -p "$(dirname "$1")" || exit 1
  printf '%s\n' "$2" > "$1.lever-tmp" || exit 1
  chmod 0644 "$1.lever-tmp" || exit 1
  mv -f "$1.lever-tmp" "$1" || exit 1
  return 0
}
`

// crashRecoveryScript is the root script EnsureCrashRecovery runs. It
// writes each file only when its content differs, applies the sysctls live
// on every run, unloads a loaded efi_pstore module, runs update-grub only
// when the drop-in changed or grub.cfg lacks the parameter, and reports
// what waits for a reboot. A failed write exits the script with 1
// (converge exits itself: set -e does not reach a function called from a
// || list); a failed sysctl, unload or update-grub is a warning.
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
	fmt.Fprintf(&b, "grub_changed=0\nconverge %s %s && grub_changed=1\n", q(p.GrubDropIn), q(crashGrubFile))
	fmt.Fprintf(&b, `if [ "$grub_changed" = 1 ] || ! grep -q -- %s %s 2>/dev/null; then
  if command -v update-grub >/dev/null 2>&1; then
    update-grub >/dev/null 2>&1 || echo %s
  else
    echo %s
  fi
fi
`, q(EFIPstoreCmdline), q(p.GrubCfg),
		q("warn: update-grub failed; the kernel command line will not get "+EFIPstoreCmdline+" (run sudo update-grub in the guest)"),
		q("warn: the guest has no update-grub; the kernel command line will not get "+EFIPstoreCmdline))
	fmt.Fprintf(&b, "if ! grep -qw -- %s %s 2>/dev/null; then echo %s; fi\n", q(EFIPstoreCmdline), q(p.Cmdline),
		q("reboot: the kernel command line gets "+EFIPstoreCmdline+" at the next boot"))
	fmt.Fprintf(&b, "if efi_pstore_active %s; then echo %s; fi\n", q(p.SysModule),
		q("reboot: the EFI pstore backend is still registered (built in, or efi_pstore could not be unloaded); it is off from the next boot"))
	// The tmux-core capture rides the same root call (one guest round trip
	// on every up). It is diagnostics: run in a subshell, its failure is a
	// warning and never fails the bring-up.
	fmt.Fprintf(&b, "(\n%s) || echo %s\n", coreCaptureScript(p.Core), q("warn: could not set up the tmux server core capture ("+CoreDir+"); a tmux crash leaves no core"))
	return b.String()
}

// EnsureCrashRecovery converges the guest's crash-recovery settings (see
// the package notes above) and its tmux-core capture (corecapture.go), and
// reports what waits for a reboot.
// Idempotent; run on every apply and up. Lima only: the caller gates it.
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
// guest_kernel hwe installs. Each has the metapackage HWEPackage(release),
// which tracks the kernel of the newest Ubuntu interim release; until a
// release's first point release with a newer kernel it names the release's
// own GA kernel.
var HWEReleases = []string{"24.04", "26.04"}

// HWEPackage is the hardware-enablement kernel metapackage of release.
func HWEPackage(release string) string { return "linux-generic-hwe-" + release }

// hwePackageFor is HWEPackage for one of HWEReleases, else "".
func hwePackageFor(release string) string {
	for _, r := range HWEReleases {
		if r == release {
			return HWEPackage(r)
		}
	}
	return ""
}

// HWEPackageShell sets pkg to the HWE metapackage of the release in
// $VERSION_ID, or to "" when lever knows none for it. Shared with doctor's
// probe so both name the same package.
func HWEPackageShell() string {
	var b strings.Builder
	b.WriteString("case \"$VERSION_ID\" in\n")
	for _, r := range HWEReleases {
		fmt.Fprintf(&b, "  %s) pkg=%s ;;\n", r, HWEPackage(r))
	}
	b.WriteString("  *) pkg= ;;\nesac\n")
	return b.String()
}

// kernelPaths are the guest paths the kernel script reads; see crashPaths.
type kernelPaths struct {
	OSRelease, Vmlinuz, MachineID string
}

var realKernelPaths = kernelPaths{OSRelease: "/etc/os-release", Vmlinuz: "/boot/vmlinuz", MachineID: "/etc/machine-id"}

// HWE install outcomes the kernel script reports ("hwe: <state>"), read
// into KernelReport.HWE.
const (
	HWEInstalled    = "installed"      // the package is installed (now or before)
	HWEFailed       = "failed"         // apt-get update or install failed
	HWESkippedClose = "skipped-egress" // the guest's egress is closed; apt not tried
	HWESkippedStamp = "skipped-stamp"  // an earlier install failed on this VM; apt not tried
)

// KernelOpts tunes EnsureGuestKernel.
type KernelOpts struct {
	// SkipIfMachineID makes the script skip the HWE install, without
	// running apt, when the guest's /etc/machine-id equals it: the host
	// keeps a stamp of an install that failed on that VM (lima's
	// hweFailStamp) and passes its id while the egress posture is closed.
	// "" never skips.
	SkipIfMachineID string
}

// KernelReport is EnsureGuestKernel's GuestReport plus what the host needs
// to keep its failed-install stamp: the HWE outcome ("" when guest_kernel
// is not hwe or the release is not one of HWEReleases), the package it is
// about, and the guest's machine id ("" if unreadable).
type KernelReport struct {
	GuestReport
	HWE        string
	HWEPackage string
	MachineID  string
}

// machineIDRE is the shape of /etc/machine-id (32 lowercase hex digits).
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

// AptLockWait is how long (seconds) the kernel script lets apt wait for
// the apt and dpkg locks: on a fresh VM cloud-init or apt-daily may hold
// them, and that first apply is the one with open egress.
const AptLockWait = 120

// guestKernelScript is the root script EnsureGuestKernel runs. Under hwe it
// installs the HWE package of the guest's release (HWEPackageShell) unless dpkg-query says "install ok installed" (a
// package in state rc, or a cut-off install, is installed again). It does
// not try apt when the host's stamp names this VM (opts.SkipIfMachineID)
// or when lever's egress chain in the guest ends in the closed posture's
// catch-all DROP: the mirrors are unreachable there, and apt would only
// wait out its timeout. apt-get update and the install both wait up to
// AptLockWait for the apt and dpkg locks (DPkg::Lock::Timeout; update is
// also retried while it reports a held lock, for an apt that does not
// apply the option to the lists lock); update is bounded by a timeout, the
// install itself is never killed midway. Under any flavour it reports a
// reboot when the kernel /boot/vmlinuz names (the one grub boots by
// default) is not the running one. It never removes a kernel.
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
	fmt.Fprintf(&b, `running=$(uname -r)
default=$(readlink -f %s 2>/dev/null || true)
case "$default" in */vmlinuz-*) default=${default##*/vmlinuz-} ;; *) default= ;; esac
if [ -n "$default" ] && [ "$default" != "$running" ]; then
  echo "reboot: the guest runs kernel $running; the default kernel is now $default"
fi
`, q(p.Vmlinuz))
	return b.String()
}

// EnsureGuestKernel converges the guest kernel flavour: hwe installs the
// release's HWE package (idempotent), ga changes nothing (switching back does not
// remove an installed HWE kernel). Under either it reports a reboot when
// the default kernel is not the running one. "" (lever provision, which
// has no config) only reports.
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
