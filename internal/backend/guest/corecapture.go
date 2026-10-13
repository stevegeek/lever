package guest

import (
	"fmt"
	"strconv"
	"strings"
)

// Tmux server cores. The agent's tmux server holds claude's pane, so every
// Lima guest keeps the core of a crashed tmux server:
//
//   - kernel.core_pattern pipes every core dump to a handler lever installs.
//     A piped dump ignores RLIMIT_CORE (except a limit of exactly 1 byte), so
//     the container's soft limit of 0 does not stop it.
//   - The handler keeps a dump only for a process named "tmux: server" or
//     "tmux", and drops every other one.
//
// Cores land in the guest in a root-only directory (CoreDir, 0700, files
// 0600), never in the agent's container or on the host: a core holds the
// terminal contents of the agent's panes.
//
// The agent is hostile, so everything from the crash is untrusted: the
// process name is only compared, never put in a path; the pid and time must
// be digits. The agent can crash "tmux: server" processes at will, so the
// handler bounds the disk it fills: one dump at a time (non-blocking lock),
// each cut at CoreMaxBytes, at most CoreKeep cores, none while the newest is
// younger than CoreMinGap. The kernel runs the handler as root outside the
// container's limits, so kernel.core_pipe_limit caps concurrent handlers. A
// kept core may be a fake: it is untrusted diagnostic material, not proof.
// The handler writes only into a real, root-owned directory (no symlink).
//
// Ubuntu's apport is masked (its service rewrites core_pattern at boot).
// Lima only: kernel.core_pattern is not namespaced, and an OrbStack machine
// shares OrbStack's one kernel.

const (
	CoreHandlerPath = "/usr/local/libexec/lever-core-handler"
	CoreSysctlPath  = "/etc/sysctl.d/99-lever-core.conf"
	CoreDir         = "/var/lib/lever/cores"
)

// CoreMaxBytes caps one kept core; CoreKeep is how many are kept.
// CoreMinGap is the least time between two kept cores, in seconds.
// CorePipeLimit is kernel.core_pipe_limit.
const (
	CoreMaxBytes  = 256 << 20
	CoreKeep      = 2
	CoreMinGap    = 60
	CorePipeLimit = 1
)

// corePaths is a test seam.
type corePaths struct {
	Handler, Sysctl, Dir string
	OwnerUID             int // the core directory's owner: root in the guest
}

var realCorePaths = corePaths{Handler: CoreHandlerPath, Sysctl: CoreSysctlPath, Dir: CoreDir, OwnerUID: 0}

// corePattern puts the process name last: it may hold spaces, and older
// kernels split it into words.
func corePattern(handler string) string {
	return "|" + handler + " %P %t %e"
}

// coreHandlerScript is run by the kernel as root for every core dump in the
// guest. dir must be a real directory owned by ownerUID (root in the guest).
func coreHandlerScript(dir string, ownerUID, maxBytes, keep, minGap int) string {
	q := shellSingleQuote
	return `#!/bin/sh
# Written by lever (tmux server cores, lever#201). Do not edit: lever apply rewrites it.
# The kernel runs this as root for every core dump in the guest (kernel.core_pattern).
# It keeps the core of a crashed tmux server and drops every other one.
# Arguments: global pid, dump time, process name (the process's own choice: compared, never used in a path).
PATH=/usr/sbin:/usr/bin:/sbin:/bin
dir=` + q(dir) + `
max=` + strconv.Itoa(maxBytes) + `
keep=` + strconv.Itoa(keep) + `
gap=` + strconv.Itoa(minGap) + `
[ $# -ge 3 ] || exit 0
pid=$1
t=$2
shift 2
case "$*" in
'tmux: server'|tmux) ;;
*) exit 0 ;;
esac
case "$pid" in ''|*[!0-9]*) exit 0 ;; esac
case "$t" in ''|*[!0-9]*) exit 0 ;; esac
umask 077
mkdir -p "$dir" || exit 0
[ -d "$dir" ] && [ ! -L "$dir" ] && [ "$(stat -c %u "$dir")" = ` + strconv.Itoa(ownerUID) + ` ] || exit 0
chmod 0700 "$dir" || exit 0
exec 9>>"$dir/.lock" || exit 0
flock -n 9 || exit 0
rm -f "$dir"/*.part
# At most one kept core per $gap seconds: an agent that crashes its tmux server in a loop keeps
# one core a minute. A core dated in the future (the clock went back) does not hold dumps off.
now=$(date +%s) || exit 0
for f in $(ls -1 "$dir" | grep -E '^core\.tmux\.[0-9]+\.[0-9]+$'); do
  m=$(stat -c %Y "$dir/$f") || exit 0
  age=$((now - m))
  [ "$age" -lt 0 ] || [ "$age" -ge "$gap" ] || exit 0
done
ls -1 "$dir" | grep -E '^core\.tmux\.[0-9]+\.[0-9]+$' | sort -t . -k 3,3nr -k 4,4nr | tail -n +"$keep" | while read -r f; do
  rm -f "$dir/$f"
done
f="$dir/core.tmux.$t.$pid"
head -c "$max" > "$f.part" || exit 0
mv -f "$f.part" "$f" || exit 0
logger -t lever-core "kept the core of a crashed tmux server (pid $pid) in $f" 2>/dev/null || true
exit 0
`
}

// coreCaptureScript sets core_pattern live on every run, since apport or a
// package upgrade can change it. A failed write exits 1; a failed sysctl or
// apport disable is a warning.
func coreCaptureScript(p corePaths) string {
	q := shellSingleQuote
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(convergeShell)
	// The handler is written executable before it is moved into place: the
	// live core_pattern may already name it.
	fmt.Fprintf(&b, "h=%s\nc=%s\n", q(p.Handler), q(strings.TrimSuffix(coreHandlerScript(p.Dir, p.OwnerUID, CoreMaxBytes, CoreKeep, CoreMinGap), "\n")))
	b.WriteString(`if ! [ -f "$h" ] || [ "$(cat "$h")" != "$c" ]; then
  mkdir -p "$(dirname "$h")" || exit 1
  printf '%s\n' "$c" > "$h.lever-tmp" || exit 1
  chmod 0755 "$h.lever-tmp" || exit 1
  mv -f "$h.lever-tmp" "$h" || exit 1
fi
`)
	fmt.Fprintf(&b, "mkdir -p %s || exit 1\nchmod 0700 %s || exit 1\n", q(p.Dir), q(p.Dir))
	fmt.Fprintf(&b, "converge %s %s || true\n", q(p.Sysctl), q(coreSysctlFile(p.Handler)))
	fmt.Fprintf(&b, `if command -v systemctl >/dev/null 2>&1 && [ "$(systemctl is-enabled apport.service 2>/dev/null)" != masked ] &&
  systemctl cat apport.service >/dev/null 2>&1; then
  systemctl stop apport.service >/dev/null 2>&1 || true
  systemctl mask apport.service >/dev/null 2>&1 || echo %s
fi
`, q("warn: could not disable apport; it sets its own core_pattern at boot, until lever apply or up sets lever's again"))
	// The pipe limit first: core_pattern then pipes dumps with the bound
	// already in place.
	lim := "kernel.core_pipe_limit=" + strconv.Itoa(CorePipeLimit)
	fmt.Fprintf(&b, "sysctl -q -w %s >/dev/null 2>&1 || echo %s\n", q(lim), q("warn: could not set kernel.core_pipe_limit in the running guest kernel; crashing processes can run any number of core handlers at once"))
	kv := "kernel.core_pattern=" + corePattern(p.Handler)
	fmt.Fprintf(&b, "sysctl -q -w %s >/dev/null 2>&1 || echo %s\n", q(kv), q("warn: could not set kernel.core_pattern in the running guest kernel; a tmux server crash leaves no core"))
	return b.String()
}

func coreSysctlFile(handler string) string {
	return "# Written by lever (tmux server cores, lever#201). Do not edit: lever apply rewrites it.\n" +
		"# Pipe core dumps to lever's handler, which keeps a crashed tmux server's core in " + CoreDir + ".\n" +
		"# Run at most " + strconv.Itoa(CorePipeLimit) + " handler at a time; the kernel skips a dump past it.\n" +
		"kernel.core_pipe_limit = " + strconv.Itoa(CorePipeLimit) + "\n" +
		"kernel.core_pattern = " + corePattern(handler)
}
