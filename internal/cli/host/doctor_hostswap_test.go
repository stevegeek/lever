package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

func TestCheckVMHostSwap(t *testing.T) {
	lim := &config.App{Backend: config.BackendLima}
	opted := &config.App{Backend: config.BackendLima, HostSwap: true}
	scoped := lima.HostSwap{PID: 42, VmSwap: 0, SwapMax: "0", Cgroup: "/user.slice/x.scope"}
	session := lima.HostSwap{PID: 42, VmSwap: 64 << 20, SwapMax: "max", Cgroup: "/user.slice/session-3.scope"}
	swapped := lima.HostSwap{PID: 42, VmSwap: 19 << 30, SwapMax: "max", Cgroup: "/user.slice/session-3.scope"}
	for _, c := range []struct {
		name    string
		app     *config.App
		goos    string
		s       lima.HostSwap
		err     error
		ok      bool // ok with no fix
		detail  string
		wantFix string
	}{
		{"orbstack", &config.App{Backend: config.BackendOrbstack}, "darwin", lima.HostSwap{}, nil, true, "not applicable", ""},
		{"macos", lim, "darwin", lima.HostSwap{}, nil, true, "not applicable (macOS host)", ""},
		{"vm down", lim, "linux", lima.HostSwap{}, lima.ErrVMNotRunning, true, "not running", ""},
		{"read error", lim, "linux", lima.HostSwap{}, errors.New("permission denied"), true, "not checked (permission denied)", ""},
		{"scoped", lim, "linux", scoped, nil, true, "qemu pid 42 has 0.0 GiB in host swap; cgroup /user.slice/x.scope, memory.swap.max=0", ""},
		{"unscoped", lim, "linux", session, nil, false, "the host can swap the VM's memory out", "loginctl enable-linger"},
		{"swapped out", lim, "linux", swapped, nil, false, "19.0 GiB in host swap", "`lever stop`, then `lever up`"},
		{"unscoped, host without swap", lim, "linux", session, nil, true, "(the host has no swap device)", ""},
		{"opted in, low", opted, "linux", session, nil, true, "(host_swap: true)", ""},
		{"opted in, swapped out", opted, "linux", swapped, nil, false, "stalls", "remove host_swap: true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hostHasSwap := !strings.Contains(c.name, "without swap")
			r := checkVMHostSwap(c.app, c.goos, c.s, hostHasSwap, c.err)
			if r.name != "VM host swap" || !r.ok {
				t.Fatalf("a warning at worst: %+v", r)
			}
			if c.ok != (r.fix == "") {
				t.Errorf("clean = %v, fix %q", c.ok, r.fix)
			}
			if !strings.Contains(r.detail, c.detail) || !strings.Contains(r.fix, c.wantFix) {
				t.Errorf("got %+v, want detail %q fix %q", r, c.detail, c.wantFix)
			}
		})
	}
}

// asyncPFSandbox is a fake /proc the probe script scans, with a sleep stub
// that runs between the two samples.
type asyncPFSandbox struct {
	t         *testing.T
	proc, bin string
}

func newAsyncPFSandbox(t *testing.T) *asyncPFSandbox {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	s := &asyncPFSandbox{t: t, proc: filepath.Join(root, "proc"), bin: filepath.Join(root, "bin")}
	s.write(filepath.Join(s.proc, "cmdline"), "BOOT_IMAGE=/vmlinuz ro console=ttyS0\n")
	s.write(filepath.Join(s.bin, "sleep"), "#!/bin/sh\n[ -x \"$(dirname \"$0\")/between\" ] && \"$(dirname \"$0\")/between\"\nexit 0\n")
	if err := os.Chmod(filepath.Join(s.bin, "sleep"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *asyncPFSandbox) write(path, body string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// task lays out /proc/<pid> with a stat whose comm has a space and a
// parenthesis, as a hostile process name may.
func (s *asyncPFSandbox) task(pid, comm, state, wchan, switches string) {
	d := filepath.Join(s.proc, pid)
	s.write(filepath.Join(d, "stat"), pid+" ("+comm+") "+state+" 1 1 1 0 -1\n")
	s.write(filepath.Join(d, "wchan"), wchan)
	s.write(filepath.Join(d, "status"), "Name:\t"+comm+"\nState:\t"+state+"\nvoluntary_ctxt_switches:\t"+switches+"\nnonvoluntary_ctxt_switches:\t3\n")
	s.write(filepath.Join(d, "comm"), comm+"\n")
}

func (s *asyncPFSandbox) between(body string) {
	s.write(filepath.Join(s.bin, "between"), "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(filepath.Join(s.bin, "between"), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *asyncPFSandbox) run() (asyncPFProbe, string) {
	s.t.Helper()
	cmd := exec.Command("sh", "-c", asyncPFProbeScript(s.proc))
	cmd.Env = []string{"PATH=" + s.bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("probe failed: %v\n%s", err, out)
	}
	return parseAsyncPFProbe(string(out)), string(out)
}

func TestAsyncPFProbeScriptFindsStuckTasks(t *testing.T) {
	s := newAsyncPFSandbox(t)
	s.task("101", "claude", "D", asyncPFWait, "40")
	s.task("102", "qemu-system-x86", "D", asyncPFWait, "7")
	s.task("103", "sshd", "S", asyncPFWait, "9")        // asleep, not D
	s.task("104", "kworker", "D", "io_schedule", "1")   // D, other wait
	s.task("105", "a) D (x", "D", asyncPFWait, "2")     // name tries to fake the state field
	s.task("107", `a\nroot=7\c`, "D", asyncPFWait, "2") // dash's echo would expand it
	s.task("106", "woke", "D", asyncPFWait, "5")        // switches between the samples
	s.between("sed -i 's/^voluntary_ctxt_switches:.*/voluntary_ctxt_switches:\t6/' " + filepath.Join(s.proc, "106", "status"))
	s.task("108", "gone", "D", asyncPFWait, "1")
	os.Remove(filepath.Join(s.proc, "108", "status")) // as when the task exits mid-scan
	p, out := s.run()
	if strings.Contains(out, "\nroot=7") || strings.Contains(out, "cannot open") || strings.Contains(out, "No such file") {
		t.Errorf("forged or noisy output:\n%s", out)
	}
	got := map[int]string{}
	for _, st := range p.stuck {
		got[st.pid] = st.comm
	}
	if len(got) != 5 || got[101] != "claude" || got[102] != "qemu-system-x86" || got[105] != "?" || got[107] != "?" {
		t.Errorf("stuck = %+v\n%s", p.stuck, out)
	}
	if p.root || p.hung != -1 || p.noKVMAPF {
		t.Errorf("probe = %+v\n%s", p, out)
	}
}

func TestAsyncPFProbeScriptHealthyGuestDoesNotSleep(t *testing.T) {
	s := newAsyncPFSandbox(t)
	s.write(filepath.Join(s.proc, "cmdline"), "ro efi_pstore.pstore_disable=1 no-kvmapf\n")
	s.task("101", "claude", "S", "do_epoll_wait", "40")
	marker := filepath.Join(t.TempDir(), "slept")
	s.between("touch " + marker)
	p, out := s.run()
	if len(p.stuck) != 0 || !p.noKVMAPF {
		t.Errorf("probe = %+v\n%s", p, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the probe must not wait when the first sample finds nothing")
	}
}

func TestReadAsyncPFProbe(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("sh -c", proc.Result{Stdout: "root=1\nstuck=101 claude\nhung=4\n"})
	p, err := readAsyncPFProbe(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !p.root || p.hung != 4 || len(p.stuck) != 1 || p.stuck[0] != (stuckPID{101, "claude"}) {
		t.Errorf("probe = %+v", p)
	}
	c := f.Calls[0]
	if len(c.Args) != 4 || !strings.Contains(c.Args[1], "sudo -n sh -c") || !strings.Contains(c.Args[3], asyncPFWait) {
		t.Errorf("call = %+v", c.Args)
	}

	f = proc.NewFakeRunner()
	f.Script("sh -c", proc.Result{Stdout: "garbage\n"})
	if _, err := readAsyncPFProbe(context.Background(), f); err == nil {
		t.Error("want an error for output without root=")
	}
}

func TestCheckGuestAsyncPF(t *testing.T) {
	many := asyncPFProbe{root: true, hung: 0}
	for i := 1; i <= 7; i++ {
		many.stuck = append(many.stuck, stuckPID{i, "sh"})
	}
	for _, c := range []struct {
		name    string
		backend string
		p       asyncPFProbe
		perr    error
		ok      bool
		detail  string
		wantFix string
	}{
		{"orbstack", config.BackendOrbstack, asyncPFProbe{}, nil, true, "not applicable", ""},
		{"no answer", config.BackendLima, asyncPFProbe{}, errors.New("the guest did not answer within 45s"), true, "not checked (the guest did not answer", ""},
		{"healthy", config.BackendLima, asyncPFProbe{root: true, hung: 0}, nil, true, "no guest task is waiting", ""},
		{"no-kvmapf", config.BackendLima, asyncPFProbe{root: true, noKVMAPF: true}, nil, true, "async page faults are off (no-kvmapf)", ""},
		{"no sudo", config.BackendLima, asyncPFProbe{hung: -1}, nil, true, "only the run user's tasks", ""},
		{"stuck", config.BackendLima, asyncPFProbe{root: true, hung: 3, stuck: []stuckPID{{101, "claude"}, {7, "sshd"}}}, nil, false,
			"2 guest task(s) in D state in kvm_async_pf_task_wait_schedule, unchanged for 5 s: claude (101), sshd (7); the guest kernel logged 3", "limactl stop -f"},
		{"many stuck", config.BackendLima, many, nil, false, "sh (5), and 2 more", "`lever up`"},
		{"recovered", config.BackendLima, asyncPFProbe{root: true, hung: 2}, nil, false, "no task is stuck now, but the guest kernel logged 2", "lever stop"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := checkGuestAsyncPF(c.backend, c.p, c.perr)
			if r.name != "guest async page faults" || !r.ok {
				t.Fatalf("a warning at worst: %+v", r)
			}
			if c.ok != (r.fix == "") {
				t.Errorf("clean = %v, fix %q", c.ok, r.fix)
			}
			if !strings.Contains(r.detail, c.detail) || !strings.Contains(r.fix, c.wantFix) {
				t.Errorf("got %+v, want detail %q fix %q", r, c.detail, c.wantFix)
			}
		})
	}
}
