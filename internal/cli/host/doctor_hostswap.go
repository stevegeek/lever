package host

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

// hostSwapWarnBytes is how much of the VM's memory may sit in host swap
// before doctor warns: past it, the guest stalls on whatever it touches.
const hostSwapWarnBytes = 1 << 30

const hostSwapRestartFix = "restart the VM: `lever stop`, then `lever up` (keeps the manager conversation). lever up starts it in a no-swap systemd user scope when your user lingers (`loginctl enable-linger`) and the user manager can set memory.swap.max; otherwise it warns why not"

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// checkVMHostSwap reports how much of the Lima VM's qemu process the host
// has swapped out, and whether its cgroup lets the host swap it at all. A
// warning only.
func checkVMHostSwap(app *config.App, goos string, s lima.HostSwap, err error) checkResult {
	const name = "VM host swap"
	switch {
	case app.Backend != config.BackendLima:
		return checkResult{name, true, "not applicable (OrbStack manages its machine's memory)", ""}
	case goos != "linux":
		return checkResult{name, true, "not applicable (macOS host)", ""}
	case errors.Is(err, lima.ErrVMNotRunning):
		return checkResult{name, true, "not checked (the VM is not running)", ""}
	case err != nil:
		return checkResult{name, true, "not checked (" + firstLine(err.Error()) + ")", ""}
	}
	detail := fmt.Sprintf("qemu pid %d has %s in host swap; %s", s.PID, gib(s.VmSwap), s.Limit())
	over := s.VmSwap > hostSwapWarnBytes
	if app.HostSwap {
		detail += " (host_swap: true)"
		if over {
			return warnResult(name, detail+": the guest stalls on memory the host swapped out",
				"remove host_swap: true from lever.yaml, then "+hostSwapRestartFix)
		}
		return checkResult{name, true, detail, ""}
	}
	if over {
		return warnResult(name, detail+": the guest stalls on memory the host swapped out", hostSwapRestartFix)
	}
	if !s.NoSwap() {
		return warnResult(name, detail+": the host can swap the VM's memory out", hostSwapRestartFix)
	}
	return checkResult{name, true, detail, ""}
}

func readVMHostSwap(machine string) (lima.HostSwap, error) {
	if machine == "" {
		return lima.HostSwap{}, errors.New("the machine name is unknown")
	}
	return lima.ReadHostSwap(lima.InstanceDir(machine))
}

// asyncPFWait is the guest kernel function a task sleeps in while it waits
// for the host to bring a page back (x86 KVM guests).
const asyncPFWait = "kvm_async_pf_task_wait_schedule"

// asyncPFSampleGap is how long a task must stay asleep in asyncPFWait,
// without one context switch, to count as stuck. A healthy async page fault
// completes in well under a second.
const asyncPFSampleGap = 5

// asyncPFProbeTimeout bounds the probe: a guest wedged on async page faults
// can leave the ssh doctor runs through hanging too.
const asyncPFProbeTimeout = 45 * time.Second

// asyncPFScanScript prints "pid switches comm" for every task in D state in
// asyncPFWait, read with shell builtins so a healthy guest costs no forks
// per process. The wait shows in wchan, or, for a task wchan does not name,
// in the stack root can read.
const asyncPFScanScript = `scan() {
  for d in "$P"/[0-9]*; do
    { read -r line < "$d/stat"; } 2>/dev/null || continue
    rest=${line##*) }
    [ "${rest%% *}" = D ] || continue
    w=
    { read -r w < "$d/wchan"; } 2>/dev/null || true
    if [ "$w" != ` + asyncPFWait + ` ]; then
      grep -qs ` + asyncPFWait + ` "$d/stack" || continue
    fi
    sw=0
    while read -r k v; do
      case "$k" in voluntary_ctxt_switches:|nonvoluntary_ctxt_switches:) sw=$((sw + v)) ;; esac
    done < "$d/status" 2>/dev/null
    { read -r c < "$d/comm"; } 2>/dev/null || c=?
    echo "${d##*/} $sw $c"
  done
}
`

// asyncPFProbeScript samples twice, asyncPFSampleGap apart, only when the
// first sample finds a waiting task, and counts the hung-task reports the
// kernel logged in asyncPFWait this boot (root only). procDir is a test
// seam.
func asyncPFProbeScript(procDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "P=%s\n", shellQuote(procDir))
	b.WriteString(asyncPFScanScript)
	fmt.Fprintf(&b, `if [ "$(id -u)" = 0 ]; then echo root=1; else echo root=0; fi
grep -qwF -- %s "$P/cmdline" 2>/dev/null && echo nokvmapf=1
first=$(scan)
if [ -n "$first" ]; then
  sleep %d
  second=$(scan)
  printf '%%s\n' "$second" | while read -r pid sw c; do
    [ -n "$pid" ] || continue
    printf '%%s\n' "$first" | grep -qxF -- "$pid $sw $c" && echo "stuck=$pid $c"
  done
fi
if [ "$(id -u)" = 0 ]; then
  echo "hung=$(journalctl -k -b 0 -o cat --no-pager 2>/dev/null | grep -c %s)"
fi
`, shellQuote(guest.NoKVMAPFCmdline), asyncPFSampleGap, asyncPFWait)
	return b.String()
}

// asyncPFProbe is what doctor reads about guest tasks stuck on KVM async
// page faults.
type asyncPFProbe struct {
	root     bool       // read as root: every task, and the kernel log
	noKVMAPF bool       // no-kvmapf is on the running kernel's command line
	stuck    []stuckPID // in asyncPFWait in both samples, no context switch between
	hung     int        // hung-task reports in asyncPFWait this boot; -1 when not read
}

type stuckPID struct {
	pid  int
	comm string
}

// commRE: a task name is the guest's (an agent's process can set its own),
// so only a plain short one is shown.
var commRE = regexp.MustCompile(`^[A-Za-z0-9._:+-]{1,15}$`)

const maxStuckShown = 5

func parseAsyncPFProbe(out string) asyncPFProbe {
	p := asyncPFProbe{hung: -1}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "root":
			p.root = v == "1"
		case "nokvmapf":
			p.noKVMAPF = v == "1"
		case "hung":
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				p.hung = n
			}
		case "stuck":
			ps, c, _ := strings.Cut(v, " ")
			pid, err := strconv.Atoi(ps)
			if err != nil || pid <= 0 {
				continue
			}
			if !commRE.MatchString(c) {
				c = "?"
			}
			p.stuck = append(p.stuck, stuckPID{pid, c})
		}
	}
	return p
}

// readAsyncPFProbe runs the probe as root when sudo -n works in the guest:
// a task's wchan and stack are readable only to its owner and root, and the
// container processes are not the run user's.
func readAsyncPFProbe(ctx context.Context, jr proc.Runner) (asyncPFProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, asyncPFProbeTimeout)
	defer cancel()
	const wrapper = `if sudo -n true 2>/dev/null; then exec sudo -n sh -c "$1"; fi; exec sh -c "$1"`
	res, err := jr.Run(ctx, nil, "sh", "-c", wrapper, "sh", asyncPFProbeScript("/proc"))
	if ctx.Err() != nil {
		return asyncPFProbe{}, fmt.Errorf("the guest did not answer within %s", asyncPFProbeTimeout)
	}
	if err != nil {
		return asyncPFProbe{}, err
	}
	if !strings.Contains(res.Stdout, "root=") {
		return asyncPFProbe{}, errors.New("unexpected probe output")
	}
	return parseAsyncPFProbe(res.Stdout), nil
}

const asyncPFFix = "the stuck tasks do not recover: `lever stop` (or `limactl stop -f <vm>` if it hangs), then `lever up`. That reboot also applies no-kvmapf when `lever apply` reported it pending; keep the VM out of host swap (row \"VM host swap\")"

// checkGuestAsyncPF warns when guest tasks are stuck waiting for the host to
// bring back a page it swapped out (KVM async page faults). A warning only.
func checkGuestAsyncPF(backendName string, p asyncPFProbe, perr error) checkResult {
	const name = "guest async page faults"
	if backendName != config.BackendLima {
		return checkResult{name, true, "not applicable (OrbStack: the kernel is OrbStack's)", ""}
	}
	if perr != nil {
		return checkResult{name, true, "not checked (" + firstLine(perr.Error()) + ")", ""}
	}
	var hung string
	if p.hung > 0 {
		hung = fmt.Sprintf("the guest kernel logged %d hung-task line(s) in %s this boot", p.hung, asyncPFWait)
	}
	if len(p.stuck) > 0 {
		var names []string
		for i, s := range p.stuck {
			if i == maxStuckShown {
				names = append(names, fmt.Sprintf("and %d more", len(p.stuck)-maxStuckShown))
				break
			}
			names = append(names, fmt.Sprintf("%s (%d)", s.comm, s.pid))
		}
		detail := fmt.Sprintf("%d guest task(s) in D state in %s, unchanged for %d s: %s", len(p.stuck), asyncPFWait, asyncPFSampleGap, strings.Join(names, ", "))
		if hung != "" {
			detail += "; " + hung
		}
		return warnResult(name, detail, asyncPFFix)
	}
	if hung != "" {
		return warnResult(name, "no task is stuck now, but "+hung,
			"if the guest is slow or tasks hang: `lever stop`, then `lever up`; keep the VM out of host swap (row \"VM host swap\")")
	}
	detail := "no guest task is waiting on a KVM async page fault"
	if p.noKVMAPF {
		detail = "async page faults are off (" + guest.NoKVMAPFCmdline + ")"
	}
	if !p.root {
		detail += "; only the run user's tasks were checked (sudo in the guest refused)"
	}
	return checkResult{name, true, detail, ""}
}
