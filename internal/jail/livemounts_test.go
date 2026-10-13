package jail

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// liveRunner answers the guest reads ContainerLiveMounts makes. Each must
// run under the guest's timeout(1); calls records them without it.
type liveRunner struct {
	pids      []string // successive podman inspect answers
	mountinfo string
	stat      string
	statErr   bool
	ns        []string // successive namespace answers (the last repeats); nil = nsHeld
	nsErr     bool
	calls     []string
	stdin     []string // what each stdin call carried
	unbounded bool     // a call ran without the guest timeout
}

var statRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// nsHeld is nsReader's answer for a first process in its runtime's
// namespaces: the pid and mnt namespaces owned by its user namespace.
const nsHeld = "4026532463 user 0\n4026532464 mnt 4026532463\n4026532536 pid 4026532463\n"

// nsNested is the answer after the first process moved into a user and
// mount namespace made inside the container: the pid namespace (which it
// cannot leave) is still owned by the container's user namespace.
const nsNested = "4026532900 user 0\n4026532901 mnt 4026532900\n4026532536 pid 4026532463\n"

func (l *liveRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (proc.Result, error) {
	if name != "timeout" || len(args) < 3 || !strings.HasPrefix(args[0], "--kill-after=") || !strings.HasSuffix(args[1], "s") {
		l.unbounded = true
	} else {
		name, args = args[2], args[3:]
	}
	l.calls = append(l.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "podman":
		pid := l.pids[0]
		if len(l.pids) > 1 {
			l.pids = l.pids[1:]
		}
		return proc.Result{Stdout: pid + "\n"}, nil
	case "head":
		return proc.Result{Stdout: l.mountinfo}, nil
	case "perl":
		if len(args) != 3 || args[0] != "-T" || args[1] != "-" || l.stdin[len(l.stdin)-1] != nsReader {
			return proc.Result{Code: 1}, errors.New("unexpected perl call")
		}
		if l.nsErr {
			// The reader exits non-zero (on Linux 7.0 the namespace link
			// of a process the run user may not ptrace opens as a
			// directory): whatever it printed must not count.
			return proc.Result{Code: 25, Stdout: "4026532463 user 0\n4026532464 mnt 4026532463\n4026532536 pid 4026532463\n", Stderr: "type pid"}, errors.New("exit status 25")
		}
		ns := nsHeld
		if len(l.ns) > 0 {
			ns = l.ns[0]
			if len(l.ns) > 1 {
				l.ns = l.ns[1:]
			}
		}
		return proc.Result{Stdout: ns}, nil
	case "xargs":
		if l.statErr {
			return proc.Result{Code: 1, Stderr: "xargs: stat: Permission denied"}, errors.New("exit status 1")
		}
		// l.stat lists one answer per path in order: "dev:ino", "-" for a
		// path stat could not answer (xargs then exits 123), or raw text.
		paths := strings.Split(strings.TrimSuffix(l.stdin[len(l.stdin)-1], "\x00"), "\x00")
		var out strings.Builder
		missing := false
		for k, line := range strings.Split(strings.TrimSuffix(l.stat, "\n"), "\n") {
			switch {
			case line == "-" || k >= len(paths):
				missing = true
			case statRE.MatchString(line):
				out.WriteString(line + ":" + paths[k] + "\n")
			default:
				out.WriteString(line + "\n")
			}
		}
		if missing || strings.Count(l.stat, "\n") < len(paths) {
			return proc.Result{Code: 123, Stdout: out.String(), Stderr: "stat: cannot statx: No such file or directory"}, errors.New("exit status 123")
		}
		return proc.Result{Stdout: out.String()}, nil
	}
	return proc.Result{Code: 1}, errors.New("unexpected")
}

func (l *liveRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return l.Run(ctx, env, name, args...)
}

func (l *liveRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return proc.Result{}, err
	}
	l.stdin = append(l.stdin, string(b))
	return l.Run(ctx, env, name, args...)
}

// mountinfo lines as the kernel writes them, for a manager with the tree,
// a pin and a read-only entry.
const (
	miRoot  = "500 400 0:50 / / rw,relatime - overlay overlay rw\n"
	miTree  = "510 500 0:60 /lever /workspace rw,relatime - virtiofs mac rw\n"
	miPin   = "511 510 0:60 /lever/a /workspace/a rw,relatime - virtiofs mac rw\n"
	miEntry = "512 511 0:60 /lever/a/tools /workspace/a/tools ro,relatime - virtiofs mac rw\n"
)

var liveWant = []LiveMount{
	{Target: "/workspace/a", Source: "/lever/a"},
	{Target: "/workspace/a/tools", Source: "/lever/a/tools", ReadOnly: true},
}

func TestContainerLiveMounts(t *testing.T) {
	same := "60:10\n60:10\n60:11\n60:11\n"
	cases := []struct {
		name string
		r    liveRunner
		want []LiveMountProblem
		err  error // nil = no error expected; errLiveMounts = any error
	}{
		{"held", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same}, nil, nil},
		{"entry missing", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin, stat: same},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountMissing}}, nil},
		{"entry read-write", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + strings.Replace(miEntry, "ro,relatime", "rw,relatime", 1), stat: same},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountWritable}}, nil},
		// rm -rf and recreate on the host: the mount stays on the old
		// directory, whose point the kernel marks deleted.
		{"replaced, deleted", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin +
			"512 511 0:60 /lever/a/tools//deleted /workspace/a/tools\\040(deleted) ro,relatime - virtiofs mac rw\n", stat: "60:10\n60:10\n60:99\n60:11\n"},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountMissing}}, nil},
		// The point still listed read-only, but another inode behind it.
		{"replaced, other inode", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n60:10\n60:99\n60:11\n"},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountReplaced}}, nil},
		// A later mount over the same point is the one that counts.
		{"mounted over read-write", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry +
			"513 512 0:70 / /workspace/a/tools rw - tmpfs tmpfs rw\n", stat: same},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountWritable}}, nil},
		{"not running", liveRunner{pids: []string{"0"}, mountinfo: miRoot, stat: same}, nil, ErrContainerNotRunning},
		{"pid garbage", liveRunner{pids: []string{"<no value>"}, mountinfo: miRoot, stat: same}, nil, errLiveMounts},
		{"pid changed", liveRunner{pids: []string{"4242", "4343"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same}, nil, errLiveMounts},
		{"mountinfo too long", liveRunner{pids: []string{"4242"}, mountinfo: strings.Repeat(miRoot, liveMountsMaxOutput/len(miRoot)+1), stat: same}, nil, errLiveMounts},
		{"mountinfo empty", liveRunner{pids: []string{"4242"}, mountinfo: "", stat: same}, nil, errLiveMounts},
		// stat could not answer for some paths: those mounts are not held,
		// and the check still runs (deleting a source must not turn it
		// into "could not read").
		{"stat short", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n"},
			[]LiveMountProblem{{"/workspace/a", LiveMountReplaced}, {"/workspace/a/tools", LiveMountReplaced}}, nil},
		{"source gone", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n60:10\n-\n60:11\n"},
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountReplaced}}, nil},
		{"stat garbage", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n60:10\nx\n60:11\n"}, nil, errLiveMounts},
		{"stat fails", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, statErr: true}, nil, errLiveMounts},
		// The first process moved into namespaces made inside the
		// container: its view says nothing, whatever it shows.
		{"nested namespace", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same, ns: []string{nsNested}}, nil, ErrForeignNamespace},
		// Moved during the reads: the check after them catches it.
		{"moved during the reads", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same, ns: []string{nsHeld, nsNested}}, nil, ErrForeignNamespace},
		// Only the mount namespace moved (owned by a nested user namespace).
		{"mount namespace owned elsewhere", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{strings.Replace(nsHeld, "4026532464 mnt 4026532463", "4026532901 mnt 4026532900", 1)}}, nil, ErrForeignNamespace},
		// Only the user namespace moved.
		{"user namespace moved", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{strings.Replace(nsHeld, "4026532463 user", "4026532900 user", 1)}}, nil, ErrForeignNamespace},
		{"owner not shown", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{"4026532463 user 0\n4026532464 mnt 0\n4026532536 pid 0\n"}}, nil, ErrForeignNamespace},
		{"namespace read fails", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same, nsErr: true}, nil, errLiveMounts},
		{"namespaces empty", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same, ns: []string{""}}, nil, errLiveMounts},
		{"no mnt line", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{"4026532463 user 0\n4026532536 pid 4026532463\n"}}, nil, errLiveMounts},
		{"namespaces garbage", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{nsHeld + "x mnt 1\n"}}, nil, errLiveMounts},
		{"other type without owner", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{"4026531834 time\n" + nsHeld}}, nil, nil},
		{"duplicate type", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: same,
			ns: []string{nsHeld + "4026532999 mnt 4026532463\n"}}, nil, errLiveMounts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r
			got, err := ContainerLiveMounts(context.Background(), &r, "lever--m", liveWant)
			switch {
			case tc.err == nil && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.err != nil && !errors.Is(err, tc.err):
				t.Fatalf("error = %v, want %v", err, tc.err)
			case !reflect.DeepEqual(got, tc.want):
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
			if tc.err == errLiveMounts && strings.Contains(err.Error(), "Permission") {
				t.Fatal("guest output leaked into the error")
			}
		})
	}
}

func TestContainerLiveMountsReadsFromTheGuest(t *testing.T) {
	r := &liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n60:10\n60:11\n60:11\n"}
	if _, err := ContainerLiveMounts(context.Background(), r, "lever--m", liveWant); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"podman inspect --type container --format {{.State.Pid}} lever--m",
		"perl -T - 4242",
		"head -c 65536 /proc/4242/mountinfo",
		"xargs -0 stat -c %d:%i:%n --",
		"perl -T - 4242",
		"podman inspect --type container --format {{.State.Pid}} lever--m",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
	if r.unbounded {
		t.Fatal("every guest read must run under the guest's timeout")
	}
	if wantIn := []string{nsReader, "/lever/a\x00/proc/4242/root/workspace/a\x00/lever/a/tools\x00/proc/4242/root/workspace/a/tools\x00", nsReader}; !reflect.DeepEqual(r.stdin, wantIn) {
		t.Fatalf("stdin = %q, want %q", r.stdin, wantIn)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "exec") {
			t.Fatal("nothing may run inside the container")
		}
	}
	if _, err := ContainerLiveMounts(context.Background(), r, "--all", liveWant); err == nil {
		t.Fatal("a flag-shaped ref must be refused")
	}
	if _, err := ContainerLiveMounts(context.Background(), r, "lever--m", nil); err == nil {
		t.Fatal("an empty plan must be refused, never held")
	}
}

// A path with a space (or any byte but NUL) reaches stat whole on stdin:
// no word of the guest command line carries it, so a transport that
// re-splits the argv (a remote shell) cannot cut it in two.
func TestContainerLiveMountsPathWithSpace(t *testing.T) {
	want := []LiveMount{{Target: "/workspace/my tools", Source: "/lever/my tools", ReadOnly: true}}
	mi := miRoot + miTree + "512 510 0:60 /lever/my\\040tools /workspace/my\\040tools ro,relatime - virtiofs mac rw\n"
	r := &liveRunner{pids: []string{"4242"}, mountinfo: mi, stat: "60:11\n60:11\n"}
	got, err := ContainerLiveMounts(context.Background(), r, "lever--m", want)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want held", got, err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "my tools") {
			t.Fatalf("a path rode the command line: %q", c)
		}
	}
	if r.stdin[1] != "/lever/my tools\x00/proc/4242/root/workspace/my tools\x00" {
		t.Fatalf("stat stdin = %q", r.stdin[1])
	}
}

func TestParseMountinfoUnescapes(t *testing.T) {
	got := parseMountinfo("1 0 0:1 / /work\\040space ro shared:1 - x y ro\n2 0 0:1 / /bad\\04 rw - x y rw\ntruncated line without end")
	if !reflect.DeepEqual(got, map[string]mountOpts{"/work space": {ro: true, dev: "0:1", root: "/"}}) {
		t.Fatalf("parse = %v", got)
	}
}

// A tree directory mounted over its own place must come, by the kernel's
// record, from the tree at that place: the parent mount's device, and the
// parent's root joined with the path below it. No inode numbers needed.
func TestContainerLiveMountsSelfMount(t *testing.T) {
	want := []LiveMount{
		{Target: "/workspace/a", Source: "/lever/a", SelfMount: true},
		{Target: "/workspace/a/tools", Source: "/lever/a/tools", ReadOnly: true, SelfMount: true},
	}
	same := "60:10\n60:10\n60:11\n60:11\n"
	cases := []struct {
		name      string
		mountinfo string
		want      []LiveMountProblem
	}{
		{"held", miRoot + miTree + miPin + miEntry, nil},
		// The runtime followed a link to a guest path when it mounted the
		// entry: another device and root.
		{"link to the guest", miRoot + miTree + miPin + "512 511 252:1 /home/u/.scion /workspace/a/tools ro,relatime - ext4 /dev/vda1 rw\n",
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountElsewhere}}},
		// A link to another tree directory: same device, another root.
		{"link inside the tree", miRoot + miTree + miPin + "512 511 0:60 /lever/b /workspace/a/tools ro,relatime - virtiofs mac rw\n",
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountElsewhere}}},
		// The pin itself came through a link: caught at the pin; the
		// entry under it still matches its parent.
		{"pin through a link", miRoot + miTree + "511 510 0:60 /lever/x /workspace/a rw,relatime - virtiofs mac rw\n" +
			"512 511 0:60 /lever/x/tools /workspace/a/tools ro,relatime - virtiofs mac rw\n",
			[]LiveMountProblem{{"/workspace/a", LiveMountElsewhere}}},
		// Renamed away on the host after the start: the kernel follows the
		// rename.
		{"renamed since", miRoot + miTree + miPin + "512 511 0:60 /lever/a/tools.old /workspace/a/tools ro,relatime - virtiofs mac rw\n",
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountElsewhere}}},
		// No tree mount above the pin: nothing to compare with.
		{"no parent mount", miRoot + miPin + miEntry,
			[]LiveMountProblem{{"/workspace/a", LiveMountElsewhere}}},
		{"unreadable root", miRoot + miTree + miPin + "512 511 0:60 /lever/a/tools\\9 /workspace/a/tools ro,relatime - virtiofs mac rw\n",
			[]LiveMountProblem{{"/workspace/a/tools", LiveMountElsewhere}}},
		// A space in the path, escaped by the kernel in both fields.
		{"space", miRoot + miTree + "511 510 0:60 /lever/my\\040a /workspace/my\\040a rw - virtiofs mac rw\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := want
			if tc.name == "space" {
				w = []LiveMount{{Target: "/workspace/my a", Source: "/lever/my a", SelfMount: true}}
			}
			stat := same
			if len(w) == 1 {
				stat = "60:10\n60:10\n"
			}
			r := &liveRunner{pids: []string{"4242"}, mountinfo: tc.mountinfo, stat: stat}
			got, err := ContainerLiveMounts(context.Background(), r, "lever--m", w)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// parseNamespaces takes nsReader's lines and lsns's, as seen in Lima
// guests: nsReader on Ubuntu 26.04, lsns as the run user on 24.04
// (util-linux 2.39, time owned by the initial user namespace) and as root
// on 26.04 (util-linux 2.41).
func TestParseNamespacesGuestOutputs(t *testing.T) {
	want := map[string]nsEntry{
		"user": {4026532398, 0},
		"mnt":  {4026532399, 4026532398},
		"pid":  {4026532402, 4026532398},
	}
	reader := "4026532398 user 0\n4026532399 mnt 4026532398\n4026532402 pid 4026532398\n"
	lsns := "4026531834 time 4026531837\n4026532398 user 4026532282\n4026532399 mnt 4026532398\n4026532400 uts 4026532398\n" +
		"4026532401 ipc 4026532398\n4026532402 pid 4026532398\n4026532403 net 4026532398\n4026532462 cgroup 4026532398\n"
	for name, out := range map[string]string{"nsReader": reader, "lsns": lsns} {
		got, err := parseNamespaces(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "lsns" {
			got["user"] = nsEntry{got["user"].id, 0}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v, want %v", name, got, want)
		}
	}
}

// nsReader against this test's own process, where perl is installed: the
// ids are the kernel's, the owners are user namespaces, and a pid it
// cannot read is an error.
func TestNsReaderRunsOnThisKernel(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("no perl")
	}
	run := func(pid string) (string, error) {
		cmd := exec.Command("perl", "-T", "-", pid)
		cmd.Stdin = strings.NewReader(nsReader)
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run(strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Fatalf("nsReader: %v", err)
	}
	ns, err := parseNamespaces(out)
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	for _, typ := range []string{"user", "mnt", "pid"} {
		fi, err := os.Stat("/proc/self/ns/" + typ)
		if err != nil {
			t.Fatal(err)
		}
		if ino := fi.Sys().(*syscall.Stat_t).Ino; ns[typ].id != ino {
			t.Fatalf("%s: id %d, kernel %d", typ, ns[typ].id, ino)
		}
	}
	if ns["mnt"].owner == 0 || ns["pid"].owner == 0 || ns["user"].owner != 0 {
		t.Fatalf("owners: %v", ns)
	}
	for _, pid := range []string{"0", "-1", "1x", "4294967296123"} {
		if out, err := run(pid); err == nil {
			t.Fatalf("pid %q: %q, want an error", pid, out)
		}
	}
}
