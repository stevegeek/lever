package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

func TestManagerTreeVolumes(t *testing.T) {
	if got := managerTreeVolumes("/lever", nil); got != nil {
		t.Fatalf("no plan must give no volumes (so no inline config), got %v", got)
	}
	got := managerTreeVolumes("/lever", []config.TreeMount{
		{Rel: "assistant", ReadOnly: false},
		{Rel: "assistant/tools", ReadOnly: true},
	})
	want := []scion.VolumeMount{
		{Source: "/lever/assistant", Target: "/workspace/assistant", ReadOnly: false},
		{Source: "/lever/assistant/tools", Target: "/workspace/assistant/tools", ReadOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volumes =\n%v\nwant\n%v", got, want)
	}
}

// readOnlyConfig writes an instance whose manager protects assistant/tools,
// creating that directory under the tree unless the caller wants to shape
// the tree itself.
func readOnlyConfig(t *testing.T, makeDir bool) (dir string, app *config.App) {
	t.Helper()
	dir = t.TempDir()
	if makeDir {
		if err := os.MkdirAll(filepath.Join(dir, "workspace", "assistant", "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	body := "name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  read_only:\n    - assistant/tools\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dir, app
}

// manager.read_only reaches the CREATE as inline-config volumes on stdin:
// the entry read-only and its ancestor pinned read-write, both over their
// own place in the workspace.
func TestStartManagerCarriesReadOnlyVolumes(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	f := scionOKRunner()
	if err := runApply(app, Deps{Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{})}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var start *proc.Call
	for i := range f.Calls {
		if strings.Contains(strings.Join(f.Calls[i].Args, " "), "-- hello") {
			start = &f.Calls[i]
		}
	}
	if start == nil {
		t.Fatalf("no manager start call; calls=%+v", f.Calls)
	}
	if !slices.Contains(start.Args, "--config") {
		t.Fatalf("start argv %q must send inline config", start.Args)
	}
	var body struct {
		Volumes []scion.VolumeMount `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(start.Stdin), &body); err != nil {
		t.Fatalf("stdin %q: %v", start.Stdin, err)
	}
	jp := JailPath(app.Tree, app.Tree, "")
	want := []scion.VolumeMount{
		{Source: filepath.ToSlash(filepath.Join(jp, "assistant")), Target: "/workspace/assistant"},
		{Source: filepath.ToSlash(filepath.Join(jp, "assistant", "tools")), Target: "/workspace/assistant/tools", ReadOnly: true},
	}
	if !reflect.DeepEqual(body.Volumes, want) {
		t.Fatalf("volumes =\n%v\nwant\n%v", body.Volumes, want)
	}
}

// A read_only path reached through a symlink fails the apply naming the
// link, with no start attempted (and so, under --fresh, no delete either:
// the check runs before the record is touched).
func TestStartManagerRefusesSymlinkedReadOnlyPath(t *testing.T) {
	dir, app := readOnlyConfig(t, false)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "workspace", "assistant")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	err := runApply(app, Deps{Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{})})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), "assistant/tools") {
		t.Fatalf("want a symlink refusal naming the entry, got %v", err)
	}
	for _, c := range f.Calls {
		if j := strings.Join(c.Args, " "); strings.Contains(j, "-- hello") || slices.Contains(c.Args, "delete") {
			t.Fatalf("no start or delete may be attempted; got %q", j)
		}
	}
}

// swapOnDeleteRunner runs onDelete just before the manager delete reaches
// the fake: the moment the old (unprotected) manager could still change the
// tree, after start-manager's first read_only check.
type swapOnDeleteRunner struct {
	*agentLifecycleRunner
	onDelete func()
}

func (r *swapOnDeleteRunner) hook(name string, args []string) {
	if name == "scion" && r.verb(args) == "delete" && r.onDelete != nil {
		r.onDelete()
		r.onDelete = nil
	}
}

func (r *swapOnDeleteRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	r.hook(name, args)
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *swapOnDeleteRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *swapOnDeleteRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	r.hook(name, args)
	return r.agentLifecycleRunner.RunStdin(ctx, stdin, env, name, args...)
}

// Under --fresh a symlinked read_only path is refused BEFORE the delete:
// refusing only at the create would discard the old session and start
// nothing. (The record exists, so the "no delete" assertion has teeth.)
func TestStartManagerFreshRefusesSymlinkedReadOnlyPathBeforeDelete(t *testing.T) {
	dir, app := readOnlyConfig(t, false)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "workspace", "assistant")); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "suspended", initContainerStatus: "stopped"}
	err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("want a symlink refusal, got %v", err)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 {
		t.Fatalf("delete=%d start=%d resume=%d, want 0/0/0: the session must be left alone", r.deleteCalls, r.startCalls, r.resumeCalls)
	}
}

// The second check: the old manager is still running during the first one
// and can swap the tree until the --fresh delete removes it. A swap made in
// that window is refused right before the create.
func TestStartManagerFreshRechecksReadOnlyBeforeCreate(t *testing.T) {
	dir, app := readOnlyConfig(t, true)
	f := scionOKRunner()
	inner := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "running", initContainerStatus: "running"}
	tools := filepath.Join(dir, "workspace", "assistant", "tools")
	r := &swapOnDeleteRunner{agentLifecycleRunner: inner, onDelete: func() {
		if err := os.Remove(tools); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), tools); err != nil {
			t.Fatal(err)
		}
	}}
	err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("want the pre-create check to refuse the swapped path, got %v", err)
	}
	if inner.deleteCalls != 1 {
		t.Fatalf("deleteCalls = %d, want 1 (the swap happens at the delete)", inner.deleteCalls)
	}
	if inner.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0: no manager may be created over a swapped path", inner.startCalls)
	}
}

// A resumed or kept manager may predate read_only, or a protected dir may
// have been replaced on the host since its create: apply warns (never
// fails) and says how to fix it.
func TestStartManagerWarnsWhenKeptManagerLacksReadOnlyMounts(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	jp := JailPath(app.Tree, app.Tree, "")
	held := []jail.Mount{
		{Source: jp, Destination: "/workspace", RW: true},
		{Source: path.Join(jp, "assistant"), Destination: "/workspace/assistant", RW: true},
		{Source: path.Join(jp, "assistant/tools"), Destination: "/workspace/assistant/tools"},
	}
	refused := func(context.Context, string, string) (bool, error) { return false, nil }
	for _, tc := range []struct {
		name     string
		inspect  func(context.Context, string) ([]jail.Mount, error)
		probe    func(context.Context, string, string) (bool, error)
		wantWarn string
	}{
		{"lacks the mounts", func(context.Context, string) ([]jail.Mount, error) {
			return []jail.Mount{{Source: jp, Destination: "/workspace", RW: true}}, nil
		}, refused, "not mounted read-only"},
		{"holds them", func(context.Context, string) ([]jail.Mount, error) { return held, nil }, refused, ""},
		{"replaced on the host", func(context.Context, string) ([]jail.Mount, error) { return held, nil },
			func(_ context.Context, _ string, target string) (bool, error) {
				return target == "/workspace/assistant/tools", nil
			},
			"edit protected directories in place"},
		{"probe cannot run", func(context.Context, string) ([]jail.Mount, error) { return held, nil },
			func(context.Context, string, string) (bool, error) { return false, errors.New("exec failed") }, "could not probe"},
		{"cannot inspect", func(context.Context, string) ([]jail.Mount, error) { return nil, errors.New("podman exploded") }, refused, "lever doctor"},
		{"no probe wired", nil, nil, "lever doctor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			f := scionOKRunner()
			r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "suspended", initContainerStatus: "stopped"}
			var refs []string
			deps := Deps{
				Scion: scion.New(r, scion.Options{}),
				Log:   func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			}
			if tc.inspect != nil {
				deps.InspectContainerMounts = func(ctx context.Context, ref string) ([]jail.Mount, error) {
					refs = append(refs, ref)
					return tc.inspect(ctx, ref)
				}
				deps.ProbeContainerWritable = tc.probe
			}
			if err := runApply(app, deps); err != nil {
				t.Fatalf("a missing mount must not fail apply: %v", err)
			}
			joined := strings.Join(logs, "\n")
			if tc.wantWarn == "" {
				if strings.Contains(joined, "WARNING") {
					t.Fatalf("no warning expected, got %q", joined)
				}
			} else if !strings.Contains(joined, "WARNING") || !strings.Contains(joined, tc.wantWarn) {
				t.Fatalf("want a warning mentioning %q, got %q", tc.wantWarn, joined)
			}
			// Twice on a resume: the stale-mount check before it, and the
			// protection check after.
			if tc.inspect != nil && (len(refs) != 2 || refs[0] != jail.ContainerName(path.Base(jp), "hello") || refs[1] != refs[0]) {
				t.Fatalf("inspected %q, want the manager container by name, before and after the resume", refs)
			}
		})
	}
	// A manager this run CREATED is not probed: the create carried the mounts.
	f := scionOKRunner()
	probed := false
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) {
			probed = true
			return nil, nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probed {
		t.Fatal("a freshly created manager must not be probed")
	}
}

func TestManagerTreeMountGaps(t *testing.T) {
	want := []config.TreeMount{{Rel: "a"}, {Rel: "w", WorkerPin: true}, {Rel: "a/tools", ReadOnly: true}}
	ok := []jail.Mount{
		{Source: "/lever/a", Destination: "/workspace/a", RW: true},
		{Source: "/lever/w", Destination: "/workspace/w", RW: true},
		{Source: "/lever/a/tools", Destination: "/workspace/a/tools"},
	}
	if g := ManagerTreeMountGaps("/lever", want, ok); !g.Empty() {
		t.Fatalf("a matching container must have no gaps, got %+v", g)
	}
	g := ManagerTreeMountGaps("/lever", want, []jail.Mount{{Source: "/other/a/tools", Destination: "/workspace/a/tools"}})
	if !reflect.DeepEqual(g.MissingPins, []string{"a"}) || !reflect.DeepEqual(g.MissingWorkerPins, []string{"w"}) || !reflect.DeepEqual(g.WrongSource, []string{"a/tools"}) {
		t.Fatalf("gaps = %+v", g)
	}
	if s := g.String(); !strings.Contains(s, "w (a worker dir) not pinned: the manager can replace it with a symbolic link") || strings.Contains(s, "w (a worker dir) not pinned: the agent can write") {
		t.Fatalf("a missing worker pin must be worded as a redirect, got %q", s)
	}
	g = ManagerTreeMountGaps("/lever", want, nil)
	if !reflect.DeepEqual(g.Missing, []string{"a/tools"}) {
		t.Fatalf("gaps = %+v", g)
	}
	g = ManagerTreeMountGaps("/lever", want, []jail.Mount{ok[0], ok[1], {Source: "/lever/a/tools", Destination: "/workspace/a/tools", RW: true}})
	if !reflect.DeepEqual(g.Writable, []string{"a/tools"}) || !strings.Contains(g.String(), "a/tools mounted read-write") {
		t.Fatalf("gaps = %+v (%s)", g, g)
	}
	// The live probe: only entries are probed, at their container path; a
	// probe error is an error, never "protected".
	var probed []string
	replaced, err := ProbeReplacedEntries(context.Background(), func(_ context.Context, _ string, target string) (bool, error) {
		probed = append(probed, target)
		return true, nil
	}, "c", want)
	if err != nil || !reflect.DeepEqual(replaced, []string{"a/tools"}) || !reflect.DeepEqual(probed, []string{"/workspace/a/tools"}) {
		t.Fatalf("replaced=%v probed=%v err=%v", replaced, probed, err)
	}
	if _, err := ProbeReplacedEntries(context.Background(), func(context.Context, string, string) (bool, error) {
		return false, errors.New("exec failed")
	}, "c", want); err == nil {
		t.Fatal("a probe that cannot run must be an error")
	}
}

// card #157: a manager record keeps the mounts of its create. A directory it
// mounts that is gone on the host refuses the resume up front, naming the
// directory and both fixes; an entry the config dropped is a warning; the
// mounts of the current plan, the workspace and scion's own say nothing.
func TestStartManagerStaleTreeMounts(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	jp := JailPath(app.Tree, app.Tree, "")
	if err := os.MkdirAll(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := []jail.Mount{
		{Source: jp, Destination: "/workspace", RW: true},
		{Source: "/home/u/.scion/x", Destination: "/workspace/.scion-volumes/x", RW: true},
		{Source: path.Join(jp, "assistant"), Destination: "/workspace/assistant", RW: true},
		{Source: path.Join(jp, "assistant/tools"), Destination: "/workspace/assistant/tools"},
	}
	refused := func(context.Context, string, string) (bool, error) { return false, nil }
	for _, tc := range []struct {
		name             string
		extra            []jail.Mount
		phase, cstatus   string
		wantErr, wantLog string
	}{
		{"current plan", nil, "suspended", "stopped", "", ""},
		{"dropped entry, dir kept", []jail.Mount{{Source: path.Join(jp, "kb"), Destination: "/workspace/kb"}}, "suspended", "stopped",
			"", "kb still mounted read-only although no longer in manager.read_only"},
		{"dropped entry, dir gone, stopped", []jail.Mount{{Source: path.Join(jp, "gone"), Destination: "/workspace/gone"}}, "suspended", "stopped",
			"gone mounted by the manager record but no longer on the host", ""},
		{"dropped pin, dir gone, running", []jail.Mount{{Source: path.Join(jp, "old"), Destination: "/workspace/old", RW: true}}, "running", "Up 2 hours",
			"", "old mounted by the manager record but no longer on the host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			f := scionOKRunner()
			r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: tc.phase, initContainerStatus: tc.cstatus}
			mounts := append(append([]jail.Mount{}, base...), tc.extra...)
			deps := Deps{
				Scion:                  scion.New(r, scion.Options{}),
				Log:                    func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
				ProbeContainerWritable: refused,
			}
			err := runApply(app, deps)
			joined := strings.Join(logs, "\n")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "recreate each missing directory") || !strings.Contains(err.Error(), "lever up --fresh") {
					t.Fatalf("err = %v, want a refusal naming %q and both fixes", err, tc.wantErr)
				}
				if f.Called(proc.ArgvPrefix("scion", "resume")) {
					t.Fatal("the resume ran although its container cannot be recreated")
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantLog == "" {
				if strings.Contains(joined, "WARNING") {
					t.Fatalf("no warning expected, got %q", joined)
				}
				return
			}
			if !strings.Contains(joined, "WARNING") || !strings.Contains(joined, tc.wantLog) || !strings.Contains(joined, "lever up --fresh") {
				t.Fatalf("want a warning mentioning %q and the fix, got %q", tc.wantLog, joined)
			}
		})
	}
}

func TestManagerStaleTreeMounts(t *testing.T) {
	tree := t.TempDir()
	for _, d := range []string{"a/tools", "kb", "w"} {
		if err := os.MkdirAll(filepath.Join(tree, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := []config.TreeMount{{Rel: "a"}, {Rel: "a/tools", ReadOnly: true}}
	got := []jail.Mount{
		{Source: "/lever", Destination: "/workspace", RW: true},
		{Source: "/lever/a", Destination: "/workspace/a", RW: true},
		{Source: "/lever/a/tools", Destination: "/workspace/a/tools"},
		{Source: "/lever/kb", Destination: "/workspace/kb"},
		{Source: "/lever/w", Destination: "/workspace/w", RW: true},
		{Source: "/lever/gone", Destination: "/workspace/gone"},
		{Source: "/elsewhere/x", Destination: "/workspace/x"},       // not lever's shape
		{Source: "/lever/../etc", Destination: "/workspace/../etc"}, // not a clean path
		{Source: "/lever/tmp", Destination: "/tmp"},                 // outside the workspace
	}
	s := ManagerStaleTreeMounts("/lever", tree, want, got)
	if !reflect.DeepEqual(s.Gone, []string{"gone"}) || !reflect.DeepEqual(s.DroppedReadOnly, []string{"kb"}) || !reflect.DeepEqual(s.DroppedPins, []string{"w"}) {
		t.Fatalf("stale = %+v", s)
	}
	if !strings.Contains(s.Fix(), "recreate each missing directory") {
		t.Fatalf("fix with a gone dir = %q", s.Fix())
	}
	s.Gone = nil
	if !strings.Contains(s.Fix(), "keep these directories") || !strings.Contains(s.String(), "kb still mounted read-only") || !strings.Contains(s.String(), "w still pinned") {
		t.Fatalf("dropped only: %q / %q", s, s.Fix())
	}
	if !ManagerStaleTreeMounts("/lever", tree, want, got[:3]).Empty() {
		t.Fatal("the current plan alone is not stale")
	}
	// Paths are sanitized for the terminal.
	if str := (StaleTreeMounts{Gone: []string{"x\x1b[2J"}}).String(); strings.Contains(str, "\x1b") {
		t.Fatalf("unsanitized: %q", str)
	}
}
