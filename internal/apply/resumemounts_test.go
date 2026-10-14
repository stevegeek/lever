package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// The stale-mount check reads the host tree before the resume, but the
// runtime resolves each mount source again (following links) when it
// recreates the container. So apply reads back where every tree self-mount
// comes from right after a resume, and a mount from elsewhere suspends the
// manager and fails the bring-up.
func TestStartManagerVerifiesResumedTreeMounts(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	if err := os.MkdirAll(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	jp := JailPath(app.Tree, app.Tree, "")
	mounts := []jail.Mount{
		{Source: jp, Destination: "/workspace", RW: true},
		{Source: path.Join(jp, "assistant"), Destination: "/workspace/assistant", RW: true},
		{Source: path.Join(jp, "assistant/tools"), Destination: "/workspace/assistant/tools"},
		// A dropped entry the record still mounts: checked too.
		{Source: path.Join(jp, "kb"), Destination: "/workspace/kb"},
		// Not a tree self-mount: not lever's, not checked.
		{Source: "/run/lever/x", Destination: "/run/lever"},
	}
	wantPlan := []jail.LiveMount{
		{Target: "/workspace/assistant", Source: path.Join(jp, "assistant"), SelfMount: true},
		{Target: "/workspace/assistant/tools", Source: path.Join(jp, "assistant/tools"), ReadOnly: true, SelfMount: true},
		{Target: "/workspace/kb", Source: path.Join(jp, "kb"), ReadOnly: true, SelfMount: true},
	}
	type answer struct {
		problems []jail.LiveMountProblem
		err      error
	}
	cases := []struct {
		name        string
		phase       string
		answers     []answer // successive live-check answers (the last repeats)
		wantErr     string
		wantSuspend bool
		wantLog     string
	}{
		{"held", "suspended", []answer{{}}, "", false, ""},
		{"stale mount through a link", "suspended", []answer{{problems: []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}}},
			`"kb" is mounted from another place`, true, ""},
		{"entry through a link, forced resume", "error", []answer{{problems: []jail.LiveMountProblem{{Target: "/workspace/assistant/tools", Reason: jail.LiveMountElsewhere}}}},
			`"assistant/tools" is mounted from another place`, true, ""},
		// The runtime resolved the destination through the same link: the
		// mount point moved, so the planned place has no mount.
		{"mount point moved by the link", "suspended", []answer{{problems: []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountMissing}}}},
			`"kb" not a mount point`, true, ""},
		{"read-only mount writable", "suspended", []answer{{problems: []jail.LiveMountProblem{{Target: "/workspace/assistant/tools", Reason: jail.LiveMountWritable}}}},
			`"assistant/tools" mounted read-write`, true, ""},
		{"foreign namespace", "suspended", []answer{{err: jail.ErrForeignNamespace}}, "mount namespace made inside the container", true, ""},
		// Not running yet right after the resume: checked again once live.
		{"not running yet", "suspended", []answer{{err: jail.ErrContainerNotRunning}, {problems: []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}}},
			`"kb" is mounted from another place`, true, ""},
		{"never readable", "suspended", []answer{{err: errors.New("cannot read")}}, "", false, "could not check from the guest where manager"},
		// A replaced directory (inode only) is the existing warning, not a
		// suspension: the kernel's record still names the tree.
		{"replaced, not elsewhere", "suspended", []answer{{problems: []jail.LiveMountProblem{{Target: "/workspace/assistant/tools", Reason: jail.LiveMountReplaced}}}},
			"", false, "covers another directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs, refs []string
			var plans [][]jail.LiveMount
			i := 0
			// start-manager and its post-live probes must not overlap: the
			// check after the resume (which alone may suspend) waits for them.
			var mu sync.Mutex
			var inFlight atomic.Int32
			var overlapped atomic.Bool
			live := func(_ context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				if inFlight.Add(1) > 1 {
					overlapped.Store(true)
				}
				defer inFlight.Add(-1)
				time.Sleep(5 * time.Millisecond) // widen the window an overlap needs
				mu.Lock()
				defer mu.Unlock()
				refs = append(refs, ref)
				plans = append(plans, want)
				a := tc.answers[min(i, len(tc.answers)-1)]
				i++
				return a.problems, a.err
			}
			f := scionOKRunner()
			r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: tc.phase, initContainerStatus: "stopped"}
			err := runApply(app, Deps{
				Scion:                  scion.New(r, scion.Options{}),
				Log:                    func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
				CheckLiveMounts:        live,
			})
			if r.resumeCalls == 0 {
				t.Fatal("the case must resume the manager")
			}
			if overlapped.Load() {
				t.Fatal("the check after the resume ran alongside the post-live probes")
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if suspended := sawScionCall(f, "suspend hello"); suspended != tc.wantSuspend {
				t.Fatalf("suspend called = %v, want %v (calls %q)", suspended, tc.wantSuspend, joinedCalls(f))
			}
			if tc.wantLog != "" && !strings.Contains(strings.Join(logs, "\n"), tc.wantLog) {
				t.Fatalf("logs %q, want %q", logs, tc.wantLog)
			}
			if !reflect.DeepEqual(plans[0], wantPlan) {
				t.Fatalf("plan = %+v, want %+v", plans[0], wantPlan)
			}
			for _, ref := range refs {
				if ref != jail.ContainerName(path.Base(jp), app.Name) {
					t.Fatalf("checked %q", ref)
				}
			}
		})
	}
}

// A manager apply keeps running is not recreated, so it is not suspended:
// a foreign namespace there is the loud warning of warnManagerTreeMounts.
func TestStartManagerKeptForeignNamespaceWarns(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	jp := JailPath(app.Tree, app.Tree, "")
	mounts := []jail.Mount{
		{Source: jp, Destination: "/workspace", RW: true},
		{Source: path.Join(jp, "assistant"), Destination: "/workspace/assistant", RW: true},
		{Source: path.Join(jp, "assistant/tools"), Destination: "/workspace/assistant/tools"},
	}
	var logs []string
	f := scionOKRunner()
	r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "running", initContainerStatus: "Up 2 hours"}
	err := runApply(app, Deps{
		Scion:                  scion.New(r, scion.Options{}),
		AgentSession:           &fakeSessionProbe{tok: validToken(), alive: true},
		Log:                    func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
		CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return nil, jail.ErrForeignNamespace
		},
	})
	if err != nil || r.resumeCalls != 0 || sawScionCall(f, "suspend hello") {
		t.Fatalf("err=%v resume=%d calls=%q", err, r.resumeCalls, joinedCalls(f))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "treat the manager as compromised") {
		t.Fatalf("logs %q", logs)
	}
}

// ManagerResume: Settle checks again until the container runs and fails on
// what it then finds; a check that never runs is a warning.
func TestManagerResumeSettle(t *testing.T) {
	old := settleBudget
	settleBudget = RetryBudget{Attempts: 4, Interval: time.Millisecond}
	t.Cleanup(func() { settleBudget = old })
	app := &config.App{Name: "hello", Tree: t.TempDir()}
	if err := os.Mkdir(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	mounts := []jail.Mount{{Source: "/lever/kb", Destination: "/workspace/kb"}}
	for _, tc := range []struct {
		name    string
		answers []error
		wantErr string
		wantLog string
	}{
		{"running after two looks", []error{jail.ErrContainerNotRunning, jail.ErrContainerNotRunning, nil}, "is mounted from another place", ""},
		{"never running", []error{jail.ErrContainerNotRunning}, "", "could not check from the guest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			var logs []string
			f := scionOKRunner()
			d := Deps{
				Scion:                  scion.New(f, scion.Options{}),
				Log:                    func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
				CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
					err := tc.answers[min(i, len(tc.answers)-1)]
					i++
					if err != nil {
						return nil, err
					}
					return []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}, nil
				},
			}
			g, err := BeginManagerResume(context.Background(), d, app, "/lever", "conversation")
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Resumed(context.Background()); err != nil {
				t.Fatal(err)
			}
			err = g.Settle(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.HasPrefix(err.Error(), "conversation: ") || !sawScionCall(f, "suspend hello") {
					t.Fatalf("err = %v (calls %q), want %q and the manager suspended", err, joinedCalls(f), tc.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(strings.Join(logs, "\n"), tc.wantLog) {
				t.Fatalf("err = %v logs %q, want the warning %q", err, logs, tc.wantLog)
			}
		})
	}
}

// A resume that fails may still have started the container: its mounts are
// read back before the failure is reported, and a mismatch suspends it.
func TestStartManagerChecksMountsAfterFailedResume(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	jp := JailPath(app.Tree, app.Tree, "")
	mounts := []jail.Mount{
		{Source: jp, Destination: "/workspace", RW: true},
		{Source: path.Join(jp, "assistant"), Destination: "/workspace/assistant", RW: true},
		{Source: path.Join(jp, "assistant/tools"), Destination: "/workspace/assistant/tools"},
	}
	f := scionOKRunner()
	r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: errors.New("pre-start hook failed")}
	err := runApply(app, Deps{
		Scion:                  scion.New(r, scion.Options{}),
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
		CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return []jail.LiveMountProblem{{Target: "/workspace/assistant/tools", Reason: jail.LiveMountElsewhere}}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "is mounted from another place") || !sawScionCall(f, "suspend hello") {
		t.Fatalf("err = %v calls %q, want the mount mismatch reported and the manager suspended", err, joinedCalls(f))
	}
}

// An interrupt (a cancelled context) right after the resume does not skip
// the read-back or the suspend.
func TestManagerResumeCheckSurvivesCancel(t *testing.T) {
	app := &config.App{Name: "hello", Tree: t.TempDir()}
	if err := os.Mkdir(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	d := Deps{
		Scion: scion.New(f, scion.Options{}),
		InspectContainerMounts: func(ctx context.Context, _ string) ([]jail.Mount, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []jail.Mount{{Source: "/lever/kb", Destination: "/workspace/kb"}}, nil
		},
		CheckLiveMounts: func(ctx context.Context, _ string, _ []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}, nil
		},
	}
	g, err := BeginManagerResume(context.Background(), d, app, "/lever", "up")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Resumed(ctx); err == nil || !strings.Contains(err.Error(), "is mounted from another place") || !sawScionCall(f, "suspend hello") {
		t.Fatalf("err = %v calls %q, want the mismatch found and the manager suspended despite the cancel", err, joinedCalls(f))
	}
}
