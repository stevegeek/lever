package guest

import (
	"context"
	"fmt"
	"strings"
)

// The guest profile (config guest_profile, Lima only).
//
// "small" disables the Ubuntu cloud image services a jail never uses and adds
// zram swap. Disabling unattended-upgrades and the apt-daily timers means the
// guest no longer patches itself: the operator updates or recreates it.
//
// Only units that exist and are enabled are disabled, and lever records
// them, so "standard" enables exactly those again: a unit the operator
// disabled themselves is never touched in either direction.
//
// The zram unit and helper are lever's own rather than a package, so nothing
// is installed with apt (a closed-egress guest has no mirrors). A kernel
// without the zram module only warns.
//
// Lima only: an OrbStack machine shares OrbStack's kernel and VM.

const (
	GuestProfileStandard = "standard"
	GuestProfileSmall    = "small"
)

// TrimmedUnits are the systemd units the small profile disables, in order.
var TrimmedUnits = []string{
	// Sockets before their services, so socket activation cannot start a
	// service again between the two.
	"snapd.socket", "snapd.service", "snapd.seeded.service",
	"unattended-upgrades.service", "apt-daily.timer", "apt-daily-upgrade.timer",
	"multipathd.socket", "multipathd.service",
	"ModemManager.service",
}

const ZramUnitName = "lever-zram-swap.service"

// profilePaths is a test seam.
type profilePaths struct {
	// State lists, one per line, the units lever disabled.
	State                string
	ZramUnit, ZramHelper string
	// ZramDev records the device the helper set up, for its stop.
	ZramDev string
}

var realProfilePaths = profilePaths{
	State:      "/var/lib/lever/guest-profile-disabled-units",
	ZramUnit:   "/etc/systemd/system/" + ZramUnitName,
	ZramHelper: "/usr/local/sbin/lever-zram-swap",
	ZramDev:    "/run/lever-zram-swap.dev",
}

func zramHelper(p profilePaths) string {
	return fmt.Sprintf(`#!/bin/sh
# Written by lever (guest_profile: small, lever#193). Do not edit: lever apply rewrites it.
set -eu
case "${1:-}" in
start)
  modprobe zram 2>/dev/null || true
  if [ ! -e /sys/class/zram-control ] && [ ! -e /dev/zram0 ]; then
    echo "lever-zram-swap: this kernel has no zram module" >&2
    exit 1
  fi
  kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
  size="$((kb / 2))KiB"
  if ! dev=$(zramctl --find --size "$size" --algorithm zstd 2>/dev/null); then
    # A device zramctl added before the algorithm failed is left unused
    # (it holds no memory); the fallback takes the next one.
    dev=$(zramctl --find --size "$size")
  fi
  echo "$dev" > %[1]s
  trap 'swapoff "$dev" 2>/dev/null || true; zramctl --reset "$dev" 2>/dev/null || true; rm -f %[1]s' EXIT
  mkswap -L lever-zram "$dev" >/dev/null
  swapon -p 100 "$dev"
  trap - EXIT
  ;;
stop)
  dev=$(cat %[1]s 2>/dev/null || true)
  [ -n "$dev" ] || exit 0
  swapoff "$dev" 2>/dev/null || true
  zramctl --reset "$dev" 2>/dev/null || true
  rm -f %[1]s
  ;;
*)
  echo "usage: $0 start|stop" >&2
  exit 2
  ;;
esac`, shellSingleQuote(p.ZramDev))
}

func zramUnit(p profilePaths) string {
	return `# Written by lever (guest_profile: small, lever#193). Do not edit: lever apply rewrites it.
[Unit]
Description=lever zram swap (guest_profile: small)
DefaultDependencies=no
Before=swap.target shutdown.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + p.ZramHelper + ` start
ExecStop=` + p.ZramHelper + ` stop

[Install]
WantedBy=swap.target`
}

// profileConvergeShell is convergeShell with a mode argument.
const profileConvergeShell = `converge() {
  if [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; then return 1; fi
  mkdir -p "$(dirname "$1")" || exit 1
  printf '%s\n' "$2" > "$1.lever-tmp" || exit 1
  chmod "$3" "$1.lever-tmp" || exit 1
  mv -f "$1.lever-tmp" "$1" || exit 1
  return 0
}
unit_exists() { systemctl list-unit-files --no-legend --no-pager "$1" 2>/dev/null | grep -q .; }
`

// guestProfileScript: a failed write of lever's own files exits 1; anything
// systemd refuses is a warning, so the bring-up goes on with a guest that is
// merely less trim.
func guestProfileScript(profile string, p profilePaths) string {
	q := shellSingleQuote
	var b strings.Builder
	b.WriteString("set -u\n")
	b.WriteString(profileConvergeShell)
	fmt.Fprintf(&b, "state=%s\n", q(p.State))
	if profile == GuestProfileSmall {
		fmt.Fprintf(&b, "units=%s\n", q(strings.Join(TrimmedUnits, " ")))
		b.WriteString(`mkdir -p "$(dirname "$state")" || exit 1
touch "$state" || exit 1
for u in $units; do
  unit_exists "$u" || continue
  case "$(systemctl is-enabled "$u" 2>/dev/null || true)" in
    enabled|enabled-runtime) ;;
    *) continue ;;
  esac
  if systemctl disable --now "$u" >/dev/null 2>&1; then
    grep -qxF "$u" "$state" || echo "$u" >> "$state" || exit 1
  else
    echo "warn: guest_profile small: could not disable $u"
  fi
done
`)
		fmt.Fprintf(&b, "changed=0\nconverge %s %s 0755 && changed=1\nconverge %s %s 0644 && changed=1\n",
			q(p.ZramHelper), q(zramHelper(p)), q(p.ZramUnit), q(zramUnit(p)))
		fmt.Fprintf(&b, `if [ "$changed" = 1 ]; then systemctl daemon-reload >/dev/null 2>&1 || true; fi
systemctl enable %[1]s >/dev/null 2>&1 || echo %[2]s
if systemctl is-active --quiet %[1]s; then
  if [ "$changed" = 1 ]; then echo %[3]s; fi
elif ! systemctl start %[1]s >/dev/null 2>&1; then
  echo %[4]s
fi
`, ZramUnitName,
			q("warn: guest_profile small: could not enable "+ZramUnitName),
			q("reboot: the zram swap's new settings apply at the next boot"),
			q("warn: guest_profile small: zram swap did not start (does the guest kernel have the zram module? see journalctl -u "+ZramUnitName+"); the guest runs without swap"))
		return b.String()
	}
	// standard: undo what small did, and only that.
	fmt.Fprintf(&b, `if [ -f "$state" ]; then
  left=
  while IFS= read -r u; do
    [ -n "$u" ] || continue
    unit_exists "$u" || continue
    if ! systemctl enable --now "$u" >/dev/null 2>&1; then
      echo "warn: guest_profile standard: could not enable $u again; the next apply tries again"
      left="$left$u
"
    fi
  done < "$state"
  if [ -n "$left" ]; then
    printf '%%s' "$left" > "$state" || exit 1
  else
    rm -f "$state" || exit 1
  fi
fi
if [ -f %[1]s ]; then
  systemctl disable --now %[2]s >/dev/null 2>&1 || echo %[3]s
  rm -f %[1]s %[4]s || exit 1
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
`, q(p.ZramUnit), ZramUnitName,
		q("warn: guest_profile standard: could not stop the zram swap; it is gone at the next boot"),
		q(p.ZramHelper))
	return b.String()
}

// EnsureGuestProfile converges the guest on profile and reports what needs
// the operator. Idempotent; "" skips. Lima only: the caller gates it.
func (g Guest) EnsureGuestProfile(ctx context.Context, profile string) (GuestReport, error) {
	switch profile {
	case "":
		return GuestReport{}, nil
	case GuestProfileSmall, GuestProfileStandard:
	default:
		return GuestReport{}, fmt.Errorf("guest profile %q unknown", profile)
	}
	res, err := g.RootRun(ctx, "bash", "-c", guestProfileScript(profile, realProfilePaths))
	if err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		return GuestReport{}, fmt.Errorf("guest profile %s: %w", profile, err)
	}
	return parseGuestReport(res.Stdout), nil
}
