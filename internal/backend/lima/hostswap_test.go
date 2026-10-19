package lima

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
)

// TestMain keeps every test off the host's systemd and /proc: a test that
// wants the Linux path sets it with onLinux.
func TestMain(m *testing.M) {
	hostGOOS = "other"
	hostUID = func() int { return 1000 }
	dir, err := os.MkdirTemp("", "lever-lima-proc-")
	if err != nil {
		panic(err)
	}
	procRoot = dir
	if err := os.WriteFile(filepath.Join(dir, "swaps"), []byte(swapsWithZram), 0o644); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const swapsWithZram = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n/dev/zram0                              partition\t25165820\t19300000\t100\n"

func onLinux(t *testing.T) {
	t.Helper()
	was := hostGOOS
	hostGOOS = "linux"
	t.Cleanup(func() { hostGOOS = was })
}

var (
	lingerKey     = "loginctl show-user 1000 --property=Linger --value"
	probeKey      = strings.Join(NoSwapScopePrefix("probe"), " ") + " sh -c"
	scopedStart   = strings.Join(NoSwapScopePrefix(vm), " ") + " limactl start --tty=false " + vm
	unscopedStart = "limactl start --tty=false " + vm
)

func TestNoSwapScope(t *testing.T) {
	for _, c := range []struct {
		name    string
		linux   bool
		script  map[string]proc.Result
		ok      bool
		why     string // substring; "" wants no reason
		noProbe bool
	}{
		{name: "not linux", noProbe: true},
		{name: "lingers, scope limits swap", linux: true, ok: true,
			script: map[string]proc.Result{lingerKey: {Stdout: "yes\n"}, probeKey: {Stdout: "0\n"}}},
		{name: "no linger", linux: true, why: "does not linger", noProbe: true,
			script: map[string]proc.Result{lingerKey: {Stdout: "no\n"}}},
		{name: "loginctl fails", linux: true, why: "could not read whether your user lingers", noProbe: true,
			script: map[string]proc.Result{lingerKey: {Code: 1, Stderr: "Failed to get user: No such user\n"}}},
		{name: "memory controller not delegated", linux: true, why: "got unreadable",
			script: map[string]proc.Result{lingerKey: {Stdout: "yes\n"}, probeKey: {Stdout: ""}}},
		{name: "scope keeps swap", linux: true, why: "got max",
			script: map[string]proc.Result{lingerKey: {Stdout: "yes\n"}, probeKey: {Stdout: "max\n"}}},
		{name: "no user bus", linux: true, why: "systemd-run --user --scope failed (Failed to connect to bus",
			script: map[string]proc.Result{lingerKey: {Stdout: "yes\n"}, probeKey: {Code: 1, Stderr: "Failed to connect to bus: No medium found\n"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.linux {
				onLinux(t)
			}
			f := proc.NewFakeRunner()
			for k, v := range c.script {
				f.Script(k, v)
			}
			ok, why := noSwapScope(context.Background(), f)
			if ok != c.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, why, c.ok)
			}
			if (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
				t.Errorf("why = %q, want %q", why, c.why)
			}
			if c.noProbe && f.Called(proc.Subcommand("systemd-run", "--user")) {
				t.Errorf("must not probe a scope: %+v", f.Calls)
			}
		})
	}
}

func TestNoSwapScopeHostWithoutSwapSaysNothing(t *testing.T) {
	onLinux(t)
	dir := t.TempDir()
	was := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = was })
	if err := os.WriteFile(filepath.Join(dir, "swaps"), []byte("Filename\tType\tSize\tUsed\tPriority\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := proc.NewFakeRunner()
	if ok, why := noSwapScope(context.Background(), f); ok || why != "" || len(f.Calls) != 0 {
		t.Errorf("ok=%v why=%q calls=%+v", ok, why, f.Calls)
	}
	if HostHasSwap() {
		t.Error("a header-only /proc/swaps is no swap")
	}
}

func TestScopeNotStarted(t *testing.T) {
	for stderr, want := range map[string]bool{
		"Failed to start transient scope unit: Unit run-r1.scope already exists.\n": true,
		"Failed to connect to bus: No medium found\n":                               true,
		"time=x level=info msg=\"Failed to connect to bus\"\n":                      false,
		"level=fatal msg=boom\nFailed to start transient scope unit\n":              false,
		"": false,
	} {
		if got := scopeNotStarted(proc.Result{Stderr: stderr}); got != want {
			t.Errorf("%q: got %v, want %v", stderr, got, want)
		}
	}
}

// ensureUpStopped runs EnsureUp on a stopped VM and returns the warnings.
func ensureUpStopped(t *testing.T, f *proc.FakeRunner, hostSwap bool) ([]string, error) {
	t.Helper()
	limaVersionScript(f)
	scriptList(f, listStopped)
	scriptRealizedConfig(f, vm, matchingRealizedConfigJSON(vm, tree))
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsV4)
	var warns []string
	warn := func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) }
	l := New(f, vm, common.Options{})
	err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, HostSwap: hostSwap, Warn: warn})
	return warns, err
}

func startCalls(f *proc.FakeRunner) []string {
	var out []string
	for _, c := range f.Calls {
		if a := c.Argv(); strings.Contains(a, "limactl start") {
			out = append(out, a)
		}
	}
	return out
}

func TestEnsureUpStartsVMInNoSwapScope(t *testing.T) {
	onLinux(t)
	f := proc.NewFakeRunner()
	f.Script(lingerKey, proc.Result{Stdout: "yes\n"})
	f.Script(probeKey, proc.Result{Stdout: "0\n"})
	f.Script(scopedStart, proc.Result{})
	warns, err := ensureUpStopped(t, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := startCalls(f); len(got) != 1 || got[0] != scopedStart {
		t.Fatalf("starts = %q, want the scoped one", got)
	}
	for _, w := range warns {
		if strings.Contains(w, "swap") {
			t.Errorf("unexpected warning %q", w)
		}
	}
}

func TestEnsureUpNoScopeStartsUnscopedAndWarns(t *testing.T) {
	onLinux(t)
	f := proc.NewFakeRunner()
	f.Script(lingerKey, proc.Result{Stdout: "no\n"})
	f.Script(unscopedStart, proc.Result{})
	warns, err := ensureUpStopped(t, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := startCalls(f); len(got) != 1 || got[0] != unscopedStart {
		t.Fatalf("starts = %q, want the plain one", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "without the host swap limit") || !strings.Contains(warns[0], "loginctl enable-linger") {
		t.Errorf("warns = %q", warns)
	}
}

func TestEnsureUpHostSwapTrueSkipsScope(t *testing.T) {
	onLinux(t)
	f := proc.NewFakeRunner()
	f.Script(unscopedStart, proc.Result{})
	warns, err := ensureUpStopped(t, f, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Called(proc.Subcommand("loginctl", "show-user")) || f.Called(proc.Subcommand("systemd-run", "--user")) {
		t.Errorf("host_swap: true must not probe: %+v", f.Calls)
	}
	if got := startCalls(f); len(got) != 1 || got[0] != unscopedStart {
		t.Fatalf("starts = %q", got)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %q", warns)
	}
}

func TestEnsureUpOffLinuxStartsPlainWithoutWarning(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script(unscopedStart, proc.Result{})
	warns, err := ensureUpStopped(t, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Called(proc.Subcommand("loginctl", "show-user")) {
		t.Error("macOS has no user scope to probe")
	}
	if len(warns) != 0 {
		t.Errorf("warns = %q", warns)
	}
}

func TestEnsureUpScopeThatFailsToStartRetriesPlain(t *testing.T) {
	onLinux(t)
	f := proc.NewFakeRunner()
	f.Script(lingerKey, proc.Result{Stdout: "yes\n"})
	f.Script(probeKey, proc.Result{Stdout: "0\n"})
	f.Script(scopedStart, proc.Result{Code: 1, Stderr: "Failed to start transient scope unit: Unit run-r1.scope already exists.\n"})
	f.Script(unscopedStart, proc.Result{})
	warns, err := ensureUpStopped(t, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := startCalls(f); len(got) != 2 || got[0] != scopedStart || got[1] != unscopedStart {
		t.Fatalf("starts = %q", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "could not start the scope") {
		t.Errorf("warns = %q", warns)
	}
}

// A failure of limactl itself under the scope is not retried: limactl start
// can take minutes, and a second run would fail the same way.
func TestEnsureUpScopedLimactlFailureIsReturned(t *testing.T) {
	onLinux(t)
	f := proc.NewFakeRunner()
	f.Script(lingerKey, proc.Result{Stdout: "yes\n"})
	f.Script(probeKey, proc.Result{Stdout: "0\n"})
	f.Script(scopedStart, proc.Result{Code: 1, Stderr: "level=fatal msg=\"did not receive an event with the \\\"running\\\" status\"\n"})
	if _, err := ensureUpStopped(t, f, false); err == nil || !strings.Contains(err.Error(), "limactl start") {
		t.Fatalf("err = %v", err)
	}
	if got := startCalls(f); len(got) != 1 {
		t.Fatalf("starts = %q", got)
	}
}

// hostSwapRoot lays out a fake instance dir, /proc and cgroup tree.
type hostSwapRoot struct {
	t                  *testing.T
	inst, proc, cgroup string
}

func newHostSwapRoot(t *testing.T) *hostSwapRoot {
	t.Helper()
	onLinux(t)
	root := t.TempDir()
	r := &hostSwapRoot{t: t, inst: filepath.Join(root, "inst"), proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup")}
	wasProc, wasCg := procRoot, cgroupRoot
	procRoot, cgroupRoot = r.proc, r.cgroup
	t.Cleanup(func() { procRoot, cgroupRoot = wasProc, wasCg })
	return r
}

func (r *hostSwapRoot) write(path, body string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

const scopeCgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/run-r1.scope"

func (r *hostSwapRoot) qemu(pid, comm, swapKB, cgroup string) {
	r.write(filepath.Join(r.inst, "qemu.pid"), pid+"\n")
	r.write(filepath.Join(r.proc, pid, "comm"), comm+"\n")
	r.write(filepath.Join(r.proc, pid, "status"), "Name:\t"+comm+"\nVmRSS:\t 1153024 kB\nVmSwap:\t "+swapKB+" kB\nThreads:\t9\n")
	r.write(filepath.Join(r.proc, pid, "cgroup"), "0::"+cgroup+"\n")
}

func TestReadHostSwap(t *testing.T) {
	t.Run("scope limits swap", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.qemu("4242", "qemu-system-x86", "0", scopeCgroup)
		r.write(filepath.Join(r.cgroup, scopeCgroup, "memory.swap.max"), "0\n")
		r.write(filepath.Join(r.cgroup, "/user.slice/memory.swap.max"), "max\n")
		s, err := ReadHostSwap(r.inst)
		if err != nil {
			t.Fatal(err)
		}
		if s.PID != 4242 || s.VmSwap != 0 || !s.NoSwap() || s.Cgroup != scopeCgroup {
			t.Errorf("got %+v", s)
		}
	})
	t.Run("session scope swapped out", func(t *testing.T) {
		r := newHostSwapRoot(t)
		cg := "/user.slice/user-1000.slice/session-3.scope"
		r.qemu("4242", "qemu-system-x86", "20132659", cg)
		r.write(filepath.Join(r.cgroup, cg, "memory.swap.max"), "max\n")
		r.write(filepath.Join(r.cgroup, "/user.slice/user-1000.slice/memory.swap.max"), "max\n")
		s, err := ReadHostSwap(r.inst)
		if err != nil {
			t.Fatal(err)
		}
		if s.VmSwap != 20132659<<10 || s.NoSwap() || s.SwapMax != "max" {
			t.Errorf("got %+v", s)
		}
	})
	t.Run("an ancestor at 0 wins over a nearer limit", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.qemu("7", "qemu-system-aarch64", "0", scopeCgroup)
		r.write(filepath.Join(r.cgroup, scopeCgroup, "memory.swap.max"), "1073741824\n")
		r.write(filepath.Join(r.cgroup, "/user.slice/user-1000.slice/memory.swap.max"), "0\n")
		if s, err := ReadHostSwap(r.inst); err != nil || !s.NoSwap() {
			t.Errorf("got %+v, %v", s, err)
		}
	})
	t.Run("no swap accounting", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.qemu("7", "qemu-system-x86", "12", scopeCgroup)
		s, err := ReadHostSwap(r.inst)
		if err != nil || s.SwapMax != "" || s.NoSwap() || s.VmSwap != 12<<10 {
			t.Errorf("got %+v, %v", s, err)
		}
	})
	t.Run("no pid file", func(t *testing.T) {
		r := newHostSwapRoot(t)
		if _, err := ReadHostSwap(r.inst); !errors.Is(err, ErrVMNotRunning) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("stale pid names another process", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.qemu("7", "bash", "0", scopeCgroup)
		if _, err := ReadHostSwap(r.inst); !errors.Is(err, ErrVMNotRunning) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("garbage pid", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.write(filepath.Join(r.inst, "qemu.pid"), "../1\n")
		if _, err := ReadHostSwap(r.inst); err == nil || errors.Is(err, ErrVMNotRunning) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("off linux", func(t *testing.T) {
		hostGOOS = "darwin"
		if _, err := ReadHostSwap(t.TempDir()); err == nil {
			t.Error("want an error")
		}
	})
}

// The probe's scope must be a real no-swap scope, and the shell it runs
// must read the cgroup the scope put it in.
func TestNoSwapScopePrefixShape(t *testing.T) {
	p := strings.Join(NoSwapScopePrefix(vm), " ")
	for _, want := range []string{"systemd-run --user --scope", "--property=MemorySwapMax=0", "--collect", " --"} {
		if !strings.Contains(p, want) {
			t.Errorf("prefix %q lacks %q", p, want)
		}
	}
	if !strings.Contains(swapScopeProbe, "/proc/self/cgroup") || !strings.Contains(swapScopeProbe, "memory.swap.max") {
		t.Errorf("probe = %q", swapScopeProbe)
	}
}

func TestWarnRunningVMSwap(t *testing.T) {
	const swaps = "Filename\tType\tSize\tUsed\tPriority\n/dev/zram0\tpartition\t8388604\t0\t100\n"
	cases := []struct {
		name     string
		hostSwap bool
		limit    string
		swaps    string
		want     bool
	}{
		{"unscoped VM warns", false, "max", swaps, true},
		{"scoped VM is quiet", false, "0", swaps, false},
		{"host_swap true is quiet", true, "max", swaps, false},
		{"host without swap is quiet", false, "max", "Filename\tType\tSize\tUsed\tPriority\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHostSwapRoot(t)
			r.write(filepath.Join(r.proc, "swaps"), c.swaps)
			r.qemu("4242", "qemu-system-x86", "1024", scopeCgroup)
			r.write(filepath.Join(r.cgroup, scopeCgroup, "memory.swap.max"), c.limit+"\n")
			var got []string
			warnRunningVMSwap(c.hostSwap, r.inst, func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) })
			if (len(got) == 1) != c.want || len(got) > 1 {
				t.Fatalf("warnings = %q, want one: %v", got, c.want)
			}
			if c.want && !strings.Contains(got[0], "pid 4242") {
				t.Fatalf("warning = %q", got[0])
			}
		})
	}
	t.Run("VM with no qemu.pid is quiet", func(t *testing.T) {
		r := newHostSwapRoot(t)
		r.write(filepath.Join(r.proc, "swaps"), swaps)
		warnRunningVMSwap(false, r.inst, func(f string, a ...any) { t.Fatalf("warned: "+f, a...) })
	})
}
