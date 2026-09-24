package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// A running container blocks the window: a controller token the instance
// cannot run without fails the apply with the repair, and nothing is stopped
// or started.
func TestDevAuthWindowRefusesBesideRunningContainers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f := patMintRunner("pat-new")
	f.Script(argvPodmanPs, proc.Result{Stdout: "assistant\nworker-a\n"})
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{})
	if err == nil || !strings.Contains(err.Error(), "assistant, worker-a") || !strings.Contains(err.Error(), "`lever stop`, then `lever up`") {
		t.Fatalf("err = %v, want a refusal naming the containers and the repair", err)
	}
	if f.Called(proc.ArgvPrefix("scion", "server")) {
		t.Fatalf("the window touched the hub beside running containers: %+v", f.Calls)
	}
}

// A container list that fails is treated as containers running: the window
// cannot prove it runs agent-free.
func TestDevAuthWindowRefusesWhenTheContainerListFails(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f := proc.NewFakeRunner() // podman ps unscripted: the fake fails it
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{})
	if err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// Work apply can go on without (here the role grant alone, and a controller
// token inside its renew window) is skipped with a warning, not an error.
func TestDevAuthWindowSkipsOptionalWorkBesideRunningContainers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	now := time.Now()
	if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old", Requested: controllerPATScopes(), MintedAt: now, ExpiresAt: now.Add(5 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-new")
	f.Script(argvPodmanPs, proc.Result{Stdout: "assistant\n"})
	warned, warn := collectWarnings()
	hub := newFakeAdminHub("you@github")
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatalf("optional work must not fail apply: %v", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "skipped the dev-auth window") {
		t.Fatalf("warnings = %q", *warned)
	}
	if f.Called(proc.ArgvPrefix("scion", "server")) || len(hub.calls) != 0 {
		t.Fatalf("the window opened: jail %+v, hub %v", f.Calls, hub.calls)
	}
	if tok, _ := st.LoadControllerPAT(); tok != "pat-controller" {
		t.Fatalf("controller PAT = %q, want it kept", tok)
	}
}

func TestPatUrgent(t *testing.T) {
	now := time.Now()
	want := controllerPATScopes()
	rec := func(scopes []string, exp time.Time) state.PATRecord {
		return state.PATRecord{Requested: scopes, ExpiresAt: exp}
	}
	for _, tc := range []struct {
		name  string
		tok   string
		rec   state.PATRecord
		found bool
		want  bool
	}{
		{"current", "t", rec(want, now.Add(300*24*time.Hour)), true, false},
		{"renew window", "t", rec(want, now.Add(5*24*time.Hour)), true, false},
		{"absent", "", state.PATRecord{}, false, true},
		{"no record", "t", state.PATRecord{}, false, true},
		{"scope drift", "t", rec([]string{"agent:manage"}, now.Add(300*24*time.Hour)), true, true},
		{"expired", "t", rec(want, now.Add(-time.Hour)), true, true},
	} {
		if got := patUrgent(tc.tok, tc.rec, tc.found, want, now); got != tc.want {
			t.Errorf("%s: patUrgent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A window that fails after it stopped the live hub starts the live hub
// again, and still stops the throwaway and deletes the dev token.
func TestDevAuthWindowFailureRestartsTheLiveHub(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f2 := proc.NewFakeRunner()
	scriptPATMintChain(f2)
	// FakeRunner fails only unscripted commands; a failing hub link needs a
	// wrapper.
	r := &failingRunner{FakeRunner: f2, fail: "scion hub link"}
	restarted := 0
	err := ensureControllerPAT(context.Background(), r, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{
		RestartHub: func(context.Context) error { restarted++; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "hub link") || !strings.Contains(err.Error(), "live hub was started again") {
		t.Fatalf("err = %v, want the hub link failure plus the restart note", err)
	}
	if restarted != 1 {
		t.Fatalf("RestartHub called %d times, want 1", restarted)
	}
	if n := countCalls(f2.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStop) }); n != 2 {
		t.Fatalf("server stop calls = %d, want 2 (live hub, then the throwaway)", n)
	}
	if !f2.Called(proc.ArgvContains("/home/tester/.scion/dev-token")) {
		t.Fatalf("dev token not deleted: %+v", f2.Calls)
	}
}

// Without a restart hook the error says the live hub is down and how to
// recover; a restart that fails says so too.
func TestDevAuthWindowFailureNamesTheDownHub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart func(context.Context) error
		want    string
	}{
		{"no hook", nil, "the live hub is stopped"},
		{"restart fails", func(context.Context) error { return errors.New("boom") }, "restarting it failed: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			scriptPATMintChain(f)
			r := &failingRunner{FakeRunner: f, fail: "scion init"}
			err := ensureControllerPAT(context.Background(), r, state.ForConfig(t.TempDir()), t.TempDir(), "/lever",
				remoteAccess{}, patMintOpts{RestartHub: tc.restart})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "lever apply") {
				t.Fatalf("err = %v, want %q and the recovery", err, tc.want)
			}
		})
	}
}

// A throwaway that does not stop fails even a successful window: a dev-auth
// hub left running is the surface the cleanup exists to close.
func TestDevAuthWindowThrowawayThatWillNotStopFails(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	f.Script(argvScionTokenCreate, proc.Result{Stdout: "Token: pat-new\n"})
	r := &failingRunner{FakeRunner: f, fail: "scion server stop", after: 1} // the live stop works, the throwaway's does not
	err := ensureControllerPAT(context.Background(), r, state.ForConfig(t.TempDir()), t.TempDir(), "/lever",
		remoteAccess{}, patMintOpts{RestartHub: func(context.Context) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "did not stop") || !strings.Contains(err.Error(), "scion server stop") {
		t.Fatalf("err = %v, want the throwaway stop failure with its stop command", err)
	}
}

// An apply cancelled mid-window (SIGINT/SIGTERM cancel the apply context)
// still closes the window: the cleanup runs on a context of its own.
func TestDevAuthWindowCleanupSurvivesACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	r := &cancellingRunner{FakeRunner: f, at: "scion hub link", cancel: cancel}
	restarted := false
	err := ensureControllerPAT(ctx, r, state.ForConfig(t.TempDir()), t.TempDir(), "/lever", remoteAccess{}, patMintOpts{
		RestartHub: func(c context.Context) error {
			if c.Err() != nil {
				return c.Err()
			}
			restarted = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("a cancelled window must fail")
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStop) }); n != 2 {
		t.Fatalf("server stop calls = %d, want 2 (the throwaway is stopped after the cancel)", n)
	}
	if !f.Called(proc.ArgvContains("/home/tester/.scion/dev-token")) || !restarted {
		t.Fatalf("cleanup incomplete: token delete %v, restart %v; calls %+v",
			f.Called(proc.ArgvContains("/home/tester/.scion/dev-token")), restarted, f.Calls)
	}
}

// W4: a window that opens for another reason re-runs the role grant even
// when remote-role.json says the grant is complete, so a hub whose database
// was reset gets its role and ceiling back.
func TestDevAuthWindowAlwaysRunsTheRoleGrant(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	ra := remoteAccess{Enabled: true, Emails: []string{"you@github"}}
	if err := ensureControllerPAT(context.Background(), patMintRunner("x"), st, t.TempDir(), "/lever", ra,
		patMintOpts{AdminHub: newFakeAdminHub("you@github")}); err != nil {
		t.Fatal(err)
	}
	if rec, found, _ := st.LoadRemoteRoleRecord(); !found || remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()) != "" {
		t.Fatal("setup: want a complete grant record")
	}
	// The hub database is reset (a fresh hub), and the controller token has
	// to be minted again.
	if err := os.Remove(st.ControllerPAT()); err != nil {
		t.Fatal(err)
	}
	fresh := newFakeAdminHub("you@github")
	if err := ensureControllerPAT(context.Background(), patMintRunner("pat-new"), st, t.TempDir(), "/lever", ra,
		patMintOpts{AdminHub: fresh}); err != nil {
		t.Fatal(err)
	}
	if len(fresh.roles) != 1 || len(fresh.bindings) != 1 || len(fresh.constraints) != 1 {
		t.Fatalf("fresh hub after the window: roles %d, bindings %d, ceilings %d; want 1 each (calls %v)",
			len(fresh.roles), len(fresh.bindings), len(fresh.constraints), fresh.calls)
	}
}

// W5: a ceiling replacement that deleted the old constraint and then failed
// to write the new one fails the apply: the user has no ceiling now.
func TestRemoteCeilingDeletedButNotRecreatedFailsApply(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	hub.constraints = []*fakeConstraint{{id: "ac-old", revision: 3, ConstraintDraft: hubapi.ConstraintDraft{
		Name: remoteCeilingName("you@github"), Purpose: "x",
		Subject:            hubapi.ConstraintSubject{Kind: "principal", PrincipalType: "user", PrincipalID: "user-1"},
		Scope:              hubapi.ConstraintScope{Type: "system"},
		MaximumPermissions: []string{"project.create"},
	}}}
	hub.failCreateConstraint = true
	f := patMintRunner("x")
	_, warn := collectWarnings()
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: hub, Warn: warn})
	if !errors.Is(err, errCeilingRemoved) {
		t.Fatalf("err = %v, want errCeilingRemoved", err)
	}
	if hub.count("DELETE /api/v1/admin/access-constraints/ac-old") != 1 {
		t.Fatalf("calls = %v, want the old constraint deleted", hub.calls)
	}
	if _, found, _ := st.LoadRemoteRoleRecord(); found {
		t.Fatal("an uncapped user must not be recorded as granted")
	}
}

// A create that fails with nothing deleted stays a warning.
func TestRemoteCeilingCreateFailureWithoutDeleteIsAWarning(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	hub.failCreateConstraint = true
	warned, warn := collectWarnings()
	if err := ensureControllerPAT(context.Background(), patMintRunner("x"), st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatalf("err = %v, want a warning only", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "remote web role not granted") {
		t.Fatalf("warnings = %q", *warned)
	}
}

func TestCheckDevAuthWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    string
		ok     bool
		inFix  string
		inText string
	}{
		{"closed", "", true, "", "closed"},
		{"token left", "token", false, "rm ~/.scion/dev-token", "dev-token exists"},
		{"hub listening", " listening", false, "scion server stop", "127.0.0.1:48080"},
		{"both", "token listening", false, "scion server stop", "127.0.0.1:48080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script("sh -c out=", proc.Result{Stdout: tc.out})
			got := checkDevAuthWindow(context.Background(), f)
			if got.ok != tc.ok || !strings.Contains(got.fix, tc.inFix) || !strings.Contains(got.detail, tc.inText) {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if got := checkDevAuthWindow(context.Background(), proc.NewFakeRunner()); got.ok {
		t.Fatalf("a failed probe must not report closed: %+v", got)
	}
}

// applySignalContext cancels on SIGINT, so an interrupted apply runs its
// deferred cleanup instead of dying at once.
func TestApplySignalContextCancelsOnSIGINT(t *testing.T) {
	ctx, stop := applySignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the apply context")
	}
}

// failingRunner fails the calls whose argv starts with fail, from the
// (after+1)th such call on; everything else goes to the FakeRunner.
type failingRunner struct {
	*proc.FakeRunner
	fail  string
	after int
	seen  int
}

func (r *failingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if callHasPrefix(proc.Call{Name: name, Args: args}, r.fail) {
		r.seen++
		if r.seen > r.after {
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
			return proc.Result{Code: 1, Stderr: "Error: scripted failure"}, errors.New("exit status 1")
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *failingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// cancellingRunner cancels the apply context when it sees the call at, and
// fails every call made on a cancelled context, as exec.CommandContext does.
type cancellingRunner struct {
	*proc.FakeRunner
	at     string
	cancel context.CancelFunc
}

func (r *cancellingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if callHasPrefix(proc.Call{Name: name, Args: args}, r.at) {
		r.cancel()
	}
	if ctx.Err() != nil {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Code: -1}, ctx.Err()
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *cancellingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}
