package brokerctl

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

func TestCheckManagerReadOnly(t *testing.T) {
	const jp = "/lever"
	want := []config.TreeMount{{Rel: "assistant", ReadOnly: false}, {Rel: "assistant/tools", ReadOnly: true}}
	mount := func(rel string, rw bool) jail.Mount {
		return jail.Mount{Source: path.Join(jp, rel), Destination: path.Join(scion.ContainerWorkspace, rel), RW: rw}
	}
	full := []jail.Mount{mount("assistant", true), mount("assistant/tools", false)}
	held := func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil }
	leak := "Error: <container output \x1b[31m>"
	cases := []struct {
		name   string
		mounts []jail.Mount
		err    error
		live   func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error)
		want   string // "" = passes
		class  string
	}{
		{"held", full, nil, held, "", ""},
		{"cannot read", nil, errors.New(leak), held, "cannot read manager", "mounts-unreadable"},
		{"no container", nil, jail.ErrNoContainer, held, "no container yet", "no-container"},
		{"entry missing", []jail.Mount{mount("assistant", true)}, nil, held, "not mounted read-only", ""},
		{"entry writable", []jail.Mount{mount("assistant", true), mount("assistant/tools", true)}, nil, held, "mounted read-write", ""},
		{"pin missing", []jail.Mount{mount("assistant/tools", false)}, nil, held, "not pinned", ""},
		{"replaced on host", full, nil, func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return []jail.LiveMountProblem{{Target: "/workspace/assistant/tools", Reason: jail.LiveMountReplaced}}, nil
		}, "assistant/tools covers another directory", ""},
		{"not running", full, nil, func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return nil, jail.ErrContainerNotRunning
		}, "is not running", "not-running"},
		{"live unreadable", full, nil, func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return nil, errors.New(leak)
		}, "cannot read manager \"m\"'s live mounts", "live-unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var plan []jail.LiveMount
			live := func(ctx context.Context, p []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				plan = p
				return tc.live(ctx, p)
			}
			err := checkManagerReadOnly(context.Background(), "m", jp, want,
				func(context.Context) ([]jail.Mount, error) { return tc.mounts, tc.err }, live)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("want pass, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
			if err != nil && strings.Contains(err.Error(), "container output") {
				t.Fatalf("container output reached the refusal: %v", err)
			}
			if tc.class != "" && guardClass(err) != tc.class {
				t.Fatalf("class = %q, want %q", guardClass(err), tc.class)
			}
			if tc.name == "held" {
				wantPlan := []jail.LiveMount{
					{Target: "/workspace/assistant", Source: "/lever/assistant"},
					{Target: "/workspace/assistant/tools", Source: "/lever/assistant/tools", ReadOnly: true},
				}
				if len(plan) != 2 || plan[0] != wantPlan[0] || plan[1] != wantPlan[1] {
					t.Fatalf("live plan = %v, want %v", plan, wantPlan)
				}
			}
		})
	}
}

// A guard whose text changes on every try (as container output would) is
// logged once per reason class, sanitised and bounded.
func TestSupervisorLogsGuardRefusalOncePerClass(t *testing.T) {
	logs := filepath.Join(t.TempDir(), "tool-logs")
	s := NewSupervisor([]ToolSpec{{Name: "ro", Command: []string{"/bin/sleep", "60"}, ReadOnly: []string{"tools"}}}, "http://127.0.0.1:0", logs, testToolSecret)
	s.retry = 5 * time.Millisecond
	var n atomic.Int32
	s.ReadOnlyGuard = func(context.Context, []string) error {
		return &guardError{"live-unreadable", strings.Repeat("x", 2000) + "\x1b[2J" + strings.Repeat("y", int(n.Add(1)))}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return n.Load() > 5 })
	s.Stop()
	b, _ := os.ReadFile(filepath.Join(logs, "ro.log"))
	if c := strings.Count(string(b), "not starting tool"); c != 1 {
		t.Fatalf("logged %d times, want once for one class", c)
	}
	if strings.Contains(string(b), "\x1b") {
		t.Fatal("an escape sequence reached the log")
	}
	if len(b) > 2000 {
		t.Fatalf("log line is %d bytes, want it bounded", len(b))
	}
	if !strings.Contains(string(b), "check the program before you trust it again") {
		t.Fatalf("the fix text lacks the check: %q", b)
	}
}
