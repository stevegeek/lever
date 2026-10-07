package jail

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// liveRunner answers the three guest reads ContainerLiveMounts makes.
type liveRunner struct {
	pids      []string // successive podman inspect answers
	mountinfo string
	stat      string
	statErr   bool
	calls     []string
}

func (l *liveRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (proc.Result, error) {
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
	case "stat":
		if l.statErr {
			return proc.Result{Code: 1, Stderr: "stat: cannot statx: Permission denied"}, errors.New("exit status 1")
		}
		return proc.Result{Stdout: l.stat}, nil
	}
	return proc.Result{Code: 1}, errors.New("unexpected")
}

func (l *liveRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return l.Run(ctx, env, name, args...)
}

func (l *liveRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
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
		{"stat short", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n"}, nil, errLiveMounts},
		{"stat garbage", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, stat: "60:10\n60:10\nx\n60:11\n"}, nil, errLiveMounts},
		{"stat fails", liveRunner{pids: []string{"4242"}, mountinfo: miRoot + miTree + miPin + miEntry, statErr: true}, nil, errLiveMounts},
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
		"head -c 65536 /proc/4242/mountinfo",
		"stat -c %d:%i -- /lever/a /proc/4242/root/workspace/a /lever/a/tools /proc/4242/root/workspace/a/tools",
		"podman inspect --type container --format {{.State.Pid}} lever--m",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "exec") {
			t.Fatal("nothing may run inside the container")
		}
	}
	if _, err := ContainerLiveMounts(context.Background(), r, "--all", liveWant); err == nil {
		t.Fatal("a flag-shaped ref must be refused")
	}
}

func TestParseMountinfoUnescapes(t *testing.T) {
	got := parseMountinfo("1 0 0:1 / /work\\040space ro shared:1 - x y ro\n2 0 0:1 / /bad\\04 rw - x y rw\ntruncated line without end")
	if !reflect.DeepEqual(got, map[string]mountOpts{"/work space": {ro: true}}) {
		t.Fatalf("parse = %v", got)
	}
}
