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
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// TestPhaseOrAbsent covers the definitive-absence fallback: a failed phase
// probe whose error proves the manager cannot be running must be treated as
// "manager absent" so `up` falls through to upDecision -> "apply". Three such
// signatures: hub unreachable (fresh machine — the hub is only started by
// apply's scion-server step: "is not responding" / "connection refused"),
// hub-side project not found (hub up but the manager project never
// hub-registered, e.g. partial bring-up where `scion init` ran but
// `scion hub link` didn't: "Project not found (status: 404)"), and no local
// registration at all (scion's git-origin fallback when a path isn't a
// registered project: "no git origin remote found"). ANY OTHER probe error
// must propagate: `lever apply` is not fully idempotent (each run leaves a
// duplicate scion project-configs entry), so a transient list failure on an
// already-up instance must NOT force a re-apply.
func TestPhaseOrAbsent(t *testing.T) {
	// The live fresh-machine repro (both signature fragments present).
	freshErr := errors.New(`scion list --format json -g /lever: Error: Hub at http://127.0.0.1:8080 is not responding: Get "http://127.0.0.1:8080/api/health": dial tcp 127.0.0.1:8080: connect: connection refused`)
	// The live partial-bring-up repro: hub up, manager project not registered.
	notFoundErr := errors.New(`scion list --format json -g /lever: Error: failed to list agents via Hub: not_found: Project not found (status: 404)`)
	// The live no-local-registration repro: scion's git-origin fallback when
	// the path isn't a registered project at all (this exact string is
	// documented in internal/scion/bringup.go).
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
		// --fresh is ALWAYS the apply path, whatever the probe saw: apply's
		// start-manager step carries the intent (PlanOpts.Fresh) and discards
		// any present record once the hub is up. Deciding on the probe alone
		// dropped --fresh after every `lever stop`, when the hub is down and
		// the record invisible (lever#33); and since 0.12 apply preserves an
		// error-phase record whose forced resume comes up dead (#3), so
		// --fresh must reach apply for a bricked record too.
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

// The --fresh discard itself (`scion delete`, never `scion stop`, which would
// leave a resumable stopped record) is pinned where it now lives: apply's
// start-manager step, TestStartManagerFreshDiscardsPresentRecord.

// TestFirstLine covers the extraction used to keep the fresh-bring-up probe
// message to one short line: scion's error includes its entire usage dump
// after the first line, which must never reach the user's terminal.
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

// brokerReusable (#19): apply's broker-reuse shortcut may only keep a running broker
// whose /epoch identity matches this binary AND this broker config. A broker
// predating the fields reports them empty — always a mismatch.
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

// TestVerifyManagerRoleGuardsUpsFastPaths: `up` resumes a suspended manager and
// no-ops a running one WITHOUT calling apply.Run, so the pre-role record guard
// has to be invoked on those paths explicitly. This pins that it is wired to
// the same Deps hook, keyed by the hub's project name (the mount basename).
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

// gateAfterUp (lever#31): the paths that act on the manager without apply.Run
// must observe it before "is up." — a resume with the full settle window, a
// running manager with one fresh look — while the apply paths, gated inside
// apply.Run already, add nothing.
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
	// And a dead manager on the resume path is a refusal, not "is up."
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

// TestUpProbeNotice: the line `up` prints when the probe could not see the
// manager. With --fresh it must say the flag survives — that a record found
// once the hub is up will be discarded — instead of the plain "bringing up"
// that read as a silent downgrade to resume (lever#33).
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

// TestUpProbeReason: the probe failure in the operator's words, never the
// raw client error (lever#195); an unknown error keeps its first line only.
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

// up's own resume (lever#158, also after a guest restart, lever#179) runs
// the checks apply's start-manager runs around its resume: a mount whose
// directory is gone refuses before the resume; a tree mount that comes
// from elsewhere after it suspends the manager, right after the resume or
// once it is live, and up never reports it up.
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
	// The resume path never passes the gate without its checks.
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: `[{"slug":"assistant","phase":"running","containerStatus":"Up 1 second"}]`})
	deps := apply.Deps{Scion: scion.New(f, scion.Options{}), ManagerLiveRetry: apply.RetryBudget{Attempts: 2, Interval: time.Millisecond}}
	if err := gateAfterUp(context.Background(), deps, upResume, "/lever", "assistant", nil, nil); err == nil {
		t.Fatal("a resume with no mount checks must not pass the gate")
	}
}

// noMounts and noProblems are mount readers for a manager with no tree
// mounts: wired (BeginManagerResume refuses without them), checking nothing.
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
