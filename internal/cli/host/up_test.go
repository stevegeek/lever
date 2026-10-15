package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// TestPhaseOrAbsent: a probe error proving the manager cannot be running reads
// as absent, so up applies. Three signatures: hub unreachable (fresh machine),
// hub project not found (registered locally but never hub-linked), and no
// local registration (scion's git-origin fallback). Any other error must
// propagate: apply is not fully idempotent, so a transient list failure must
// not force a re-apply.
func TestPhaseOrAbsent(t *testing.T) {
	freshErr := errors.New(`scion list --format json -g /lever: Error: Hub at http://127.0.0.1:8080 is not responding: Get "http://127.0.0.1:8080/api/health": dial tcp 127.0.0.1:8080: connect: connection refused`)
	notFoundErr := errors.New(`scion list --format json -g /lever: Error: failed to list agents via Hub: not_found: Project not found (status: 404)`)
	// scion's git-origin fallback when the path is not a registered project (see
	// internal/scion/bringup.go).
	noOriginErr := errors.New(`scion list --format json -g /lever: Error: no git origin remote found for this project.`)
	cases := []struct {
		name    string
		phase   string
		err     error
		want    string
		wantErr bool
	}{
		{"no error passes phase through unchanged", "running", nil, "running", false},
		{"no error, absent phase stays absent", "", nil, "", false},
		{"live fresh-machine hub error is treated as absent", "", freshErr, "", false},
		{"'is not responding' alone is treated as absent, case-insensitively", "", errors.New("Hub at http://127.0.0.1:8080 IS NOT RESPONDING"), "", false},
		{"'connection refused' alone is treated as absent, case-insensitively", "", errors.New("dial tcp 127.0.0.1:8080: connect: Connection Refused"), "", false},
		{"hub-unreachable overrides any stale phase value", "running", freshErr, "", false},
		{"live project-not-found (404) error is treated as absent", "", notFoundErr, "", false},
		{"'project not found' alone is treated as absent, case-insensitively", "running", errors.New("not_found: PROJECT NOT FOUND (status: 404)"), "", false},
		{"live no-git-origin (unregistered project) error is treated as absent", "", noOriginErr, "", false},
		{"'no git origin remote found' alone is treated as absent, case-insensitively", "running", errors.New("Error: NO GIT ORIGIN REMOTE FOUND for this project."), "", false},
		{"any other error propagates (no forced re-apply)", "running", errors.New("could not parse scion JSON output: unexpected JSON"), "", true},
		{"auth-ish error propagates", "", errors.New("scion list: 401 unauthorized"), "", true},
	}
	for _, c := range cases {
		got, err := phaseOrAbsent(c.phase, c.err)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: phaseOrAbsent(%q,%v) err=%v wantErr=%v", c.name, c.phase, c.err, err, c.wantErr)
			continue
		}
		if c.wantErr && err != c.err {
			t.Errorf("%s: propagated error %v is not the original %v", c.name, err, c.err)
		}
		if got != c.want {
			t.Errorf("%s: phaseOrAbsent(%q,%v)=%q want %q", c.name, c.phase, c.err, got, c.want)
		}
	}
}

func TestUpDecision(t *testing.T) {
	cases := []struct {
		phase string // "" = absent
		fresh bool
		want  upAction
	}{
		{"", false, upApply},
		{"suspended", false, upResume},
		{"running", false, upNone},
		{"stopped", false, upApply},
		// --fresh is always the apply path: start-manager discards any present record
		// once the hub is up. After `lever stop` the hub is down and the record
		// invisible to the probe, and apply keeps an error-phase record whose forced
		// resume died, so --fresh must reach apply.
		{"running", true, upApply},
		{"suspended", true, upApply},
		{"stopped", true, upApply},
		{"error", true, upApply},
		{"", true, upApply},
	}
	for _, c := range cases {
		if got := upDecision(c.phase, c.fresh); got != c.want {
			t.Errorf("upDecision(%q,%v)=%q want %q", c.phase, c.fresh, got, c.want)
		}
	}
}

// The --fresh discard itself (`scion delete`, never `scion stop`, which leaves
// a resumable record) is pinned in TestStartManagerFreshDiscardsPresentRecord.

// TestFirstLine: scion's error carries its usage dump after the first line,
// which must never reach the terminal.
func TestFirstLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"multi-line input keeps only the first line",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused\n\nUsage:\n  scion list [flags]\n\nFlags:\n  -h, --help   help for list\n",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused",
		},
		{"single line input is unchanged", "no git origin remote found for this project.", "no git origin remote found for this project."},
		{"empty input stays empty", "", ""},
		{"leading/trailing whitespace on the first line is trimmed", "  Error: project not found (status: 404)  \nUsage:\n  scion list\n", "Error: project not found (status: 404)"},
		// scion colours its errors; a compromised jail can emit anything.
		{"ANSI colour on the first line is stripped", "\x1b[1;31mError:\x1b[0m hub down\nUsage:\n", "Error: hub down"},
		{"OSC title and CR on the first line are neutralised", "ok\x1b]0;pwned\x07\rrow\nrest", "ok\ufffdrow"},
	}
	for _, c := range cases {
		if got := firstLine(c.input); got != c.want {
			t.Errorf("%s: firstLine(%q)=%q want %q", c.name, c.input, got, c.want)
		}
	}
}

// brokerReusable: only a broker whose /epoch identity matches this binary and
// broker config is kept; empty fields always mismatch.
func TestBrokerReusable(t *testing.T) {
	got := wire.EpochResponse{Version: "v", ConfigHash: "h"}
	if !brokerReusable(got, "v", "h") {
		t.Fatal("matching identity must be reusable")
	}
	if brokerReusable(got, "v2", "h") {
		t.Fatal("binary version drift must force a restart")
	}
	if brokerReusable(got, "v", "h2") {
		t.Fatal("config drift must force a restart")
	}
	if brokerReusable(wire.EpochResponse{}, "v", "h") {
		t.Fatal("an old broker (no identity fields) must force a restart")
	}
}

// TestVerifyManagerRoleGuardsUpsFastPaths: up resumes or keeps the manager
// without apply.Run, so the pre-role guard must be wired on those paths, keyed
// by the hub's project name (the mount basename).
func TestVerifyManagerRoleGuardsUpsFastPaths(t *testing.T) {
	var gotProject, gotAgent string
	deps := apply.Deps{
		VerifyAgentRole: func(_ context.Context, project, agent string) error {
			gotProject, gotAgent = project, agent
			return errors.New("no stored role")
		},
	}
	err := verifyManagerRole(context.Background(), deps, "/lever", "assistant")
	if err == nil {
		t.Fatal("the guard's refusal must reach the caller")
	}
	if gotProject != "lever" || gotAgent != "assistant" {
		t.Errorf("guard called with (%q, %q), want (\"lever\", \"assistant\")", gotProject, gotAgent)
	}
}

// gateAfterUp: paths that bypass apply.Run must observe the manager before "is
// up." (a resume with the settle window, a running manager with one look);
// the apply paths add nothing.
func TestGateAfterUpObservesTheManagerPerPath(t *testing.T) {
	newDeps := func() (apply.Deps, *proc.FakeRunner) {
		f := proc.NewFakeRunner()
		f.Script("scion", proc.Result{Stdout: `[{"slug":"assistant","phase":"running","containerStatus":"Up 1 second"}]`})
		return apply.Deps{
			Scion:                  scion.New(f, scion.Options{}),
			ManagerLiveRetry:       apply.RetryBudget{Attempts: 2, Interval: time.Millisecond, Settle: 5 * time.Millisecond},
			InspectContainerMounts: noMounts,
			CheckLiveMounts:        noProblems,
		}, f
	}
	for _, c := range []struct {
		action   upAction
		minLists int // list calls the gate must make
		maxLists int
	}{
		{upApply, 0, 0},        // apply.Run gated it (--fresh included)
		{upNone, 1, 1},         // one fresh observation, no settle
		{upResume, 2, 1 << 20}, // live, then held: at least one re-observation
	} {
		deps, f := newDeps()
		var guard *apply.ManagerResume
		if c.action == upResume {
			var err error
			if guard, err = apply.BeginManagerResume(context.Background(), deps, &config.App{Name: "assistant", Tree: t.TempDir()}, "/lever", "up"); err != nil {
				t.Fatal(err)
			}
		}
		if err := gateAfterUp(context.Background(), deps, c.action, "/lever", "assistant", nil, guard); err != nil {
			t.Fatalf("%s: a running manager must pass: %v", c.action, err)
		}
		lists := 0
		for _, call := range f.Calls {
			if len(call.Args) > 0 && call.Args[0] == "list" {
				lists++
			}
		}
		if lists < c.minLists || lists > c.maxLists {
			t.Fatalf("%s: %d list call(s), want between %d and %d", c.action, lists, c.minLists, c.maxLists)
		}
	}
	// A dead manager on the resume path is a refusal, not "is up."
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: `[{"slug":"assistant","phase":"error","containerStatus":"Exited (1) 2 seconds ago"}]`})
	deps := apply.Deps{Scion: scion.New(f, scion.Options{}), ManagerLiveRetry: apply.RetryBudget{Attempts: 2, Interval: time.Millisecond},
		InspectContainerMounts: noMounts, CheckLiveMounts: noProblems}
	guard, err := apply.BeginManagerResume(context.Background(), deps, &config.App{Name: "assistant", Tree: t.TempDir()}, "/lever", "up")
	if err != nil {
		t.Fatal(err)
	}
	if err := gateAfterUp(context.Background(), deps, upResume, "/lever", "assistant", nil, guard); err == nil {
		t.Fatal("a resume over a dead manager must not pass the gate")
	}
}

// TestUpProbeNotice: with --fresh the notice must say a record found once the
// hub is up will be discarded, not read as a silent downgrade to resume.
func TestUpProbeNotice(t *testing.T) {
	err := errors.New("hub is not responding\nUsage: scion list ...")
	plain := upProbeNotice(err, false)
	if !strings.Contains(plain, "hub is not running") || strings.Contains(plain, "Usage") || strings.Contains(plain, "--fresh") {
		t.Errorf("plain notice = %q", plain)
	}
	fresh := upProbeNotice(err, true)
	if !strings.Contains(fresh, "--fresh") || !strings.Contains(fresh, "discard") || !strings.Contains(fresh, "hub is not running") {
		t.Errorf("fresh notice = %q, want the flag, the discard, and the reason", fresh)
	}
}

// TestUpProbeReason: the probe failure in the operator's words, never the raw
// client error; an unknown error keeps its first line only.
func TestUpProbeReason(t *testing.T) {
	cases := map[string]string{
		"dial tcp 127.0.0.1:8080: connect: connection refused": "hub is not running",
		"Error: project not found":                             "not registered with the hub yet",
		"weird\nUsage: scion list":                             "no running manager (weird)",
	}
	for in, want := range cases {
		if got := upProbeReason(errors.New(in)); !strings.Contains(got, want) || strings.Contains(got, "Usage") {
			t.Errorf("upProbeReason(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}

func TestUpDecisionNote(t *testing.T) {
	cases := []struct {
		phase string
		d     upAction
		want  string
	}{
		{scion.PhaseRunning, upNone, "running"},
		{scion.PhaseSuspended, upResume, "suspended; resuming it"},
		{scion.PhaseRunning, upResume, "the VM restarted under it; resuming it"},
		{"", upApply, "no manager yet; starting it"},
	}
	for _, c := range cases {
		if got := upDecisionNote(c.phase, c.d); got != c.want {
			t.Errorf("upDecisionNote(%q, %q) = %q, want %q", c.phase, c.d, got, c.want)
		}
	}
}

// up's own resume runs the checks apply's start-manager runs around its
// resume: a mount whose directory is gone refuses before the resume; a tree
// mount from elsewhere suspends the manager, right after the resume or once
// live, and up never reports it up.
func TestResumeForUpChecksTreeMounts(t *testing.T) {
	mounts := []jail.Mount{
		{Source: "/lever", Destination: "/workspace", RW: true},
		{Source: "/lever/kb", Destination: "/workspace/kb"},
	}
	elsewhere := []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}
	for _, tc := range []struct {
		name        string
		noDir       bool
		resumeErr   error
		answers     []error // successive live-check errors; nil = the problems below
		problems    []jail.LiveMountProblem
		wantResume  bool
		wantErr     string // from resumeForUp
		wantGateErr string // from gateAfterUp
		wantSuspend bool
	}{
		{name: "held", answers: []error{nil}, wantResume: true},
		{name: "gone before the resume", noDir: true, wantErr: "cannot be resumed"},
		{name: "elsewhere after the resume", answers: []error{nil}, problems: elsewhere, wantResume: true,
			wantErr: "is mounted from another place", wantSuspend: true},
		{name: "resume failed, container started anyway", answers: []error{nil}, problems: elsewhere, resumeErr: errors.New("hook failed"),
			wantResume: true, wantErr: "is mounted from another place", wantSuspend: true},
		{name: "not running yet, elsewhere once live", answers: []error{jail.ErrContainerNotRunning, nil}, problems: elsewhere,
			wantResume: true, wantGateErr: "is mounted from another place", wantSuspend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			if !tc.noDir {
				if err := os.Mkdir(filepath.Join(tree, "kb"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			f := proc.NewFakeRunner()
			f.Script("scion", proc.Result{Stdout: `[{"slug":"assistant","phase":"running","containerStatus":"Up 1 second"}]`})
			i := 0
			deps := apply.Deps{
				Scion:                  scion.New(f, scion.Options{}),
				ManagerLiveRetry:       apply.RetryBudget{Attempts: 2, Interval: time.Millisecond, Settle: time.Millisecond},
				InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
				CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
					err := tc.answers[min(i, len(tc.answers)-1)]
					i++
					if err != nil {
						return nil, err
					}
					return tc.problems, nil
				},
			}
			resumed := false
			app := &config.App{Name: "assistant", Tree: tree}
			guard, err := resumeForUp(context.Background(), deps, app, "/lever", func(context.Context) error {
				resumed = true
				return tc.resumeErr
			})
			if resumed != tc.wantResume {
				t.Fatalf("resumed = %v, want %v (err %v)", resumed, tc.wantResume, err)
			}
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			default:
				gerr := gateAfterUp(context.Background(), deps, upResume, "/lever", "assistant", nil, guard)
				if tc.wantGateErr == "" && gerr != nil || tc.wantGateErr != "" && (gerr == nil || !strings.Contains(gerr.Error(), tc.wantGateErr)) {
					t.Fatalf("gate err = %v, want %q", gerr, tc.wantGateErr)
				}
			}
			suspended := false
			for _, c := range f.Calls {
				if len(c.Args) > 0 && c.Args[0] == "suspend" {
					suspended = true
				}
			}
			if suspended != tc.wantSuspend {
				t.Fatalf("suspended = %v, want %v (calls %+v)", suspended, tc.wantSuspend, f.Calls)
			}
		})
	}
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: `[{"slug":"assistant","phase":"running","containerStatus":"Up 1 second"}]`})
	deps := apply.Deps{Scion: scion.New(f, scion.Options{}), ManagerLiveRetry: apply.RetryBudget{Attempts: 2, Interval: time.Millisecond}}
	if err := gateAfterUp(context.Background(), deps, upResume, "/lever", "assistant", nil, nil); err == nil {
		t.Fatal("a resume with no mount checks must not pass the gate")
	}
}

// noMounts and noProblems are wired mount readers that check nothing
// (BeginManagerResume refuses without them).
func noMounts(context.Context, string) ([]jail.Mount, error) { return nil, nil }

func noProblems(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
	return nil, nil
}

// up's resume checks the host tree first, as apply does: a read_only entry
// reached through a symbolic link refuses before anything is resumed.
func TestResumeForUpRefusesLinkedReadOnly(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(tree, "tools")); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Name: "assistant", Tree: tree}
	app.Manager.ReadOnly = []string{"tools"}
	deps := apply.Deps{InspectContainerMounts: noMounts, CheckLiveMounts: noProblems}
	resumed := false
	_, err := resumeForUp(context.Background(), deps, app, "/lever", func(context.Context) error { resumed = true; return nil })
	if err == nil || resumed || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("err = %v resumed %v, want a refusal before the resume", err, resumed)
	}
}

// TestEnsureRemoteForUp: up checks the proxy only with remote access on, and
// a proxy it cannot start is a warning naming the fix, not a failed up.
func TestEnsureRemoteForUp(t *testing.T) {
	rep := progress.New(progress.Options{Mode: progress.ModeQuiet, Command: "up"})
	on := &config.App{Name: "assistant", Remote: config.Remote{Enabled: true}}
	called := false
	if w := ensureRemoteForUp(context.Background(), rep, &config.App{Name: "assistant"}, func(context.Context) error { called = true; return nil }); w != "" || called {
		t.Fatalf("remote off: warning %q, ensure called %v; want neither", w, called)
	}
	if w := ensureRemoteForUp(context.Background(), rep, on, func(context.Context) error { return nil }); w != "" {
		t.Fatalf("a serving proxy warned: %q", w)
	}
	for _, err := range []error{errRemoteProxyNoStamp, errRemoteProxyStale, errors.New("bind: address already in use")} {
		w := ensureRemoteForUp(context.Background(), rep, on, func(context.Context) error { return err })
		if !strings.Contains(w, "proxy") || !strings.Contains(w, "lever apply") || !strings.Contains(w, err.Error()) {
			t.Errorf("warning for %v = %q, want the cause and `lever apply`", err, w)
		}
	}
}
