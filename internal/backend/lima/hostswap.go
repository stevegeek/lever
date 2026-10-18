package lima

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// Keeping the VM out of host swap (Linux hosts). A guest whose memory the
// host swapped out stalls on every touch of it, and with KVM async page
// faults a lost "page ready" notice hangs the faulting guest task for good.
// limactl start runs in a transient systemd user scope with
// MemorySwapMax=0. The host agent and qemu it starts stay in that cgroup
// although they daemonize: a cgroup is inherited across fork and setsid,
// and the scope lives on while any of its processes does.
//
// The scope belongs to the user's systemd manager, which stops, and kills
// what it holds, at the user's last logout unless the user lingers. A
// session-started VM survives that logout today, so without lingering the
// VM is started the old way. Any doubt starts it the old way with a
// warning: the swap limit is never a reason not to start the VM.

// hostGOOS, hostUID and the roots below are test seams.
var (
	hostGOOS   = runtime.GOOS
	hostUID    = os.Getuid
	procRoot   = "/proc"
	cgroupRoot = "/sys/fs/cgroup"
)

// swapScopeProbeTimeout bounds the linger and scope probes: a user manager
// that does not answer must not hold up the VM start.
const swapScopeProbeTimeout = 20 * time.Second

// NoSwapScopePrefix is the argv that runs a command in a new transient user
// scope that may not use swap.
func NoSwapScopePrefix(machine string) []string {
	return []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--property=MemorySwapMax=0", "--description=lever VM " + machine, "--"}
}

// swapScopeProbe prints the memory.swap.max of the cgroup it runs in, or
// nothing. It always exits 0, so a failed probe run is systemd-run's.
const swapScopeProbe = `cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/memory.swap.max" 2>/dev/null || true`

const lingerAdvice = "loginctl enable-linger"

// noSwapScope reports whether limactl start can run under
// NoSwapScopePrefix. why is "" when the host has no such scope to offer
// (not Linux) and the caller should say nothing.
func noSwapScope(ctx context.Context, r proc.Runner) (ok bool, why string) {
	if hostGOOS != "linux" {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, swapScopeProbeTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "loginctl", "show-user", strconv.Itoa(hostUID()), "--property=Linger", "--value")
	if err = exitErr(err, res); err != nil {
		return false, "could not read whether your user lingers (loginctl: " + briefErr(err, res) + "); a VM in a user scope would stop at your last logout without it. To use the scope: " + lingerAdvice
	}
	if strings.TrimSpace(res.Stdout) != "yes" {
		return false, "your user does not linger, so the systemd user manager, and a VM in its scope, would stop at your last logout. To use the scope: " + lingerAdvice
	}
	probe := append(NoSwapScopePrefix("probe"), "sh", "-c", swapScopeProbe)
	res, err = r.Run(ctx, nil, probe[0], probe[1:]...)
	if err = exitErr(err, res); err != nil {
		return false, "systemd-run --user --scope failed (" + briefErr(err, res) + ")"
	}
	if got := strings.TrimSpace(res.Stdout); got != "0" {
		if got == "" {
			got = "unreadable"
		}
		return false, "a systemd user scope does not get memory.swap.max=0 (got " + got + "; the user manager may not have the memory controller delegated, or swap accounting is off)"
	}
	return true, ""
}

func exitErr(err error, res proc.Result) error {
	if err == nil && res.Code != 0 {
		return fmt.Errorf("exit %d", res.Code)
	}
	return err
}

func briefErr(err error, res proc.Result) string {
	s := strings.TrimSpace(res.Stderr)
	if s == "" {
		s = err.Error()
	}
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// scopeNotStarted reports whether systemd-run itself failed, before it ran
// limactl: its own messages on stderr, which limactl never prints.
func scopeNotStarted(res proc.Result) bool {
	e := res.Stderr
	return strings.Contains(e, "transient scope unit") ||
		(strings.Contains(e, "Failed to") && strings.Contains(e, " bus"))
}

// startVM runs limactl start, in a no-swap scope unless hostSwap or the
// host cannot give one. A scope that fails to start is retried without it.
func (l *Lima) startVM(ctx context.Context, hostSwap bool, instDir string, warn func(string, ...any)) error {
	start := []string{"limactl", "start", "--tty=false", l.Machine()}
	scoped := false
	if !hostSwap {
		ok, why := noSwapScope(ctx, l.Runner())
		switch {
		case ok:
			scoped = true
		case why != "":
			warn("jail: warning: starting the VM without the host swap limit: %s; the host can swap the VM's memory out (set host_swap: true to start it this way without this warning)", why)
		}
	}
	if !scoped {
		res, err := l.Runner().Run(ctx, nil, start[0], start[1:]...)
		return exitErr(err, res)
	}
	argv := append(NoSwapScopePrefix(l.Machine()), start...)
	res, err := l.Runner().Run(ctx, nil, argv[0], argv[1:]...)
	err = exitErr(err, res)
	if err != nil && scopeNotStarted(res) {
		warn("jail: warning: starting the VM without the host swap limit: systemd-run could not start the scope (%s); the host can swap the VM's memory out", briefErr(err, res))
		res, err = l.Runner().Run(ctx, nil, start[0], start[1:]...)
		return exitErr(err, res)
	}
	if err == nil {
		if s, serr := ReadHostSwap(instDir); serr == nil && !s.NoSwap() {
			warn("jail: warning: the VM's qemu (pid %d) is not in a cgroup with memory.swap.max=0 (%s); the host can swap the VM's memory out (`lever doctor`, row \"VM host swap\")", s.PID, s.describeLimit())
		}
	}
	return err
}

// HostSwap is what the host's /proc and cgroup files say about a running
// Lima VM's qemu process.
type HostSwap struct {
	PID    int
	VmSwap int64 // bytes of qemu's memory in host swap
	// SwapMax is "0" when qemu's cgroup or an ancestor has
	// memory.swap.max 0, else the nearest other limit (a byte count), else
	// "max"; "" when none is readable (cgroup v1, no swap accounting).
	SwapMax string
	// Cgroup is the cgroup v2 path of the qemu process; "" when unknown.
	Cgroup string
}

// NoSwap reports whether the kernel keeps qemu's memory out of swap.
func (s HostSwap) NoSwap() bool { return s.SwapMax == "0" }

func (s HostSwap) describeLimit() string {
	switch {
	case s.SwapMax == "":
		return "memory.swap.max unreadable"
	case s.Cgroup == "":
		return "memory.swap.max=" + s.SwapMax
	}
	return "cgroup " + s.Cgroup + ", memory.swap.max=" + s.SwapMax
}

// ErrVMNotRunning is ReadHostSwap's error when no qemu process belongs to
// the instance.
var ErrVMNotRunning = errors.New("the VM's qemu process is not running")

// InstanceDir is the Lima instance directory of machine.
func InstanceDir(machine string) string {
	home := os.Getenv("LIMA_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".lima")
	}
	return filepath.Join(home, machine)
}

// ReadHostSwap reads the qemu process of the Lima instance at instDir. A
// stale qemu.pid, one that names no qemu process now, is ErrVMNotRunning.
func ReadHostSwap(instDir string) (HostSwap, error) {
	if hostGOOS != "linux" {
		return HostSwap{}, fmt.Errorf("host swap is read on Linux only")
	}
	b, err := readHostFile(filepath.Join(instDir, "qemu.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return HostSwap{}, ErrVMNotRunning
	}
	if err != nil {
		return HostSwap{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return HostSwap{}, fmt.Errorf("%s: not a pid", filepath.Join(instDir, "qemu.pid"))
	}
	pdir := filepath.Join(procRoot, strconv.Itoa(pid))
	comm, err := readHostFile(filepath.Join(pdir, "comm"))
	if err != nil || !strings.HasPrefix(string(comm), "qemu") {
		return HostSwap{}, ErrVMNotRunning
	}
	status, err := readHostFile(filepath.Join(pdir, "status"))
	if err != nil {
		return HostSwap{}, err
	}
	s := HostSwap{PID: pid, VmSwap: -1}
	for _, line := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "VmSwap:"); ok {
			f := strings.Fields(v)
			if len(f) == 2 && f[1] == "kB" {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil && n >= 0 {
					s.VmSwap = n << 10
				}
			}
		}
	}
	if s.VmSwap < 0 {
		return HostSwap{}, fmt.Errorf("%s: no VmSwap line", filepath.Join(pdir, "status"))
	}
	cg, err := readHostFile(filepath.Join(pdir, "cgroup"))
	if err != nil {
		return s, nil
	}
	for _, line := range strings.Split(string(cg), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(p, "/") {
			s.Cgroup = path.Clean(p)
		}
	}
	if s.Cgroup == "" {
		return s, nil
	}
	for dir := s.Cgroup; ; dir = path.Dir(dir) {
		if v, err := readHostFile(filepath.Join(cgroupRoot, dir, "memory.swap.max")); err == nil {
			switch got := strings.TrimSpace(string(v)); {
			case got == "0":
				s.SwapMax = "0"
				return s, nil
			case s.SwapMax == "" || s.SwapMax == "max":
				s.SwapMax = got
			}
		}
		if dir == "/" {
			return s, nil
		}
	}
}
