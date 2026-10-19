package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/testutil"
)

func runApply(app *config.App, deps Deps) error {
	return Run(context.Background(), app, fillDeps(deps), PlanOpts{})
}

// fillDeps fills every required collaborator the test left nil with an inert
// one, so a test names only what it asserts on. The inert mint returns fresh
// material (boot.minted = true); a test that models a spent latch wires
// spentLatchMint instead.
func fillDeps(d Deps) Deps {
	if d.ManagerLock == nil {
		d.ManagerLock = HeldManagerLock
	}
	// The production default holds the manager live for 10 s; a test that
	// does not model the settle window must not pay for it.
	if d.ManagerLiveRetry == (RetryBudget{}) {
		d.ManagerLiveRetry = RetryBudget{Attempts: 15, Interval: time.Millisecond}
	}
	if d.PhaseSettleRetry == (RetryBudget{}) {
		d.PhaseSettleRetry = RetryBudget{Attempts: 5, Interval: time.Millisecond}
	}
	if d.LoadImage == nil {
		d.LoadImage = func(context.Context, string) error { return nil }
	}
	if d.ImageLoaded == nil {
		d.ImageLoaded = func(context.Context, string) bool { return false }
	}
	if d.LoadImageTar == nil {
		d.LoadImageTar = func(context.Context, string, string, func(string) error) error { return nil }
	}
	if d.ImageLoadedTar == nil {
		d.ImageLoadedTar = func(context.Context, string, string) bool { return false }
	}
	if d.PruneImages == nil {
		d.PruneImages = func(context.Context) error { return nil }
	}
	if d.WarmImage == nil {
		d.WarmImage = func(context.Context, string) error { return nil }
	}
	if d.StartBroker == nil {
		d.StartBroker = func(context.Context) error { return nil }
	}
	if d.BrokerHealthy == nil {
		d.BrokerHealthy = func(context.Context) error { return nil }
	}
	if d.EnsureHubIdentity == nil {
		d.EnsureHubIdentity = func(context.Context) error { return nil }
	}
	if d.EnsureProjectProvider == nil {
		d.EnsureProjectProvider = func(context.Context, string) error { return nil }
	}
	if d.WaitBrokerReady == nil {
		d.WaitBrokerReady = func(context.Context, string) (bool, error) { return true, nil }
	}
	if d.BrokerOnline == nil {
		d.BrokerOnline = func(context.Context) (bool, string, error) { return true, "", nil }
	}
	if d.MintManagerBootstrap == nil {
		d.MintManagerBootstrap = func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{Ticket: "minted-this-run"}, nil
		}
	}
	if d.RearmBootstrap == nil {
		d.RearmBootstrap = func(context.Context) error { return nil }
	}
	if d.EnsureHubAuth == nil {
		d.EnsureHubAuth = func(context.Context) (string, error) { return "", nil }
	}
	if d.EnsureScionTelemetry == nil {
		d.EnsureScionTelemetry = func(context.Context) (bool, error) { return false, nil }
	}
	if d.EnsureAgentTemplate == nil {
		d.EnsureAgentTemplate = func(context.Context, string) (bool, error) { return false, nil }
	}
	if d.StartRemoteProxy == nil {
		d.StartRemoteProxy = func(context.Context) error { return nil }
	}
	if d.StopRemoteProxy == nil {
		d.StopRemoteProxy = func(context.Context) error { return nil }
	}
	if d.FenceRemoteLogins == nil {
		d.FenceRemoteLogins = func(context.Context) error { return nil }
	}
	if d.RemoveJailFile == nil {
		d.RemoveJailFile = func(context.Context, string) error { return nil }
	}
	if d.RemoveScionProjectConfigs == nil {
		d.RemoveScionProjectConfigs = func(context.Context, string) error { return nil }
	}
	if d.ScionProjectRegistered == nil {
		d.ScionProjectRegistered = func(context.Context, string) (bool, error) { return false, nil }
	}
	if d.ProjectLinked == nil {
		d.ProjectLinked = func(context.Context, string) (bool, error) { return true, nil }
	}
	if d.StripProjectSharedDirs == nil {
		d.StripProjectSharedDirs = func(context.Context, string) error { return nil }
	}
	if d.RepairScionHubEndpoint == nil {
		d.RepairScionHubEndpoint = func(context.Context, string) error { return nil }
	}
	if d.EnsureAgentRoleCeiling == nil {
		d.EnsureAgentRoleCeiling = func(context.Context, string) error { return nil }
	}
	if d.VerifyAgentRole == nil {
		d.VerifyAgentRole = func(context.Context, string, string) error { return nil }
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return d
}

// spentLatchMint models a re-apply against a broker whose single-use
// /bootstrap latch is already consumed: a ticket from the earlier apply is
// staged in tree, and the mint reports ErrBootstrapLatched, so the step
// tolerates it and boot.minted stays false.
func spentLatchMint(t *testing.T, tree string) func(context.Context) (BootstrapMaterial, error) {
	t.Helper()
	if err := StageBootstrapMaterial(tree, BootstrapMaterial{Ticket: "from-an-earlier-apply"}); err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (BootstrapMaterial, error) {
		return BootstrapMaterial{}, ErrBootstrapLatched
	}
}

// isObserveList tells scion.Client.List (one project, no --all) apart from
// waitHubReady's `list --all` liveness probe. The probe runs before
// start-manager's observe List and must fall through to the blanket "ok"
// script, not consume or corrupt the observe-first fakes' state.
func isObserveList(args []string) bool {
	return len(args) > 0 && args[0] == "list" && !slices.Contains(args, "--all")
}

// flakyStartRunner fails the first startFails `scion start` calls with the
// runtime-broker-unavailable error (the registration race). Its first observe
// List reports the slug absent so the create path is taken; later ones report
// it running so the post-start verify converges.
type flakyStartRunner struct {
	*proc.FakeRunner
	slug       string
	startFails int
	startCalls int
	listCalls  int
}

func (r *flakyStartRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" {
		if isObserveList(args) {
			return absentThenRunningList(r.FakeRunner, &r.listCalls, r.slug, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		}
		hasStart, hasServer, isProbe := false, false, false
		for _, a := range args {
			if a == "start" {
				hasStart = true
			}
			if a == "server" {
				hasServer = true
			}
			// `scion start --help` is the agent-role capability probe
			// (Client.roleFlagSupported), not an agent start.
			if a == "--help" {
				isProbe = true
			}
		}
		if isProbe {
			// Answer as a scion WITHOUT --role, matching the pins lever can fetch.
			return proc.Result{Stdout: "Flags:\n      --harness-auth string   Override auth method\n"}, nil
		}
		if hasStart && !hasServer { // agent start, not `scion server start`
			r.startCalls++
			if r.startCalls <= r.startFails {
				// Client.run builds its error from Stdout+Stderr, so the marker must
				// live there, not just in the Go error.
				return proc.Result{Code: 1, Stderr: "no_runtime_broker: No runtime brokers available for this project"}, fmt.Errorf("exit status 1")
			}
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *flakyStartRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// alreadyUpRunner simulates a fully-up instance on re-apply: `scion server
// start` and agent `start` return "already running". Its first observe List
// reports the slug absent so Start is still called and its AlreadyRunning
// tolerance exercised; later ones report it running.
type alreadyUpRunner struct {
	*proc.FakeRunner
	slug      string
	listCalls int
}

func (r *alreadyUpRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" {
		if isObserveList(args) {
			return absentThenRunningList(r.FakeRunner, &r.listCalls, r.slug, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		}
		hasServer, hasStart := false, false
		for _, a := range args {
			if a == "server" {
				hasServer = true
			}
			if a == "start" {
				hasStart = true
			}
		}
		if hasServer && hasStart {
			return proc.Result{Code: 1, Stderr: "Error: server is already running (PID: 123)"}, fmt.Errorf("exit status 1")
		}
		if hasStart && !hasServer {
			return proc.Result{Code: 1, Stderr: "Error: agent already running"}, fmt.Errorf("exit status 1")
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *alreadyUpRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// agentLifecycleRunner fakes `scion list/start/resume/suspend/delete` around a
// single manager-agent record. An empty initPhase means absent; a successful
// start/resume advances the record to liveWhenPhase/liveWhenContainer
// (default "running"); delete clears it; the *Err fields make a verb fail and
// leave the record unchanged. Other scion verbs fall through to the wrapped
// FakeRunner. Intercepted calls are recorded into f.Calls too, so argv
// assertions work either way.
//
// The zero value (only slug set) models "nothing running yet": the first list
// reads absent and the create-then-verify path is taken.
type agentLifecycleRunner struct {
	*proc.FakeRunner
	slug string

	initPhase, initContainerStatus   string
	liveWhenPhase, liveWhenContainer string

	resumeErr, deleteErr, startErr, listErr error
	// resumeFailsThenSucceed, when > 0, makes resume fail for exactly this
	// many calls, then succeed (a transient broker race). Zero with resumeErr
	// set fails every call.
	resumeFailsThenSucceed int
	deleteFailsThenSucceed int
	// dieAfterLiveLists, when > 0, keeps the live record running for this
	// many list calls and then flips it to error/Exited: the harness died
	// after scion reported success.
	dieAfterLiveLists int
	liveLists         int
	// listFailsThenSucceed, when > 0, makes the observe List fail for exactly
	// this many calls, then succeed. Zero with listErr set fails every call.
	listFailsThenSucceed int
	// healerRecoversOnResumeFail models the broker's auto-re-enrol healer
	// bouncing the record concurrently: resume errors but the agent comes up.
	healerRecoversOnResumeFail bool
	// settleAfterLists, when > 0, flips a transitional record to
	// settlePhase/settleContainer after this many observe lists.
	settleAfterLists             int
	settlePhase, settleContainer string

	phase, containerStatus string
	inited                 bool

	startCalls, resumeCalls, deleteCalls, listCalls int
	resumeForceCalls                                int // subset of resumeCalls carrying --force
	suspendCalls                                    int
	suspendErr                                      error
}

func (r *agentLifecycleRunner) ensureInit() {
	if !r.inited {
		r.phase, r.containerStatus = r.initPhase, r.initContainerStatus
		r.inited = true
	}
}

func (r *agentLifecycleRunner) goLive() {
	p, c := r.liveWhenPhase, r.liveWhenContainer
	if p == "" {
		p = "running"
	}
	if c == "" {
		c = "running"
	}
	r.phase, r.containerStatus = p, c
}

func (r *agentLifecycleRunner) record(dir string, env map[string]string, name string, args []string) {
	r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
}

// verb skips the leading `-g <project>` pair that scion.Client.Start puts
// before its verb.
func (r *agentLifecycleRunner) verb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	// `start --help` is the agent-role capability probe; it must not count as
	// a start or mutate the record.
	if len(args) == 2 && args[0] == "start" && args[1] == "--help" {
		return "role-probe"
	}
	if args[0] == "-g" && len(args) > 2 {
		return args[2]
	}
	if args[0] == "list" && !isObserveList(args) {
		// waitHubReady's probe (see isObserveList).
		return ""
	}
	return args[0]
}

func (r *agentLifecycleRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name != "scion" {
		return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
	}
	r.ensureInit()
	switch r.verb(args) {
	case "role-probe":
		// Answer as a scion WITHOUT --role, matching the pins lever can fetch.
		return proc.Result{Stdout: "Flags:\n      --harness-auth string   Override auth method\n"}, nil
	case "list":
		r.listCalls++
		r.record(dir, env, name, args)
		if r.settleAfterLists > 0 && r.listCalls > r.settleAfterLists {
			r.phase, r.containerStatus = r.settlePhase, r.settleContainer
			r.settleAfterLists = 0
		}
		if r.dieAfterLiveLists > 0 && r.phase == "running" {
			r.liveLists++
			if r.liveLists > r.dieAfterLiveLists {
				r.phase, r.containerStatus = "error", "Exited (1) 2 seconds ago"
			}
		}
		if r.listErr != nil && (r.listFailsThenSucceed == 0 || r.listCalls <= r.listFailsThenSucceed) {
			return proc.Result{Code: 1, Stderr: r.listErr.Error()}, r.listErr
		}
		if r.phase == "" {
			return proc.Result{Stdout: "[]"}, nil
		}
		return proc.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":%q,"containerStatus":%q}]`, r.slug, r.phase, r.containerStatus)}, nil
	case "start":
		r.startCalls++
		r.record(dir, env, name, args)
		if r.startErr != nil {
			return proc.Result{Code: 1, Stderr: r.startErr.Error()}, r.startErr
		}
		r.goLive()
		return proc.Result{Stdout: "ok"}, nil
	case "resume":
		r.resumeCalls++
		for _, a := range args {
			if a == "--force" {
				r.resumeForceCalls++
			}
		}
		r.record(dir, env, name, args)
		if r.resumeErr != nil && (r.resumeFailsThenSucceed == 0 || r.resumeCalls <= r.resumeFailsThenSucceed) {
			if r.healerRecoversOnResumeFail {
				r.goLive()
			}
			return proc.Result{Code: 1, Stderr: r.resumeErr.Error()}, r.resumeErr
		}
		r.goLive()
		return proc.Result{Stdout: "ok"}, nil
	case "suspend":
		r.suspendCalls++
		r.record(dir, env, name, args)
		if r.suspendErr != nil {
			return proc.Result{Code: 1, Stderr: r.suspendErr.Error()}, r.suspendErr
		}
		r.phase, r.containerStatus = "suspended", "stopped"
		return proc.Result{Stdout: "ok"}, nil
	case "delete":
		r.deleteCalls++
		r.record(dir, env, name, args)
		if r.deleteErr != nil && (r.deleteFailsThenSucceed == 0 || r.deleteCalls <= r.deleteFailsThenSucceed) {
			return proc.Result{Code: 1, Stderr: r.deleteErr.Error()}, r.deleteErr
		}
		r.phase, r.containerStatus = "", ""
		return proc.Result{Stdout: "ok"}, nil
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *agentLifecycleRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// RunStdin is Start's transport when the manager has standing instructions
// (`--config -`). The stream is kept on the recorded call for assertions.
func (r *agentLifecycleRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	res, err := r.RunIn(ctx, "", env, name, args...)
	if n := len(r.FakeRunner.Calls); n > 0 && len(in) > 0 {
		r.FakeRunner.Calls[n-1].Stdin = string(in)
	}
	return res, err
}

func newObserveFirstApp(t *testing.T) (*config.App, *proc.FakeRunner) {
	t.Helper()
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	return app, f
}

// TestStartManagerObserveFirstCreatesWhenAbsent: an absent record is created,
// not resumed or deleted. It also pins `manager.model` on the create argv:
// scion resume has no --model, so create is the only path that carries it.
func TestStartManagerObserveFirstCreatesWhenAbsent(t *testing.T) {
	app, f := newObserveFirstApp(t)
	app.Manager.Model = "claude-opus-5"
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // initPhase "" == absent
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1", r.startCalls)
	}
	if r.resumeCalls != 0 || r.deleteCalls != 0 {
		t.Errorf("resumeCalls=%d deleteCalls=%d, want 0/0 (absent record must CREATE, not resume/delete)", r.resumeCalls, r.deleteCalls)
	}
	if !sawScionCall(f, "-- hello") || !sawScionCall(f, "--model claude-opus-5") {
		t.Errorf("manager create must carry the configured model; argv=%q", joinedCalls(f))
	}
}

// TestStartManagerObserveFirstResumesSuspended: a suspended record must be
// resumed (conversation restored), never blind-started (which would 409).
func TestStartManagerObserveFirstResumesSuspended(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (suspended record must RESUME, not blind-start)", r.startCalls)
	}
}

// TestStartManagerObserveFirstResumesStopped: `scion resume` covers stopped
// records as well as suspended ones.
func TestStartManagerObserveFirstResumesStopped(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "stopped"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (stopped record must RESUME, not blind-start)", r.startCalls)
	}
}

// TestStartManagerObserveFirstNoOpWhenRunning: a live record needs neither
// Start nor Resume.
func TestStartManagerObserveFirstNoOpWhenRunning(t *testing.T) {
	app, f := newObserveFirstApp(t)
	// containerStatus carries podman's status text ("Up 6 seconds"), not a
	// canonical "running"; the liveness gate must accept it.
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 6 seconds"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (an already-running manager is a pure no-op)", r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerRunningRecordButDeadContainerFailsLoud proves the no-op
// branch still runs the liveness verify: a record reporting Phase=="running"
// whose ContainerStatus never comes up (the harness died) must fail loudly,
// not silently pass just because the switch took the no-op path.
func TestStartManagerRunningRecordButDeadContainerFailsLoud(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "stopped"}
	deps := Deps{
		ManagerLiveRetry: fastRetry(3),
		Scion:            scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, `phase "running"`, `container "stopped"`)
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (the no-op branch must not start/resume)", r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerResumeFailsKeepsTheManager: a failed resume refuses. The
// record and its conversation stay; nothing is deleted or created, since
// discarding a session is `up --fresh`, which the error names. The error is
// non-transient, so it must not be retried (resumeCalls == 1).
func TestStartManagerResumeFailsKeepsTheManager(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent 'hello': agent does not exist"),
	}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "resume of the manager failed", "agent does not exist", "did NOT delete", "lever up --fresh")
	// The exit code is what a script reads; lever's own sentences come
	// before the hub's words, which only follow "Cause:".
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != cli.ExitManagerResumeFailed {
		t.Fatalf("a failed resume must exit %d: %v", cli.ExitManagerResumeFailed, err)
	}
	if msg := err.Error(); strings.Index(msg, "did NOT delete") > strings.Index(msg, "Cause:") || strings.Index(msg, "agent does not exist") < strings.Index(msg, "Cause:") {
		t.Fatalf("the hub's text must come last, after \"Cause:\": %q", msg)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0: a failed resume must keep the record", r.deleteCalls, r.startCalls)
	}
	for _, l := range logged.lines {
		if strings.Contains(l, "FRESH") {
			t.Fatalf("logged a fresh start: %q", l)
		}
	}
}

// TestStartManagerResumeRefusedByHubKeepsTheManager: a resume the hub refuses
// (scion answers 409 "already exists" to a user token without
// agent:lifecycle; a 403 is the same class) says nothing about the manager's
// state, so start-manager fails without deleting it and names the fix.
func TestStartManagerResumeRefusedByHubKeepsTheManager(t *testing.T) {
	for _, refusal := range []string{
		`failed to start agent via Hub: conflict: agent "hello" already exists in this project (status: 409)`,
		`failed to start agent via Hub: forbidden: Missing required scope: agent:lifecycle (status: 403)`,
	} {
		app, f := newObserveFirstApp(t)
		r := &agentLifecycleRunner{
			FakeRunner: f, slug: "hello",
			initPhase: "stopped", initContainerStatus: "stopped",
			resumeErr: errors.New(refusal),
		}
		var logged logSink
		err := runApply(app, Deps{Scion: scion.New(r, scion.Options{}), Log: logged.logf})
		if err == nil {
			t.Fatalf("%s: apply succeeded, want it to stop", refusal)
		}
		if r.deleteCalls != 0 || r.startCalls != 0 {
			t.Fatalf("%s: deleteCalls=%d startCalls=%d, want 0 (the manager must be kept)", refusal, r.deleteCalls, r.startCalls)
		}
		if !strings.Contains(err.Error(), "lever kept the manager") || !strings.Contains(err.Error(), "agent:lifecycle") {
			t.Fatalf("error %q does not say the manager was kept and why", err)
		}
		// A refusal is not the resume-failed case: its own exit code, and
		// none of that case's wording (--fresh is the wrong answer to it).
		var ee *cli.ExitError
		if !errors.As(err, &ee) || ee.Code != cli.ExitHubRefusedResume || strings.Contains(err.Error(), "did NOT delete the manager") {
			t.Fatalf("a hub refusal must exit %d and not read as resume-failed: %v", cli.ExitHubRefusedResume, err)
		}
		for _, l := range logged.lines {
			if strings.Contains(l, "FRESH") {
				t.Fatalf("logged a fresh start: %q", l)
			}
		}
	}
}

// TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds: `scion
// resume` shares Start's runtime-broker registration race, so a
// broker-unavailable resume must be retried on the BrokerStartRetry budget; a
// transient blip must never destroy a resumable conversation.
func TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		// scion resume's own transient wording (singular "broker").
		resumeErr:              fmt.Errorf("cannot resume agent: no runtime broker available"),
		resumeFailsThenSucceed: 2,
	}
	var logged logSink
	deps := Deps{
		BrokerStartRetry: fastRetry(5),
		Scion:            scion.New(r, scion.Options{}),
		Log:              logged.logf,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run should succeed once the broker race resolves within the retry budget: %v", err)
	}
	if r.resumeCalls != 3 {
		t.Fatalf("resumeCalls = %d, want 3 (2 transient failures + 1 success)", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 — a resume that eventually succeeds must NOT delete/recreate (conversation preserved)", r.deleteCalls, r.startCalls)
	}
	if len(logged.lines) != 0 {
		t.Errorf("no loud recovery log expected when resume eventually succeeds, got %+v", logged.lines)
	}
}

// TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRefuses: if the
// broker race never resolves within the retry budget, start-manager errors
// and keeps the record; it neither hangs nor discards a conversation over a
// late runtime broker.
func TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRefuses(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent: no runtime broker available"),
	}
	var logged logSink
	deps := Deps{
		BrokerStartRetry: fastRetry(3),
		Scion:            scion.New(r, scion.Options{}),
		Log:              logged.logf,
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "no runtime broker available", "did NOT delete")
	if r.resumeCalls != 3 {
		t.Fatalf("resumeCalls = %d, want 3 (the full retry budget burned)", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (the record is kept)", r.deleteCalls, r.startCalls)
	}
}

// TestIsBrokerUnavailable pins the transient runtime-broker wordings that must
// be retried during start-manager, including the cold-VM "deadline exceeded".
func TestIsBrokerUnavailable(t *testing.T) {
	transient := []string{
		"No runtime brokers available",
		"cannot resume agent: no runtime broker available",
		"error: no_runtime_broker",
		"context deadline exceeded",
		"Post \"http://hub\": context deadline exceeded",
	}
	for _, s := range transient {
		if !scion.IsBrokerUnavailable(fmt.Errorf("%s", s)) {
			t.Errorf("scion.IsBrokerUnavailable(%q) = false, want true (transient)", s)
		}
	}
	for _, s := range []string{
		"authentication failed", "project not found", "permission denied",
		"invalid deadline configured", // "deadline" without "exceeded" must NOT match
	} {
		if scion.IsBrokerUnavailable(fmt.Errorf("%s", s)) {
			t.Errorf("scion.IsBrokerUnavailable(%q) = true, want false (real failure)", s)
		}
	}
	if scion.IsBrokerUnavailable(nil) {
		t.Error("scion.IsBrokerUnavailable(nil) = true, want false")
	}
}

// TestStartManagerObserveListRetriesOnTransientThenSucceeds: the observe List
// is start-manager's first hub call, so on a cold VM it can hit the broker
// registration window ("deadline exceeded"). It must ride the same bounded
// retry as Start/Resume. The record is already running so only the observe
// retry is exercised.
func TestStartManagerObserveListRetriesOnTransientThenSucceeds(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "running", initContainerStatus: "Up 3 seconds",
		listErr:              fmt.Errorf("Get \"http://hub/agents\": context deadline exceeded"),
		listFailsThenSucceed: 2,
	}
	deps := Deps{
		BrokerStartRetry: fastRetry(5),
		Scion:            scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("a transient observe-List blip must be retried, not fail the apply: %v", err)
	}
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (already-running record is a no-op once observed)", r.startCalls, r.resumeCalls)
	}
	if r.listCalls < 3 {
		t.Errorf("listCalls = %d, want >= 3 (2 retried failures + success)", r.listCalls)
	}
}

// keptHub answers the hub probe with a hub started as lever starts it for
// app, so the scion-server step keeps it and only looks at its broker.
func keptHub(f *proc.FakeRunner, app *config.App) {
	scriptRunningHub(f, runningHubOut(42, matchingHubArgv(app), testHubAuthEnv()))
}

// TestStartManagerWaitsForBrokerReadyBeforeActing: on a hub that kept
// running with no online runtime broker, the readiness gate must run before
// the observe/create, or it races the broker's async registration.
func TestStartManagerWaitsForBrokerReadyBeforeActing(t *testing.T) {
	app, f := newObserveFirstApp(t)
	keptHub(f, app)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent record → create
	var waitCalls, startCallsAtGate int
	deps := Deps{
		Scion:      scion.New(r, scion.Options{}),
		HubAuthEnv: testHubAuthEnv(),
		WaitBrokerReady: func(ctx context.Context, project string) (bool, error) {
			if project == "global" {
				return false, nil // the kept hub's broker is not online
			}
			waitCalls++
			startCallsAtGate = r.startCalls
			return true, nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waitCalls != 1 {
		t.Errorf("WaitBrokerReady calls = %d, want 1", waitCalls)
	}
	if startCallsAtGate != 0 {
		t.Errorf("start ran (%d) before the readiness gate — the gate must precede any action", startCallsAtGate)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (absent record creates, after the gate)", r.startCalls)
	}
}

// When the scion-server step saw the broker online after its start,
// start-manager does not wait for it a second time in the same run.
func TestStartManagerSkipsTheBrokerWaitWhenScionServerSawItOnline(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	var waits []string
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		WaitBrokerReady: func(_ context.Context, project string) (bool, error) {
			waits = append(waits, project)
			return true, nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(waits) != 0 {
		t.Fatalf("soft waits = %q, want none: scion-server's hard wait saw it", waits)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1", r.startCalls)
	}
}

// TestStartManagerBrokerReadyErrorAbortsBeforeActing: a gate error (only ctx
// cancellation; the gate is otherwise fail-soft) aborts start-manager before
// it touches any record.
func TestStartManagerBrokerReadyErrorAbortsBeforeActing(t *testing.T) {
	app, f := newObserveFirstApp(t)
	keptHub(f, app)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	deps := Deps{
		Scion:      scion.New(r, scion.Options{}),
		HubAuthEnv: testHubAuthEnv(),
		WaitBrokerReady: func(ctx context.Context, project string) (bool, error) {
			if project == "global" {
				return false, nil // the kept hub's broker is not online
			}
			return false, context.Canceled
		},
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "start-manager: waiting for runtime broker")
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (no action when the gate errors)", r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerLivenessNeverGreenAfterCreate: `scion start` can report
// success for a container that never comes up. The liveness verify must fail
// with the last observed phase/container rather than trust the exit code.
func TestStartManagerLivenessNeverGreenAfterCreate(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		liveWhenContainer: "stopped", // Start "succeeds" but the container never lives
	}
	deps := Deps{
		ManagerLiveRetry: fastRetry(3),
		Scion:            scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "did not come up", `container "stopped"`)
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (Start itself must still have been attempted)", r.startCalls)
	}
}

// TestStartManagerErrorPhaseForcedResumeFailsKeepsTheManager: a crashed
// manager (phase "error") gets `resume --force`; when that fails too, the
// apply errors and the record and its conversation stay.
func TestStartManagerErrorPhaseForcedResumeFailsKeepsTheManager(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("container state corrupt")}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "resume --force of the manager failed", "container state corrupt", "did NOT delete", "lever up --fresh")
	if r.resumeForceCalls != 1 {
		t.Errorf("resumeForceCalls = %d, want 1 (error phase must TRY resume --force)", r.resumeForceCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (the record is kept)", r.deleteCalls, r.startCalls)
	}
	for _, l := range logged.lines {
		if strings.Contains(l, "FRESH") {
			t.Fatalf("logged a fresh start: %q", l)
		}
	}
}

// TestStartManagerErrorPhaseForcedResumeRecovers: an error-phase record is
// re-ticketed and `resume --force`d; when that succeeds the conversation
// survives, with no delete, create or loss notice.
func TestStartManagerErrorPhaseForcedResumeRecovers(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "stopped"}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeForceCalls != 1 {
		t.Errorf("resumeForceCalls = %d, want 1", r.resumeForceCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (forced resume succeeded — the conversation must survive)", r.deleteCalls, r.startCalls)
	}
	for _, l := range logged.lines {
		if strings.Contains(l, "previous session lost") {
			t.Fatalf("no loss notice expected on successful forced resume, got %q", l)
		}
	}
}

// TestStartManagerResumeFailButHealerRecovered: apply's resume fails but
// re-observation shows the record running, because the broker's auto-re-enrol
// healer bounced it concurrently. Apply must not delete and recreate, which
// would destroy the conversation the healer restored.
func TestStartManagerResumeFailButHealerRecovered(t *testing.T) {
	for _, phase := range []string{"error", "suspended"} {
		t.Run(phase, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: phase, initContainerStatus: "stopped",
				resumeErr:                  fmt.Errorf("verb conflict: agent transition in flight"),
				healerRecoversOnResumeFail: true}
			var logged logSink
			deps := Deps{
				Scion: scion.New(r, scion.Options{}),
				Log:   logged.logf,
			}
			if err := runApply(app, deps); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if r.deleteCalls != 0 || r.startCalls != 0 {
				t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (record is running — the healer recovered it, apply must keep the session)", r.deleteCalls, r.startCalls)
			}
			if len(logged.lines) != 1 || !strings.Contains(logged.lines[0], "recovered concurrently") {
				t.Fatalf("expected one 'recovered concurrently' log line, got %+v", logged.lines)
			}
			for _, l := range logged.lines {
				if strings.Contains(l, "previous session lost") {
					t.Fatalf("no loss notice when the record survived, got %q", l)
				}
			}
		})
	}
}

// TestStartManagerTransitionalPhaseSettlesThenConverges: a record caught
// mid-transition (a concurrent `lever stop`, or an interrupted `up`) is polled
// until scion settles it, and converge acts on the settled phase.
func TestStartManagerTransitionalPhaseSettlesThenConverges(t *testing.T) {
	cases := []struct {
		name, from, to, toContainer       string
		wantStart, wantResume, wantDelete int
	}{
		{"starting settles running: no-op", "starting", "running", "Up 2 seconds", 0, 0, 0},
		{"stopping settles suspended: resume", "stopping", "suspended", "stopped", 0, 1, 0},
		{"created settles stopped: resume", "created", "stopped", "stopped", 0, 1, 0},
		{"provisioning vanishes: create", "provisioning", "", "", 1, 0, 0},
		// "resumed" is scion's local-runtime interim status after a resume,
		// held until the container's sciontool reports "running" (observed
		// for 16s).
		{"resumed settles running: no-op", "resumed", "running", "Up 2 seconds", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: tc.from, initContainerStatus: "",
				settleAfterLists: 3, settlePhase: tc.to, settleContainer: tc.toContainer}
			var logged logSink
			deps := Deps{
				Scion: scion.New(r, scion.Options{}),
				Log:   logged.logf,
			}
			if err := runApply(app, deps); err != nil {
				t.Fatalf("a %q record that settles to %q must converge, not fail: %v", tc.from, tc.to, err)
			}
			if r.startCalls != tc.wantStart || r.resumeCalls != tc.wantResume || r.deleteCalls != tc.wantDelete {
				t.Fatalf("start/resume/delete = %d/%d/%d, want %d/%d/%d", r.startCalls, r.resumeCalls, r.deleteCalls, tc.wantStart, tc.wantResume, tc.wantDelete)
			}
			if r.listCalls < 4 {
				t.Errorf("listCalls = %d, want >= 4 (the transitional record must be re-observed until it settles)", r.listCalls)
			}
			for _, l := range logged.lines {
				if strings.Contains(l, "previous session lost") || strings.Contains(l, "FRESH") {
					t.Fatalf("no loss notice for a record that settled, got %q", l)
				}
			}
		})
	}
}

// TestStartManagerTransitionalPhaseNeverSettlesFailsWithoutDelete: when the
// record stays mid-transition for the whole settle budget, apply fails naming
// the phase and `up --fresh`, and touches nothing.
func TestStartManagerTransitionalPhaseNeverSettlesFailsWithoutDelete(t *testing.T) {
	for _, phase := range []string{"created", "provisioning", "cloning", "starting", "stopping", "resumed"} {
		t.Run(phase, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: phase, initContainerStatus: ""}
			deps := Deps{
				Scion:            scion.New(r, scion.Options{}),
				PhaseSettleRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond},
			}
			err := runApply(app, deps)
			testutil.WantErrContaining(t, err, phase, "--fresh")
			if r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 {
				t.Fatalf("delete/start/resume = %d/%d/%d, want 0/0/0 (a record that never settles must not be touched)", r.deleteCalls, r.startCalls, r.resumeCalls)
			}
			if r.listCalls < 3 {
				t.Errorf("listCalls = %d, want >= 3 (the settle budget must be spent before giving up)", r.listCalls)
			}
		})
	}
}

// TestStartManagerCreatedNeverSettlesNamesWarmUp: a manager
// stuck in phase created is the first start of a large image losing to
// scion's start timeout; the error names the cause and the manual warm-up
// with the exact userns flag scion uses.
func TestStartManagerCreatedNeverSettlesNamesWarmUp(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "created", initContainerStatus: ""}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		PhaseSettleRetry: RetryBudget{Attempts: 2, Interval: time.Millisecond},
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "user-namespace layer copy",
		"podman run --rm --userns=keep-id:uid=1000,gid=1000 --entrypoint true "+app.ManagerAgentImage(), "lever up --fresh")
}

// TestStartManagerUnknownPhaseFailsWithoutDelete: a phase lever does not know
// (a newer scion enum, or a hostile hub) is refused, with the record and its
// conversation left in place. Only `up --fresh` discards.
func TestStartManagerUnknownPhaseFailsWithoutDelete(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "definitely-not-a-phase", initContainerStatus: "stopped"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "definitely-not-a-phase", "--fresh")
	if r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 {
		t.Fatalf("delete/start/resume = %d/%d/%d, want 0/0/0 (an unrecognised phase must not be touched)", r.deleteCalls, r.startCalls, r.resumeCalls)
	}
}

// A created agent record has no agent home to reuse, so lever-agent boot
// always re-enrols after a create; with a spent /bootstrap latch
// (ErrBootstrapLatched) that Start would 403. The create helper must re-arm
// whenever no fresh material was minted in this run, and never otherwise.

// TestStartManagerCreateRearmsSpentLatchWhenNoFreshMintThisRun: a create after
// a tolerated spent latch must call RearmBootstrap exactly once, then Start.
func TestStartManagerCreateRearmsSpentLatchWhenNoFreshMintThisRun(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // initPhase "" == absent -> create path
	rearmCalls := 0
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		RearmBootstrap:       countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 1 {
		t.Errorf("RearmBootstrap calls = %d, want 1 (a create with no fresh mint this run must re-arm)", rearmCalls)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (Start must proceed once the re-arm succeeds)", r.startCalls)
	}
}

// TestStartManagerCreateSkipsRearmWhenFreshMintAlreadyHappened: a fresh mint
// this run already gives the create enrolable material, so RearmBootstrap must
// not be called.
func TestStartManagerCreateSkipsRearmWhenFreshMintAlreadyHappened(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent -> create path
	rearmCalls := 0
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{Ticket: "minted-this-run"}, nil // fresh mint, no latch
		},
		RearmBootstrap: countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 0 {
		t.Errorf("RearmBootstrap calls = %d, want 0 (a fresh mint this run already has enrolable material)", rearmCalls)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1", r.startCalls)
	}
}

// TestStartManagerFreshRearmsBeforeFreshCreate: the create that follows a
// `--fresh` delete must also re-arm before its Start.
func TestStartManagerFreshRearmsBeforeFreshCreate(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
	}
	rearmCalls := 0
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		Log:                  func(string, ...any) {},
		RearmBootstrap:       countRearm(&rearmCalls),
	}
	if err := runApplyFresh(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 1 {
		t.Errorf("RearmBootstrap calls = %d, want 1 (the create after a --fresh delete must re-arm too)", rearmCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 || r.resumeCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d resumeCalls=%d, want 1/1/0", r.deleteCalls, r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerFreshDeleteRetriesOnBrokerUnavailable: `up --fresh` is the
// way out of a manager that does not resume, so its delete must absorb the
// runtime-broker registration race, and a failed --fresh must leave the
// record for the next attempt, never create over it.
func TestStartManagerFreshDeleteRetriesOnBrokerUnavailable(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "error", initContainerStatus: "stopped",
		deleteErr:              fmt.Errorf("cannot delete agent: no runtime broker available"),
		deleteFailsThenSucceed: 2,
	}
	deps := Deps{BrokerStartRetry: fastRetry(5), Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {}}
	if err := runApplyFresh(app, deps); err != nil {
		t.Fatalf("--fresh over an error-phase record once the broker registers: %v", err)
	}
	if r.deleteCalls != 3 || r.startCalls != 1 || r.resumeCalls != 0 {
		t.Fatalf("deleteCalls=%d startCalls=%d resumeCalls=%d, want 3/1/0", r.deleteCalls, r.startCalls, r.resumeCalls)
	}

	// An attempt that removed the record and then reported the transient
	// failure: the retry finds nothing, which is the result asked for.
	app, f = newObserveFirstApp(t)
	gone := &deleteThenGoneRunner{agentLifecycleRunner: agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped"}}
	if err := runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(5), Scion: scion.New(gone, scion.Options{}), Log: func(string, ...any) {}}); err != nil {
		t.Fatalf("--fresh when the first delete succeeded but reported a transient error: %v", err)
	}
	if gone.deletes != 2 || gone.startCalls != 1 {
		t.Fatalf("deletes=%d startCalls=%d, want 2/1", gone.deletes, gone.startCalls)
	}

	app, f = newObserveFirstApp(t)
	r = &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "stopped", initContainerStatus: "stopped",
		deleteErr: fmt.Errorf("delete: agent locked"),
	}
	err := runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(3), Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {}})
	testutil.WantErrContaining(t, err, "delete failed", "agent locked")
	if r.deleteCalls != 1 || r.startCalls != 0 {
		t.Fatalf("deleteCalls=%d startCalls=%d, want 1/0 (a non-transient delete failure ends at once, and nothing is created over the record)", r.deleteCalls, r.startCalls)
	}
}

// TestStartManagerResumeRearmsWhenNoFreshMaterial: when this run minted no
// fresh bootstrap material (the broker outlived a spent latch), resume
// re-arms and stages a fresh ticket so lever-agent boot can re-enrol a
// possibly expired leaf. An existing agent home is not proof the leaf is valid.
func TestStartManagerResumeRearmsWhenNoFreshMaterial(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	rearmCalls := 0
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		RearmBootstrap:       countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 1 {
		t.Errorf("RearmBootstrap calls = %d, want 1 (resume must stage a fresh ticket so an expired leaf can re-enrol)", rearmCalls)
	}
	if r.resumeCalls != 1 || r.startCalls != 0 {
		t.Errorf("resumeCalls=%d startCalls=%d, want 1/0 (heal-then-resume, no fresh create)", r.resumeCalls, r.startCalls)
	}
}

// TestStartManagerResumeSkipsRearmWhenAlreadyMinted: after a fresh mint this
// run, resume must not re-arm (no needless second broker bounce).
func TestStartManagerResumeSkipsRearmWhenAlreadyMinted(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	rearmCalls := 0
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{Ticket: "minted-this-run"}, nil // fresh mint, latch was open
		},
		RearmBootstrap: countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 0 {
		t.Errorf("RearmBootstrap calls = %d, want 0 (fresh material already staged this run -> no second bounce)", rearmCalls)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
}

func TestStartManagerNoOpRunningNeverRearms(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "running"}
	rearmCalls := 0
	deps := Deps{
		Scion:          scion.New(r, scion.Options{}),
		RearmBootstrap: countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 0 {
		t.Errorf("RearmBootstrap calls = %d, want 0 (an already-running manager never creates, so it never re-arms)", rearmCalls)
	}
}

// TestStartManagerCreateFailsLoudlyWhenRearmFails: a create without enrolable
// bootstrap material crash-loops on 403, so a RearmBootstrap failure must fail
// the step rather than let Start run.
func TestStartManagerCreateFailsLoudlyWhenRearmFails(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent -> create path
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		RearmBootstrap: func(context.Context) error {
			return fmt.Errorf("broker restart failed: connection refused")
		},
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "bootstrap", "latch")
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (Start must never be attempted without enrolable bootstrap material)", r.startCalls)
	}
}

// TestStartManagerCreateProceedsWithoutRearmWhenNilBackCompat: with a nil
// RearmBootstrap, Start proceeds unguarded.
func TestStartManagerCreateProceedsWithoutRearmWhenNilBackCompat(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent -> create path
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (nil RearmBootstrap must not block the create path)", r.startCalls)
	}
}

// TestStartManagerObserveListErrorIsHardFailure: the hub is already up when
// start-manager runs, so a non-transient List error is real and fails the
// step.
func TestStartManagerObserveListErrorIsHardFailure(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", startErr: nil}
	fe := &firstListErrRunner{inner: r}
	deps := Deps{
		Scion: scion.New(fe, scion.Options{}),
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "observing agents")
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (must not guess an action past an observe failure)", r.startCalls)
	}
}

// firstListErrRunner fails exactly the first observe `scion list` call (not
// waitHubReady's probe) with a synthetic hub error.
type firstListErrRunner struct {
	inner    *agentLifecycleRunner
	listSeen int
}

func (r *firstListErrRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.inner.RunStdin(ctx, stdin, env, name, args...)
}

func (r *firstListErrRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && isObserveList(args) {
		r.listSeen++
		if r.listSeen == 1 {
			return proc.Result{Code: 1, Stderr: "hub: internal server error"}, fmt.Errorf("exit status 1")
		}
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *firstListErrRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// failNListsRunner fails observe `scion list` calls 2..1+failCount with a
// transient error, leaving the pre-action observe (call 1) alone so the
// failures land in waitManagerLive's poll.
type failNListsRunner struct {
	inner     *agentLifecycleRunner
	failCount int
	listSeen  int
}

func (r *failNListsRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.inner.RunStdin(ctx, stdin, env, name, args...)
}

func (r *failNListsRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && isObserveList(args) {
		r.listSeen++
		if r.listSeen > 1 && r.listSeen <= 1+r.failCount {
			return proc.Result{Code: 1, Stderr: "hub: transient blip"}, fmt.Errorf("exit status 1")
		}
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *failNListsRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestWaitManagerLiveToleratesMidPollListErrors: transient List errors during
// the liveness poll consume attempts from the budget rather than abort the
// apply.
func TestWaitManagerLiveToleratesMidPollListErrors(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "running"}
	fe := &failNListsRunner{inner: r, failCount: 2}
	deps := Deps{
		ManagerLiveRetry: fastRetry(5),
		Scion:            scion.New(fe, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("two transient List blips within the liveness poll's budget must not fail apply: %v", err)
	}
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("the no-op branch must not start/resume; start=%d resume=%d", r.startCalls, r.resumeCalls)
	}
}

func TestRunIdempotentReapply(t *testing.T) {
	tree := t.TempDir()
	// A prior apply already staged the manager's bootstrap ticket, so a spent
	// latch on this re-apply is tolerable.
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".lever", "bootstrap.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	r := &alreadyUpRunner{FakeRunner: scionOKRunner(), slug: "demo"}
	mintCalled := false
	deps := Deps{
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			mintCalled = true
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("re-apply of a fully-up instance must be a clean no-op: %v", err)
	}
	if !mintCalled {
		t.Fatal("mint must be CALLED (and tolerate the latch) — tied to the live broker, not a stale file")
	}
}

// TestRunMintBootstrapPropagatesRealError: a non-latch mint error must not be
// swallowed.
func TestRunMintBootstrapPropagatesRealError(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "demo", Backend: "orbstack", Tree: tree, Manager: config.Manager{Image: "img"}}
	r := &alreadyUpRunner{FakeRunner: scionOKRunner(), slug: "demo"}
	deps := Deps{
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: connection refused")
		},
	}
	if err := runApply(app, deps); err == nil {
		t.Fatal("a real mint error (not the latch) must propagate, not be tolerated")
	}
}

// TestRunLatchedWithoutStagedBootstrapFails: a spent latch with no staged
// ticket means a stale broker is being reused and the manager could never
// enrol, so the mint step must fail and point at `lever down`.
func TestRunLatchedWithoutStagedBootstrapFails(t *testing.T) {
	tree := t.TempDir() // nothing staged
	app := &config.App{Name: "demo", Backend: "orbstack", Tree: tree, Manager: config.Manager{Image: "img"}}
	r := &alreadyUpRunner{FakeRunner: scionOKRunner(), slug: "demo"}
	deps := Deps{
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, "lever down")
}

func TestStartManagerRetriesOnBrokerUnavailable(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &flakyStartRunner{FakeRunner: scionOKRunner(), slug: "hello", startFails: 2}
	deps := Deps{
		BrokerStartRetry: fastRetry(5),
		Scion:            scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run should succeed after the broker race resolves: %v", err)
	}
	if r.startCalls != 3 {
		t.Fatalf("start attempted %d times, want 3 (2 transient failures + 1 success)", r.startCalls)
	}
}
func TestRunDispatchesStepsInOrder(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "lever.local/lever-claude:latest"},
		Workers: []config.Worker{{Name: "worker", Dir: "workers/worker"}},
	}
	var loadImg bool
	deps := Deps{
		LoadImage: func(_ context.Context, ref string) error {
			loadImg = (ref == "lever.local/lever-claude:latest")
			return nil
		},
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !loadImg {
		t.Fatal("host step load-image not called")
	}
	j := joinedCalls(f)
	for _, want := range []string{"init --machine", "config set --global --non-interactive image_registry scionlocal", "server start", "init --non-interactive", "hub link", "-- hello"} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing scion call %q in: %q", want, j)
		}
	}
}

// serverStartOrderRunner appends "server-start" to *order on `scion server
// start`, so a test can order it against an injected Deps func.
type serverStartOrderRunner struct {
	*agentLifecycleRunner
	order *[]string
}

func (r *serverStartOrderRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" {
		hasServer, hasStart := false, false
		for _, a := range args {
			if a == "server" {
				hasServer = true
			}
			if a == "start" {
				hasStart = true
			}
		}
		if hasServer && hasStart {
			*r.order = append(*r.order, "server-start")
		}
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *serverStartOrderRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestRunHubIdentityRunsOnceAfterScionServer: hub-identity talks to the live
// hub, so it runs exactly once and after `scion server start`.
func TestRunHubIdentityRunsOnceAfterScionServer(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	var order []string
	alr := &agentLifecycleRunner{FakeRunner: f, slug: app.Name}
	sr := &serverStartOrderRunner{agentLifecycleRunner: alr, order: &order}
	ensureCalls := 0
	deps := Deps{
		Scion: scion.New(sr, scion.Options{}),
		EnsureHubIdentity: func(context.Context) error {
			ensureCalls++
			order = append(order, "hub-identity")
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ensureCalls != 1 {
		t.Fatalf("EnsureHubIdentity calls = %d, want 1", ensureCalls)
	}
	hiIdx, serverIdx := slices.Index(order, "hub-identity"), slices.Index(order, "server-start")
	if hiIdx < 0 || serverIdx < 0 {
		t.Fatalf("expected both hub-identity and server-start recorded; order=%v", order)
	}
	if !(serverIdx < hiIdx) {
		t.Fatalf("hub-identity must run after scion-server; order=%v", order)
	}
}

func TestRunHubIdentityFailureFailsTheApply(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	boom := errors.New("boom")
	err := runApply(app, Deps{
		Scion:             hubScion(f, app),
		EnsureHubIdentity: func(context.Context) error { return boom },
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "hub-identity") {
		t.Fatalf("err = %v, want the hub-identity failure", err)
	}
	if managerStarted(f) {
		t.Fatalf("the manager started after hub-identity failed; calls=%s", joinedCalls(f))
	}
}

func TestRunStartsTheHub(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawServerStart bool
	for _, c := range f.Calls {
		if c.Name == "scion" {
			hasServer, hasStart := false, false
			for _, a := range c.Args {
				if a == "server" {
					hasServer = true
				}
				if a == "start" {
					hasStart = true
				}
			}
			if hasServer && hasStart {
				sawServerStart = true
			}
		}
	}
	if !sawServerStart {
		t.Fatalf("scion-server did not start the hub; calls=%+v", f.Calls)
	}
}

func TestRunCredentialStep(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img", CredentialFile: "/x/token"},
	}
	deps := Deps{
		ReadCred: func(string) (string, error) { return "sk-ant-raw", nil },
		Scion:    hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := joinedCalls(f)
	// plaintext: scion stores the argument verbatim.
	if want := "hub env set --secret --always --non-interactive CLAUDE_CODE_OAUTH_TOKEN sk-ant-raw"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

// TestRunScionServerEmitsWebFlagsWhenRemoteEnabled: with remote access on,
// `server start` carries --enable-web but not the tailnet base_url (see the
// agent-endpoint test below).
func TestRunScionServerEmitsWebFlagsWhenRemoteEnabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := joinedCalls(f)
	if want := "server start --web-port 8080 --dev-auth=false --enable-web|"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

// TestRunScionServerEmitsSessionSecret: the hub signs session cookies with
// the host-persisted key, not a per-boot random one.
func TestRunScionServerEmitsSessionSecret(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion:            hubScion(f, app),
		HubSessionSecret: "sessionsecrethex",
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := joinedCalls(f)
	if want := "server start --web-port 8080 --dev-auth=false --session-secret=sessionsecrethex|"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

// TestRunScionServerPointsTheHubAtStagedAssets: a remote-enabled instance on
// a `version:` pin has no embedded SPA, so the hub must be pointed at the
// staged layout.WebAssetsDir.
func TestRunScionServerPointsTheHubAtStagedAssets(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Scion = config.ScionConfig{Version: "e82a2a08"}
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := joinedCalls(f)
	if want := "--enable-web --web-assets-dir=" + layout.WebAssetsDir + "|"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

// Binary mode builds no assets, so the flag must stay off: a non-empty value
// would make scion serve an empty directory instead of the binary's own SPA.
func TestRunScionServerOmitsAssetsDirForBinaryMode(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Scion = config.ScionConfig{Binary: "/host/scion-linux-arm64"}
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "--web-assets-dir") {
			t.Fatalf("binary mode must not claim staged assets: %v", c.Args)
		}
	}
}

// containerBridgeHost is the only host name that reaches the machine's
// loopback from an agent's netns: podman answers it with 169.254.1.2, and
// lever's pasta --map-host-loopback drop-in maps that to the machine. A hub
// endpoint naming anything else is unreachable from the jail.
const containerBridgeHost = "host.containers.internal"

// agentHubEndpoint models the SCION_HUB_ENDPOINT scion injects into every
// agent container, given lever's `scion server start` argv, so tests assert
// what agents see rather than the flags lever passed.
//
// It holds only in scion workstation mode (no --hosted, no --enable-hub),
// where scion forces enableHub on and so consults --base-url. If lever ever
// passes --hosted, re-derive this model. Then, in scion:
//
//  1. resolveHubEndpoint: --base-url wins; without it the server falls back
//     to http://localhost:<web-port>. Lever leaves the other precedence arms
//     unset and repairs the project-recorded endpoint to loopback
//     (Deps.RepairScionHubEndpoint).
//  2. applyContainerBridgeOverride: a loopback endpoint is rewritten onto the
//     container bridge host, keeping its port; any other endpoint reaches the
//     agent verbatim.
func agentHubEndpoint(t *testing.T, serverStart []string) string {
	t.Helper()
	flagVal := func(name string) string {
		for i, a := range serverStart {
			if a == name && i+1 < len(serverStart) {
				return serverStart[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
		return ""
	}
	endpoint := flagVal("--base-url")
	if endpoint == "" {
		port := flagVal("--web-port")
		if port == "" {
			port = "8080" // scion's default web port
		}
		endpoint = "http://localhost:" + port
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("hub endpoint %q does not parse: %v", endpoint, err)
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		u.Host = net.JoinHostPort(containerBridgeHost, u.Port())
	}
	return u.String()
}

// TestRunScionServerKeepsAgentHubEndpointJailReachable: a tailnet --base-url
// would become every agent's SCION_HUB_ENDPOINT, a name the jail cannot
// resolve and egress drops, breaking status, notifications and agent token
// refresh. Turning remote access on must leave agents the same
// container-bridge endpoint a headless instance gives them. It asserts the
// agent-facing outcome, not the argv.
func TestRunScionServerKeepsAgentHubEndpointJailReachable(t *testing.T) {
	serverStartArgs := func(t *testing.T, app *config.App) []string {
		t.Helper()
		f := scionOKRunner()
		deps := Deps{
			Scion: hubScion(f, app),
		}
		if err := runApply(app, deps); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, c := range f.Calls {
			if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" && c.Args[1] == "start" {
				return c.Args
			}
		}
		t.Fatal("no `scion server start` call")
		return nil
	}

	newApp := func(t *testing.T, remote config.Remote) *config.App {
		return &config.App{
			Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"},
			Remote:  remote,
		}
	}

	headless := agentHubEndpoint(t, serverStartArgs(t, newApp(t, config.Remote{})))
	if got, want := headless, "http://"+containerBridgeHost+":8080"; got != want {
		t.Fatalf("headless agent hub endpoint = %q, want %q", got, want)
	}

	remote := agentHubEndpoint(t, serverStartArgs(t, newApp(t, config.Remote{
		Enabled: true, BaseURL: "https://mac.tail.ts.net",
	})))
	if remote != headless {
		t.Errorf("remote access changed the agents' hub endpoint: got %q, want %q (the jail resolves only %s; a tailnet host reaches nothing)",
			remote, headless, containerBridgeHost)
	}
}

// TestRunScionServerOmitsWebFlagsWhenRemoteDisabled: a headless hub must not
// serve the SPA, so `server start` gains neither --enable-web nor --base-url.
func TestRunScionServerOmitsWebFlagsWhenRemoteDisabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name != "scion" {
			continue
		}
		joined := strings.Join(c.Args, " ")
		if strings.HasPrefix(joined, "server start") && (strings.Contains(joined, "--enable-web") || strings.Contains(joined, "--base-url")) {
			t.Fatalf("server start must not carry web flags when remote is disabled: %q", joined)
		}
	}
}

func TestRunRemoteProxyStepInvokesStartWhenEnabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	var starts int
	deps := Deps{
		Scion:            hubScion(f, app),
		StartRemoteProxy: func(context.Context) error { starts++; return nil },
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if starts != 1 {
		t.Fatalf("StartRemoteProxy calls = %d, want 1", starts)
	}
}

// TestRunRemoteProxyStepOrderedAfterScionServer: the proxy needs the hub up,
// so it must start after `scion server start` runs.
func TestRunRemoteProxyStepOrderedAfterScionServer(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	var order []string
	alr := &agentLifecycleRunner{FakeRunner: f, slug: app.Name}
	sr := &serverStartOrderRunner{agentLifecycleRunner: alr, order: &order}
	deps := Deps{
		Scion: scion.New(sr, scion.Options{}),
		StartRemoteProxy: func(context.Context) error {
			order = append(order, "remote-proxy")
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ssIdx, rpIdx := -1, -1
	for i, v := range order {
		if v == "server-start" && ssIdx < 0 {
			ssIdx = i
		}
		if v == "remote-proxy" && rpIdx < 0 {
			rpIdx = i
		}
	}
	if ssIdx < 0 || rpIdx < 0 {
		t.Fatalf("expected both server-start and remote-proxy recorded; order=%v", order)
	}
	if !(ssIdx < rpIdx) {
		t.Fatalf("remote-proxy must run after scion-server; order=%v", order)
	}
}

// TestRunRemoteProxyStepSkipsCleanlyWhenNil: a nil StartRemoteProxy must not
// error even though the step is in the plan.
func TestRunRemoteProxyStepSkipsCleanlyWhenNil(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run must not fail when StartRemoteProxy is nil: %v", err)
	}
}

// TestRunConvergesRemoteProxyOffWhenDisabled: Plan omits the remote-proxy
// step when remote is disabled, so Run itself must call StopRemoteProxy to
// stop a proxy left by an earlier remote-enabled apply.
func TestRunConvergesRemoteProxyOffWhenDisabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	var stops int
	deps := Deps{
		Scion:           hubScion(f, app),
		StopRemoteProxy: func(context.Context) error { stops++; return nil },
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stops != 1 {
		t.Fatalf("StopRemoteProxy calls = %d, want 1", stops)
	}
}

// The fence runs before every step, so a removed login loses the proxy even
// when an early step fails; its failure stops the apply before any step.
func TestRunFencesRemoteLoginsFirst(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	var order []string
	deps := Deps{
		Scion:             hubScion(f, app),
		FenceRemoteLogins: func(context.Context) error { order = append(order, "fence"); return nil },
		EnsureHubIdentity: func(context.Context) error { order = append(order, "hub-identity"); return errors.New("wedged") },
	}
	if err := runApply(app, deps); err == nil {
		t.Fatal("want the hub-identity failure")
	}
	if len(order) != 2 || order[0] != "fence" {
		t.Fatalf("order %v: the fence must run before every step", order)
	}
	ran := false
	deps = Deps{
		Scion:             hubScion(f, app),
		FenceRemoteLogins: func(context.Context) error { return errors.New("disk full") },
		EnsureHubIdentity: func(context.Context) error { ran = true; return nil },
	}
	if err := runApply(app, deps); err == nil || !strings.Contains(err.Error(), "disk full") || ran {
		t.Fatalf("err %v, hub-identity ran %v", err, ran)
	}
}

func TestRunDoesNotStopRemoteProxyWhenEnabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	var stops int
	deps := Deps{
		Scion:            hubScion(f, app),
		StartRemoteProxy: func(context.Context) error { return nil },
		StopRemoteProxy:  func(context.Context) error { stops++; return nil },
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stops != 0 {
		t.Fatalf("StopRemoteProxy calls = %d, want 0 (remote is enabled)", stops)
	}
}

func TestRunConvergeOffSkipsCleanlyWhenNil(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion: hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run must not fail when StopRemoteProxy is nil: %v", err)
	}
}

func TestStartManagerPassesPrompt(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace", "workers", "worker"), 0o755)
	// prompt lives at the instance ROOT (host-only), NOT under the mounted tree.
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte("Dispatch the worker to create HELLO."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  prompt_file: manager.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawPrompt bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "-- hello") && strings.Contains(j, "Dispatch the worker to create HELLO.") {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("manager prompt not passed to start; calls=%+v", f.Calls)
	}
}

func TestStartManagerSetsLLMAuthEnvForAPIKey(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	keyPath := filepath.Join(dir, "api.key")
	if err := os.WriteFile(keyPath, []byte("sk-ant-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	body := "name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: api-key\n  api_key_file: " + keyPath + "\nmanager:\n  image: img\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawEnvSet, sawPlaceholder bool
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		// --always on both: without it scion stores the value as_needed and
		// never projects it into the container, so the harness auth gate fails.
		if argv == "hub env set --project --always --non-interactive LEVER_LLM_AUTH=api-key" {
			sawEnvSet = true
		}
		// SecretSet base64-encodes the value, so match on the verb + key, not value.
		if strings.HasPrefix(argv, "hub env set --secret --always --non-interactive ANTHROPIC_API_KEY ") {
			sawPlaceholder = true
		}
		if c.Name == "scion" && strings.Contains(argv, " start ") {
			startArgv = argv
		}
	}
	if !sawEnvSet {
		t.Fatalf("api-key manager: expected LEVER_LLM_AUTH env set; calls=%+v", f.Calls)
	}
	if !sawPlaceholder {
		t.Fatalf("api-key manager: expected placeholder ANTHROPIC_API_KEY secret set; calls=%+v", f.Calls)
	}
	// api-key manager must start with --harness-auth api-key (not oauth-token).
	if !strings.Contains(startArgv, "--harness-auth api-key") || strings.Contains(startArgv, "oauth-token") {
		t.Fatalf("api-key manager start must use --harness-auth api-key (not oauth-token); argv=%q", startArgv)
	}
}

func TestStartManagerNoLLMAuthEnvForSubscription(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if strings.Contains(argv, "LEVER_LLM_AUTH") {
			t.Fatalf("subscription manager must not set LEVER_LLM_AUTH; calls=%+v", f.Calls)
		}
		if c.Name == "scion" && strings.Contains(argv, " start ") {
			startArgv = argv
		}
	}
	// subscription manager keeps oauth-token auth (scion projects the OAuth token).
	if !strings.Contains(startArgv, "--harness-auth oauth-token") || strings.Contains(startArgv, "--no-auth") {
		t.Fatalf("subscription manager start must use oauth-token (not --no-auth); argv=%q", startArgv)
	}
}

func TestJailPathTranslation(t *testing.T) {
	cases := []struct {
		host, tree, mount, want string
	}{
		{"/tmp/foo", "/tmp/foo", "/lever", "/lever"},
		{"/tmp/foo/workers/worker", "/tmp/foo", "/lever", "/lever/workers/worker"},
		{"/tmp/foo", "/tmp/foo", "", "/tmp/foo"},
		{"/elsewhere", "/tmp/foo", "/lever", "/elsewhere"},
	}
	for _, c := range cases {
		if got := JailPath(c.host, c.tree, c.mount); got != c.want {
			t.Errorf("JailPath(%q, %q, %q) = %q, want %q", c.host, c.tree, c.mount, got, c.want)
		}
	}
}

func TestRegisterRemovesStaleMarkerBeforeInit(t *testing.T) {
	// A stale marker must be gone before `scion init` runs, or init resolves
	// it and skips creating the project.
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	initSeen := func() bool { // the project init, not the earlier `init --machine`
		for _, c := range f.Calls {
			if c.Name == "scion" && len(c.Args) > 0 && c.Args[0] == "init" && !slices.Contains(c.Args, "--machine") {
				return true
			}
		}
		return false
	}
	removed := 0
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveJailFile: func(_ context.Context, p string) error {
			removed++
			if initSeen() {
				t.Errorf("RemoveJailFile(%s) ran AFTER scion init", p)
			}
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if removed != 1 || !initSeen() {
		t.Errorf("removed=%d initSeen=%v, want 1/true", removed, initSeen())
	}
}

// TestRegisterRemovesMarkerThroughJailWhenProvided: with RemoveJailFile set,
// the stale marker is removed through it (jail path), not by the host-side
// fallback. The fake leaves the host file in place to prove that.
func TestRegisterRemovesMarkerThroughJailWhenProvided(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	app := helloApp(tree)
	var calls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveJailFile: func(_ context.Context, jailPath string) error {
			calls = append(calls, jailPath)
			return nil // deliberately does NOT touch the host file
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(calls) != 1 || calls[0] != "/lever/.scion" {
		t.Fatalf("RemoveJailFile calls = %+v, want exactly one call with \"/lever/.scion\"", calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("host marker should be untouched when RemoveJailFile handles removal, stat err=%v", err)
	}
}

// TestRegisterRemovesStaleScionProjectConfigsBeforeInit: RemoveScionProjectConfigs
// runs with the jail workspace path before `scion init`, or every apply adds
// another ~/.scion/project-configs/<uuid>. Workers get no registration of
// their own.
func TestRegisterRemovesStaleScionProjectConfigsBeforeInit(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	app.Workers = []config.Worker{{Name: "worker", Dir: "workers/worker"}}
	var removeCalls []string
	var initCalls []string
	// FakeRunner records calls synchronously, so zero init calls at removal
	// time proves the removal ran first.
	var initCountAtRemove []int
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveScionProjectConfigs: func(_ context.Context, jailWorkspacePath string) error {
			removeCalls = append(removeCalls, jailWorkspacePath)
			n := 0
			for _, c := range f.Calls {
				if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
					n++
				}
			}
			initCountAtRemove = append(initCountAtRemove, n)
			return nil
		},
	}

	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(removeCalls) != 1 {
		t.Fatalf("RemoveScionProjectConfigs calls = %+v, want exactly 1 (the instance project only)", removeCalls)
	}
	if removeCalls[0] != "/lever" {
		t.Errorf("remove call path = %q, want /lever", removeCalls[0])
	}
	wantCounts := []int{0}
	for i, n := range initCountAtRemove {
		if n != wantCounts[i] {
			t.Errorf("remove call %d (%s): %d init call(s) had already fired, want %d — it must run before init", i, removeCalls[i], n, wantCounts[i])
		}
	}

	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			initCalls = append(initCalls, c.Dir)
		}
	}
	if len(initCalls) != 1 {
		t.Fatalf("init calls = %+v, want exactly 1", initCalls)
	}
}

func TestRegisterToleratesNilRemoveScionProjectConfigs(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawScionCall(f, "init --non-interactive") {
		t.Fatal("scion init should still run when RemoveScionProjectConfigs is nil")
	}
}

// TestRegisterSkipsDestructivePathWhenAlreadyRegistered: a sound registration
// skips the clean+init path entirely (no marker removal, no
// RemoveScionProjectConfigs, no init or hub link), so a suspended manager's
// project linkage survives a re-apply.
func TestRegisterSkipsDestructivePathWhenAlreadyRegistered(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	app := helloApp(tree)
	app.Workers = []config.Worker{{Name: "worker", Dir: "workers/worker"}}
	var removeJailCalls, removeConfigCalls, registeredCalls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveJailFile: func(_ context.Context, jailPath string) error {
			removeJailCalls = append(removeJailCalls, jailPath)
			return nil
		},
		RemoveScionProjectConfigs: func(_ context.Context, wp string) error {
			removeConfigCalls = append(removeConfigCalls, wp)
			return nil
		},
		ScionProjectRegistered: func(_ context.Context, wp string) (bool, error) {
			registeredCalls = append(registeredCalls, wp)
			return true, nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(registeredCalls) != 1 || registeredCalls[0] != "/lever" {
		t.Fatalf("ScionProjectRegistered calls = %+v, want [/lever]", registeredCalls)
	}
	if len(removeJailCalls) != 0 {
		t.Errorf("RemoveJailFile should not be called when already registered; got %+v", removeJailCalls)
	}
	if len(removeConfigCalls) != 0 {
		t.Errorf("RemoveScionProjectConfigs should not be called when already registered; got %+v", removeConfigCalls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("real marker must survive when already registered, stat err=%v", err)
	}
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") || strings.Contains(j, "hub link") {
			t.Errorf("scion init/hub-link must not run when already registered; call=%+v", c)
		}
	}
}

// TestRegisterStripsSharedDirsOnBothPaths: scion mounts the default
// `scratchpad` shared dir read-write into every agent of a project, so
// register-project must strip it on both the already-registered and the
// re-init path, or the manager and workers share a writable directory.
func TestRegisterStripsSharedDirsOnBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
	}{
		{"already registered", true},
		{"fresh registration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			f := scionOKRunner()
			app := helloApp(tree)
			var stripCalls []string
			deps := Deps{
				JailMount: "/lever",
				Scion:     hubScion(f, app),
				ScionProjectRegistered: func(context.Context, string) (bool, error) {
					return tc.registered, nil
				},
				StripProjectSharedDirs: func(_ context.Context, project string) error {
					stripCalls = append(stripCalls, project)
					return nil
				},
			}
			if err := runApply(app, deps); err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The hub knows the project by the workspace basename.
			if len(stripCalls) != 1 || stripCalls[0] != "lever" {
				t.Fatalf("StripProjectSharedDirs calls = %+v, want [lever]", stripCalls)
			}
		})
	}
}

// TestRegisterFailsWhenSharedDirStripFails: a strip failure (say a 403 from a
// PAT without project:update) must abort apply, not leave the manager and
// workers silently sharing a writable directory.
func TestRegisterFailsWhenSharedDirStripFails(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		StripProjectSharedDirs: func(context.Context, string) error {
			return errForbidden
		},
	}
	testutil.WantErrIs(t, runApply(app, deps), errForbidden)
}

// TestRegisterRunsDestructivePathWhenNotRegistered: when ScionProjectRegistered
// reports false, the marker removal, RemoveScionProjectConfigs, init and hub
// link all run.
func TestRegisterRunsDestructivePathWhenNotRegistered(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	var removeJailCalls, removeConfigCalls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveJailFile: func(_ context.Context, jailPath string) error {
			removeJailCalls = append(removeJailCalls, jailPath)
			return nil
		},
		RemoveScionProjectConfigs: func(_ context.Context, wp string) error {
			removeConfigCalls = append(removeConfigCalls, wp)
			return nil
		},
		ScionProjectRegistered: func(_ context.Context, wp string) (bool, error) {
			return false, nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(removeJailCalls) != 1 || removeJailCalls[0] != "/lever/.scion" {
		t.Fatalf("RemoveJailFile calls = %+v, want exactly one call with \"/lever/.scion\"", removeJailCalls)
	}
	if len(removeConfigCalls) != 1 || removeConfigCalls[0] != "/lever" {
		t.Fatalf("RemoveScionProjectConfigs calls = %+v, want exactly one call with \"/lever\"", removeConfigCalls)
	}
	sawInit, sawHubLink := sawScionCall(f, "init --non-interactive"), sawScionCall(f, "hub link")
	if !sawInit || !sawHubLink {
		t.Fatalf("scion init and hub link must both run when not registered; init=%v hublink=%v", sawInit, sawHubLink)
	}
}

// TestRegisterFallsThroughToDestructivePathOnObserveError: an error from
// ScionProjectRegistered is not fatal; it takes the destructive path like a
// false result.
func TestRegisterFallsThroughToDestructivePathOnObserveError(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	var removeConfigCalls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		RemoveScionProjectConfigs: func(_ context.Context, wp string) error {
			removeConfigCalls = append(removeConfigCalls, wp)
			return nil
		},
		// ok=true alongside an error: the error, not ok, must govern.
		ScionProjectRegistered: func(_ context.Context, wp string) (bool, error) {
			return true, fmt.Errorf("boom: guest unreachable")
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v (an observe error must fail OPEN to the destructive path, not fail the apply)", err)
	}
	if len(removeConfigCalls) != 1 {
		t.Fatalf("observe error must fall through to the destructive path; RemoveScionProjectConfigs calls = %+v", removeConfigCalls)
	}
	if !sawScionCall(f, "init --non-interactive") {
		t.Fatal("scion init should still run when the observe read errors")
	}
}

func TestRegisterToleratesNilScionProjectRegistered(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawScionCall(f, "init --non-interactive") {
		t.Fatal("scion init should still run when ScionProjectRegistered is nil")
	}
}

// TestRegisterUsesJailPaths: only the instance tree is registered, by its
// jail path; workers get no registration of their own.
func TestRegisterUsesJailPaths(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	app.Workers = []config.Worker{{Name: "worker", Dir: "workers/worker"}}
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var managerInit bool
	var initCalls int
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			initCalls++
			if c.Dir != "/lever" {
				t.Errorf("init call used dir %q, want exactly /lever", c.Dir)
			}
			managerInit = true
		}
		if strings.Contains(j, "hub link") && c.Dir != "/lever" {
			t.Errorf("hub link call used dir %q, want /lever", c.Dir)
		}
	}
	if !managerInit {
		t.Errorf("project init not run with dir /lever")
	}
	if initCalls != 1 {
		t.Errorf("init calls = %d, want exactly 1 (single register-project step)", initCalls)
	}
}

// TestSingleProjectRegisterRunsOnceAcrossTwoWorkers: two workers are needed
// to tell one registration per instance from one per worker. Exactly one
// register-project step runs, against the instance jail path "/lever".
func TestSingleProjectRegisterRunsOnceAcrossTwoWorkers(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Workers: []config.Worker{
			{Name: "a", Dir: "workers/a"},
			{Name: "b", Dir: "workers/b"},
		},
	}

	var regSteps []Step
	for _, s := range Plan(app, PlanOpts{}) {
		if s.Kind == "register-project" {
			regSteps = append(regSteps, s)
		}
	}
	if len(regSteps) != 1 {
		t.Fatalf("register-project steps = %+v, want exactly 1 (2 workers configured)", regSteps)
	}
	if regSteps[0].Target != tree {
		t.Fatalf("register-project target = %q, want the instance tree %q", regSteps[0].Target, tree)
	}

	var registeredCalls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		ScionProjectRegistered: func(_ context.Context, wp string) (bool, error) {
			registeredCalls = append(registeredCalls, wp)
			return false, nil // force the destructive path
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(registeredCalls) != 1 || registeredCalls[0] != "/lever" {
		t.Fatalf("ScionProjectRegistered calls = %+v, want exactly [\"/lever\"] (once, not per worker)", registeredCalls)
	}

	var initCalls, hubLinkCalls []string
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			initCalls = append(initCalls, c.Dir)
		}
		if strings.Contains(j, "hub link") {
			hubLinkCalls = append(hubLinkCalls, c.Dir)
		}
	}
	if len(initCalls) != 1 || initCalls[0] != "/lever" {
		t.Fatalf("scion init calls = %+v, want exactly one at /lever (the OLD per-worker model would show 3 for 2 workers + manager)", initCalls)
	}
	if len(hubLinkCalls) != 1 || hubLinkCalls[0] != "/lever" {
		t.Fatalf("scion hub link calls = %+v, want exactly one at /lever", hubLinkCalls)
	}
}

func TestStartUsesJailPath(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawJailG, sawWorkspace bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "-- hello") {
			if strings.Contains(j, "-g "+tree) {
				t.Errorf("start call used host path: %q", j)
			}
			if strings.Contains(j, "-g /lever") {
				sawJailG = true
			}
			// In-place live mount: the manager must mount the in-jail tree as
			// /workspace, else scion mounts a managed copy of the config dir.
			if strings.Contains(j, "--workspace /lever") {
				sawWorkspace = true
			}
		}
	}
	if !sawJailG {
		t.Fatalf("start call did not use -g /lever; calls=%+v", f.Calls)
	}
	if !sawWorkspace {
		t.Fatalf("start call did not pass --workspace /lever (in-place mount); calls=%+v", f.Calls)
	}
}

func TestDefaultReadCredRejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tok")
	if err := os.WriteFile(good, []byte("sk-ant-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := defaultReadCred(good); err != nil || v != "sk-ant-xyz" {
		t.Fatalf("0600 cred: got %q err %v", v, err)
	}
	bad := filepath.Join(dir, "open")
	if err := os.WriteFile(bad, []byte("sk-ant-xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultReadCred(bad); err == nil {
		t.Fatal("world-readable credential should be rejected")
	}
}

func loadImageStep(d Deps) error {
	return (&run{app: &config.App{}, d: fillDeps(d)}).step(context.Background(), Step{Kind: "load-image", Target: "img"})
}

// TestRunStepUnknownKind: an unknown StepKind is a hard error naming the
// kind, never a silent no-op.
func TestRunStepUnknownKind(t *testing.T) {
	err := (&run{app: &config.App{}}).step(context.Background(), Step{Kind: "no-such-kind"})
	testutil.WantErrContaining(t, err, "no-such-kind")
}

// TestLoadImageStepSkipsWhenAlreadyLoaded: when the jail already holds the
// exact image, neither re-import nor prune runs.
func TestLoadImageStepSkipsWhenAlreadyLoaded(t *testing.T) {
	var loads, prunes int
	d := Deps{
		LoadImage:   func(context.Context, string) error { loads++; return nil },
		ImageLoaded: func(context.Context, string) bool { return true },
		PruneImages: func(context.Context) error { prunes++; return nil },
	}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if loads != 0 {
		t.Errorf("LoadImage calls = %d, want 0 (image already loaded)", loads)
	}
	if prunes != 0 {
		t.Errorf("PruneImages calls = %d, want 0 (nothing was loaded)", prunes)
	}
}

// TestLoadImageStepLoadsAndPrunesWhenAbsent: a not-loaded (or rebuilt-ID)
// image is loaded, then the superseded dangling image is pruned.
func TestLoadImageStepLoadsAndPrunesWhenAbsent(t *testing.T) {
	var loads, prunes int
	d := Deps{
		LoadImage:   func(context.Context, string) error { loads++; return nil },
		ImageLoaded: func(context.Context, string) bool { return false },
		PruneImages: func(context.Context) error { prunes++; return nil },
	}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if loads != 1 {
		t.Errorf("LoadImage calls = %d, want 1", loads)
	}
	if prunes != 1 {
		t.Errorf("PruneImages calls = %d, want 1 (prune after load)", prunes)
	}
}

func TestLoadImageStepNilGuardLoads(t *testing.T) {
	var loads int
	d := Deps{LoadImage: func(context.Context, string) error { loads++; return nil }}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: %v", err)
	}
	if loads != 1 {
		t.Errorf("LoadImage calls = %d, want 1 (no guard ⇒ always load)", loads)
	}
}

// TestLoadImageStepLoadErrorIsFatal: a real load failure propagates and the
// prune does not run.
func TestLoadImageStepLoadErrorIsFatal(t *testing.T) {
	var prunes int
	d := Deps{
		LoadImage:   func(context.Context, string) error { return errBoom },
		ImageLoaded: func(context.Context, string) bool { return false },
		PruneImages: func(context.Context) error { prunes++; return nil },
	}
	testutil.WantErrIs(t, loadImageStep(d), errBoom)
	if prunes != 0 {
		t.Errorf("PruneImages calls = %d, want 0 (no prune after a failed load)", prunes)
	}
}

// TestLoadImageStepPruneErrorIsNonFatal: the image loaded, so a prune failure
// is logged, not returned.
func TestLoadImageStepPruneErrorIsNonFatal(t *testing.T) {
	d := Deps{
		ImageLoaded: func(context.Context, string) bool { return false },
		PruneImages: func(context.Context) error { return fmt.Errorf("prune boom") },
	}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: a prune failure must be non-fatal, got %v", err)
	}
}

// TestLoadImageStepWarmsEveryImage: the warm-up runs after a
// load and on an image that was already loaded, so an apply that loaded the
// image and then died before its warm-up finished is completed by the next.
func TestLoadImageStepWarmsEveryImage(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		var warmed []string
		d := Deps{
			ImageLoaded: func(context.Context, string) bool { return loaded },
			WarmImage: func(ctx context.Context, ref string) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Errorf("loaded=%v: the warm-up must be bounded (no deadline on its context)", loaded)
				}
				warmed = append(warmed, ref)
				return nil
			},
		}
		if err := loadImageStep(d); err != nil {
			t.Fatalf("loaded=%v: runStep: %v", loaded, err)
		}
		if !slices.Equal(warmed, []string{"img"}) {
			t.Errorf("loaded=%v: warmed %v, want [img]", loaded, warmed)
		}
	}
}

// TestLoadImageStepWarmErrorIsNonFatal: a failed warm-up warns and the step
// still succeeds.
func TestLoadImageStepWarmErrorIsNonFatal(t *testing.T) {
	var logs []string
	d := Deps{
		WarmImage: func(context.Context, string) error { return errBoom },
		Log:       func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: a warm-up failure must be non-fatal, got %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "warning: warming img failed") || !strings.Contains(logs[0], "boom") {
		t.Errorf("logs = %q, want one warning naming the image and the cause", logs)
	}
}

// TestLoadImageStepWarmNotice: a warm-up that runs past the notice delay says
// what it waits for; a fast one (an image already warm) prints nothing.
func TestLoadImageStepWarmNotice(t *testing.T) {
	old := imageWarmNoticeAfter
	imageWarmNoticeAfter = 10 * time.Millisecond
	t.Cleanup(func() { imageWarmNoticeAfter = old })
	for _, tc := range []struct {
		name  string
		delay time.Duration
		want  int
	}{{"fast", 0, 0}, {"slow", 100 * time.Millisecond, 1}} {
		var logs []string
		d := Deps{
			WarmImage: func(context.Context, string) error { time.Sleep(tc.delay); return nil },
			Log:       func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		}
		if err := loadImageStep(d); err != nil {
			t.Fatalf("%s: runStep: %v", tc.name, err)
		}
		if len(logs) != tc.want {
			t.Fatalf("%s: logs = %q, want %d line(s)", tc.name, logs, tc.want)
		}
		if tc.want == 1 && !strings.Contains(logs[0], "warming the user-namespace layer copy of img") {
			t.Errorf("%s: notice = %q", tc.name, logs[0])
		}
	}
}

// errPreRoleRefusal is what VerifyAgentRole returns for a record created by a
// scion without agent roles: it stores no role, and a roles-aware scion reads
// that as `full`.
var errPreRoleRefusal = fmt.Errorf("agent %q has no stored role", "hello")

var (
	errForbidden = errors.New("403 Forbidden")
	errBoom      = errors.New("boom")
)

// TestStartManagerRefusesPreRoleRecordOnResume: the guard must stop the apply
// before the resume and must not fall into a delete+create, which would
// destroy the conversation.
func TestStartManagerRefusesPreRoleRecordOnResume(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps := Deps{
		Scion:           scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error { return errPreRoleRefusal },
	}
	err := runApply(app, deps)
	testutil.WantErrIs(t, err, errPreRoleRefusal)
	if r.resumeCalls != 0 {
		t.Errorf("resumeCalls = %d, want 0 (the guard runs BEFORE the resume)", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (refusing must never discard the session)", r.deleteCalls, r.startCalls)
	}
}

// A record lever would refuse to keep gets no enrolment ticket staged in
// the tree it reads: the mint step refuses before it mints.
func TestMintRefusesPreRoleRecordBeforeStagingATicket(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		app, f := newObserveFirstApp(t)
		r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 6 seconds"}
		mints := 0
		deps := Deps{
			Scion:           scion.New(r, scion.Options{}),
			VerifyAgentRole: func(context.Context, string, string) error { return errPreRoleRefusal },
			MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
				mints++
				return BootstrapMaterial{Ticket: "minted-this-run"}, nil
			},
		}
		run := runApply
		if fresh {
			run = runApplyFresh
		}
		err := run(app, deps)
		_, statErr := os.Stat(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
		if fresh {
			// --fresh discards the record, so its role does not stop the mint.
			if mints != 1 || err != nil && strings.Contains(err.Error(), "mint-manager-bootstrap") {
				t.Errorf("fresh: mints = %d, err = %v; want the mint", mints, err)
			}
			continue
		}
		testutil.WantErrIs(t, err, errPreRoleRefusal)
		if !strings.Contains(err.Error(), "mint-manager-bootstrap") || mints != 0 || statErr == nil {
			t.Fatalf("err = %v, mints = %d, staged = %v; want the refusal before any mint", err, mints, statErr == nil)
		}
	}
}

// TestStartManagerRefusesPreRoleRecordWhenRunning: the no-op branch is not
// exempt. A running agent refreshes its own token and scion re-derives scopes
// from the stored role on every refresh, so an unrolled running record is
// promoted just the same.
func TestStartManagerRefusesPreRoleRecordWhenRunning(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 6 seconds"}
	deps := Deps{
		Scion:           scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error { return errPreRoleRefusal },
	}
	if err := runApply(app, deps); err == nil {
		t.Fatal("a running record with no stored role must fail the bring-up too")
	}
}

// TestStartManagerVerifyAgentRoleSkippedWhenRecordAbsent: a first create
// stamps `--role baseline` itself, so start-manager has nothing to refuse.
// The mint step asks before it stages a ticket; the guard answers nil for an
// absent record.
func TestStartManagerVerifyAgentRoleSkippedWhenRecordAbsent(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	var steps []string
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error {
			steps = append(steps, fmt.Sprintf("starts=%d", r.startCalls))
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(steps) != 1 || steps[0] != "starts=0" {
		t.Errorf("VerifyAgentRole calls = %q, want only the mint step's, before any start", steps)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1", r.startCalls)
	}
}

// TestStartManagerVerifyAgentRolePassesThrough: a record that stores a role is
// left alone.
func TestStartManagerVerifyAgentRolePassesThrough(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps := Deps{
		Scion:           scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error { return nil },
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
}

// TestRegisterRepairsHubEndpointOnTheSkipPath: a project linked against a
// hub on another port keeps that stale endpoint on the skip path, and bare
// scion verbs such as `lever attach` then dial it.
func TestRegisterRepairsHubEndpointOnTheSkipPath(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".scion"), []byte("project-id: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	app := helloApp(tree)
	var repaired []string
	deps := Deps{
		JailMount:              "/lever",
		Scion:                  hubScion(f, app),
		ScionProjectRegistered: func(context.Context, string) (bool, error) { return true, nil },
		RepairScionHubEndpoint: func(_ context.Context, wp string) error {
			repaired = append(repaired, wp)
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != "/lever" {
		t.Fatalf("RepairScionHubEndpoint calls = %+v, want [/lever]", repaired)
	}
}

// The provider link runs on the register paths, for the tree's jail path,
// and its failure stops the bring-up before any agent starts.
func TestRegisterLinksAProviderBeforeSettling(t *testing.T) {
	for _, fail := range []bool{false, true} {
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, ".scion"), []byte("project-id: real\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := scionOKRunner()
		app := helloApp(tree)
		stripped := 0
		var asked []string
		alr := &agentLifecycleRunner{FakeRunner: f, slug: app.Name}
		deps := Deps{
			JailMount:              "/lever",
			Scion:                  scion.New(alr, scion.Options{HubEndpoint: testHubEndpoint}),
			ScionProjectRegistered: func(context.Context, string) (bool, error) { return true, nil },
			EnsureProjectProvider: func(_ context.Context, jp string) error {
				asked = append(asked, jp)
				if fail {
					return errors.New("no runtime broker")
				}
				return nil
			},
			StripProjectSharedDirs: func(context.Context, string) error { stripped++; return nil },
		}
		err := runApply(app, deps)
		if (err != nil) != fail {
			t.Fatalf("fail=%v: Run = %v", fail, err)
		}
		if len(asked) != 1 || asked[0] != "/lever" {
			t.Fatalf("fail=%v: EnsureProjectProvider asked for %v, want [/lever]", fail, asked)
		}
		if fail && (stripped != 0 || alr.startCalls+alr.resumeCalls != 0) {
			t.Fatalf("after a failed provider link: strip=%d start=%d resume=%d", stripped, alr.startCalls, alr.resumeCalls)
		}
	}
}

// A repair that cannot run must fail the bring-up rather than leave the project
// pointing at a hub that no longer exists.
func TestRegisterFailsWhenHubEndpointRepairFails(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".scion"), []byte("project-id: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount:              "/lever",
		Scion:                  hubScion(f, app),
		ScionProjectRegistered: func(context.Context, string) (bool, error) { return true, nil },
		RepairScionHubEndpoint: func(context.Context, string) error {
			return fmt.Errorf("guest unreachable")
		},
	}
	if err := runApply(app, deps); err == nil {
		t.Fatal("a failed endpoint repair must fail the bring-up")
	}
}

// TestScionServerWaitsForTheBroker: a just-started hub registers its runtime
// broker only after it serves, so the step waits for it before
// register-project's hub link: a hard wait after a start, a soft look on a
// kept hub. Each happens once, in the scion-server step.
func TestScionServerWaitsForTheBroker(t *testing.T) {
	for _, kept := range []bool{false, true} {
		f := scionOKRunner()
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"},
			Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}}
		if kept {
			keptHub(f, app)
		}
		var events []string
		deps := Deps{
			Scion:      hubScion(f, app),
			HubAuthEnv: testHubAuthEnv(),
			BrokerOnline: func(context.Context) (bool, string, error) {
				events = append(events, "hard")
				return true, "", nil
			},
			WaitBrokerReady: func(_ context.Context, project string) (bool, error) {
				events = append(events, "soft "+project)
				// A kept hub's broker is only looked at: not online is no failure.
				return false, nil
			},
			EnsureProjectProvider: func(context.Context, string) error {
				events = append(events, "link")
				return nil
			},
		}
		if err := runApply(app, deps); err != nil {
			t.Fatalf("kept=%v: Run: %v", kept, err)
		}
		first := "hard"
		if kept {
			first = "soft global"
		}
		if len(events) < 2 || events[0] != first || slices.Index(events[1:], first) >= 0 || events[1] != "link" {
			t.Errorf("kept=%v: events = %q, want one %q first, before register-project's link", kept, events, first)
		}
	}
}

// flakyHubLinkRunner fails the first fails `scion hub link` calls with
// errText and passes everything else to inner.
type flakyHubLinkRunner struct {
	inner   *agentLifecycleRunner
	fails   int
	errText string
	links   int
}

func (r *flakyHubLinkRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.inner.RunStdin(ctx, stdin, env, name, args...)
}

func (r *flakyHubLinkRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "hub" && args[1] == "link" {
		r.links++
		if r.links <= r.fails {
			return proc.Result{Code: 1, Stderr: r.errText}, fmt.Errorf("exit status 1: %s", r.errText)
		}
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *flakyHubLinkRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestHubLinkRetriesNoRuntimeBroker: register-project's hub link
// retries a 422 no_runtime_broker a bounded number of times, logging each
// retry, and fails at once on any other error.
func TestHubLinkRetriesNoRuntimeBroker(t *testing.T) {
	old := hubLinkRetryDelays
	hubLinkRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { hubLinkRetryDelays = old })
	const brokerErr = `failed to register agent 'hello': 422 no_runtime_broker: No runtime brokers available for this project`
	for _, tc := range []struct {
		name      string
		fails     int
		errText   string
		wantErr   bool
		wantLinks int
	}{
		{"succeeds after two 422s", 2, brokerErr, false, 3},
		{"gives up after three retries", 9, brokerErr, true, 4},
		{"other error is not retried", 9, "not authenticated to Hub", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := scionOKRunner()
			app := helloApp(t.TempDir())
			runner := &flakyHubLinkRunner{inner: &agentLifecycleRunner{FakeRunner: f, slug: app.Name}, fails: tc.fails, errText: tc.errText}
			var sink logSink
			deps := Deps{
				JailMount: "/lever",
				Scion:     scion.New(runner, scion.Options{HubEndpoint: testHubEndpoint}),
				Log:       sink.logf,
			}
			err := runApply(app, deps)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if runner.links != tc.wantLinks {
				t.Errorf("hub link calls = %d, want %d", runner.links, tc.wantLinks)
			}
			retries := 0
			for _, l := range sink.lines {
				if strings.Contains(l, "retrying the hub link") {
					retries++
				}
			}
			if retries != tc.wantLinks-1 {
				t.Errorf("retry log lines = %d, want %d", retries, tc.wantLinks-1)
			}
		})
	}
}

// TestScionServerConvergesTelemetryWithoutRestarting: the telemetry edit runs
// before the hub starts, a failure fails the apply, and a change is only
// logged; it never restarts a running hub, which would drop every agent's
// connection.
func TestScionServerConvergesTelemetryWithoutRestarting(t *testing.T) {
	run := func(t *testing.T, changed bool, ensureErr error) (calls []proc.Call, logs []string, ensured int, err error) {
		t.Helper()
		f := scionOKRunner()
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"}}
		deps := Deps{
			Scion: hubScion(f, app),
			EnsureScionTelemetry: func(context.Context) (bool, error) {
				ensured++
				for _, c := range f.Calls {
					if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" && c.Args[1] == "start" {
						t.Error("telemetry converged after the hub started — a first bring-up would start it on scion's default")
					}
				}
				return changed, ensureErr
			},
			Log: func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) },
		}
		err = runApply(app, deps)
		return f.Calls, logs, ensured, err
	}
	stopped := func(calls []proc.Call) bool {
		for _, c := range calls {
			if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" && c.Args[1] == "stop" {
				return true
			}
		}
		return false
	}
	mentions := func(logs []string) bool {
		for _, l := range logs {
			if strings.Contains(l, "telemetry") && strings.Contains(l, "lever stop && lever up") {
				return true
			}
		}
		return false
	}

	calls, logs, ensured, err := run(t, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("EnsureScionTelemetry called %d times, want 1", ensured)
	}
	if stopped(calls) || mentions(logs) {
		t.Fatalf("a converged telemetry block restarted the hub or logged a change: logs=%q", logs)
	}

	calls, logs, _, err = run(t, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stopped(calls) {
		t.Fatal("a telemetry change restarted the hub — lever must not bounce a running hub for it")
	}
	if !mentions(logs) {
		t.Fatalf("a telemetry change did not tell the operator how to apply it: %q", logs)
	}

	if _, _, _, err := run(t, false, errors.New("guest unreachable")); err == nil {
		t.Fatal("EnsureScionTelemetry's failure did not fail the apply")
	}
}

// TestRemoteOffBrokerOnlyDoesNotDriveScion: a BrokerOnly plan need not have a
// scion binary, so converging remote access off must not touch the hub.
func TestRemoteOffBrokerOnlyDoesNotDriveScion(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img"}}
	stopped := 0
	deps := Deps{
		Scion:           hubScion(f, app),
		StopRemoteProxy: func(context.Context) error { stopped++; return nil },
	}
	if err := Run(context.Background(), app, fillDeps(deps), PlanOpts{BrokerOnly: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stopped != 1 {
		t.Fatalf("StopRemoteProxy called %d times, want 1", stopped)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("the broker-only plan drove scion or the guest: %+v", f.Calls)
	}
}

// stoppedHubRunner answers `scion server stop` the way scion does when nothing
// is running.
type stoppedHubRunner struct {
	inner *agentLifecycleRunner
}

func (r *stoppedHubRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.inner.RunStdin(ctx, stdin, env, name, args...)
}

func (r *stoppedHubRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "stop" {
		r.inner.FakeRunner.Calls = append(r.inner.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Code: 1, Stderr: "Error: server daemon is not running"}, fmt.Errorf("exit status 1")
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *stoppedHubRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestScionServerRestartsAHubThatIsAlreadyStopped: `scion server stop` exits
// non-zero with "server daemon is not running" on a hub that is already down,
// so a restart must treat that as a no-op and then start.
func TestScionServerRestartsAHubThatIsAlreadyStopped(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img"},
		Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}}
	runner := &stoppedHubRunner{inner: &agentLifecycleRunner{FakeRunner: f, slug: app.Name}}
	// A running hub whose guest key file changed: the restart's stop meets a
	// daemon that already went away.
	scriptRunningHub(f, runningHubOut(42, matchingHubArgv(app), testHubAuthEnv()))
	deps := Deps{
		Scion:         scion.New(runner, scion.Options{HubEndpoint: testHubEndpoint}),
		HubAuthEnv:    testHubAuthEnv(),
		EnsureHubAuth: func(context.Context) (string, error) { return "/etc/lever/hub-auth.pub", nil },
	}

	if err := runApply(app, deps); err != nil {
		t.Fatalf("apply failed on a hub that was already stopped: %v", err)
	}
	var stopped, started bool
	for _, c := range f.Calls {
		if c.Name != "scion" || len(c.Args) < 2 || c.Args[0] != "server" {
			continue
		}
		switch c.Args[1] {
		case "stop":
			stopped = true
		case "start":
			started = true
			if !stopped {
				t.Fatal("the hub was started before the restart's stop leg ran")
			}
		}
	}
	if !stopped || !started {
		t.Fatalf("stop=%v start=%v, want the restart to stop (tolerantly) and then start", stopped, started)
	}
}

// TestAgentTemplateGetsTheJailPath: EnsureAgentTemplate runs `scion config`
// inside the jail, where the host tree exists only at the mount point.
func TestAgentTemplateGetsTheJailPath(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/Users/someone/instance/workspace",
		Manager: config.Manager{Image: "img"},
	}
	var got string
	r := &run{app: app, d: Deps{
		JailMount: "/lever",
		EnsureAgentTemplate: func(_ context.Context, projectDir string) (bool, error) {
			got = projectDir
			return false, nil
		},
	}}
	if err := r.agentTemplate(context.Background(), Step{Kind: KindAgentTemplate, Target: app.Tree}); err != nil {
		t.Fatalf("agentTemplate: %v", err)
	}
	if got != "/lever" {
		t.Fatalf("projectDir = %q, want the jail path %q — the host path does not exist inside the jail", got, "/lever")
	}
}

// TestRunRefusesIncompleteDeps: a nil required collaborator fails Run by name
// before any step runs.
func TestRunRefusesIncompleteDeps(t *testing.T) {
	app, f := newObserveFirstApp(t)
	deps := fillDeps(Deps{Scion: scion.New(f, scion.Options{})})
	deps.VerifyAgentRole = nil
	err := Run(context.Background(), app, deps, PlanOpts{})
	if err == nil || err.Error() != "apply: Deps.VerifyAgentRole is not set" {
		t.Fatalf("err = %v, want the missing field named", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("no step may run before the check: %d calls", len(f.Calls))
	}
	if err := fillDeps(Deps{Scion: scion.New(f, scion.Options{})}).check(); err != nil {
		t.Fatalf("a complete Deps must pass: %v", err)
	}
	if err := (Deps{}).check(); err == nil {
		t.Fatal("an empty Deps must fail")
	}
}

// Standing instructions reach scion as inline config on stdin, never argv:
// the task rides argv into a tmux command capped at 16 KiB. The file is read
// at the instance root like the prompt, and the start is otherwise the same
// create.
func TestStartManagerPassesInstructionsOnStdin(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	manual := "# Manual\n\nStanding orders: it's long.\n"
	if err := os.WriteFile(filepath.Join(dir, "manual.md"), []byte(manual), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte("Read your manual, then begin."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  prompt_file: manager.md\n  instructions_file: manual.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var start *proc.Call
	for i := range f.Calls {
		if j := strings.Join(f.Calls[i].Args, " "); strings.Contains(j, "-- hello") {
			start = &f.Calls[i]
		}
	}
	if start == nil {
		t.Fatalf("no manager start call; calls=%+v", f.Calls)
	}
	argv := strings.Join(start.Args, " ")
	if !strings.Contains(argv, "-- hello Read your manual, then begin.") || !strings.Contains(argv, "--config -") {
		t.Fatalf("start argv %q must keep the task positional and add --config -", argv)
	}
	if strings.Contains(argv, "Standing orders") {
		t.Fatalf("start argv %q must not carry the manual", argv)
	}
	var body struct {
		AgentInstructions string `json:"agent_instructions"`
	}
	if err := json.Unmarshal([]byte(start.Stdin), &body); err != nil || body.AgentInstructions != manual {
		t.Fatalf("stdin must be JSON carrying the manual verbatim; got %q (err %v)", start.Stdin, err)
	}
}

// A prompt over the tmux budget fails at apply, naming the file, with no start
// attempted; otherwise the container exits "command too long" after the
// broker-ready wait.
func TestStartManagerFailsFastOnOversizedPrompt(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte(strings.Repeat("x", scion.TaskArgvBudget+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  prompt_file: manager.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	err = runApply(app, deps)
	var tl *scion.TaskTooLongError
	if !errors.As(err, &tl) {
		t.Fatalf("want *scion.TaskTooLongError from apply, got %v", err)
	}
	if !strings.Contains(err.Error(), "manager.md") {
		t.Fatalf("error %q must name the prompt file", err)
	}
	for _, c := range f.Calls {
		if j := strings.Join(c.Args, " "); strings.Contains(j, "-- hello") {
			t.Fatalf("no start may be attempted for an oversized prompt; got %q", j)
		}
	}
}

// A manual that scion would misread (a leading file://) or that cannot fit
// its hub request fails at apply naming the file, with no start attempted.
func TestStartManagerRefusesUnsendableInstructionsNamingTheFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "manual.md"), []byte("file:///etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  instructions_file: manual.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	err = runApply(app, deps)
	if err == nil || !strings.Contains(err.Error(), "manual.md") || !strings.Contains(err.Error(), "file://") {
		t.Fatalf("want an error naming manual.md and the file:// rule, got %v", err)
	}
	for _, c := range f.Calls {
		if j := strings.Join(c.Args, " "); strings.Contains(j, "-- hello") {
			t.Fatalf("no start may be attempted; got %q", j)
		}
	}
}

// A resume carries no instructions (no --config, no stdin) even when the
// config names a file: scion re-projects what it staged at create, and would
// ignore them on resume.
func TestStartManagerResumeCarriesNoInstructions(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "manual.md"), []byte("# Manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  instructions_file: manual.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	r := &agentLifecycleRunner{FakeRunner: f, slug: app.Name, initPhase: "suspended", initContainerStatus: "stopped"}
	if err := runApply(app, Deps{Scion: scion.New(r, scion.Options{})}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 1 || r.startCalls != 0 {
		t.Fatalf("resumeCalls=%d startCalls=%d, want 1/0", r.resumeCalls, r.startCalls)
	}
	for _, c := range f.Calls {
		if slices.Contains(c.Args, "--config") || c.Stdin != "" {
			t.Fatalf("a resume must carry no instructions; call %q had stdin %q", strings.Join(c.Args, " "), c.Stdin)
		}
	}
}

// scion reports the record running before the harness runs, and the harness
// can die a moment later. The settle window must catch the flip and name it as
// a death, not a failure to come up.
func TestStartManagerFailsWhenHarnessDiesDuringSettle(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", dieAfterLiveLists: 2}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		// A long window on purpose: two dead readings end it a few
		// milliseconds in, while a short one lets a late poll on a loaded
		// machine pass the gate before the death is due.
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 10 * time.Second},
	}
	err := runApply(app, deps)
	if !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied from the start-manager gate, got %v", err)
	}
	if !strings.Contains(err.Error(), "start-manager") || !strings.Contains(err.Error(), `Exited (1)`) {
		t.Fatalf("error must carry the step and the failing observation: %v", err)
	}
}

// The same death is invisible to a zero-settle gate, which is why the settle
// default must not be dropped.
func TestStartManagerZeroSettleMissesALaterDeath(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", dieAfterLiveLists: 1}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("zero settle returns on the first live look by design: %v", err)
	}
}

// `lever up`'s resume path gates through WaitManagerLive with the full settle
// window: a record that dies after the resume fails the up, named as a death.
func TestWaitManagerLiveGatesUpsResumePath(t *testing.T) {
	f := scionOKRunner()
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 1 second", dieAfterLiveLists: 1}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 60 * time.Millisecond},
	}
	err := WaitManagerLive(context.Background(), deps, "hello", "/lever", nil)
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), "up: manager") {
		t.Fatalf("want an up-prefixed ErrAgentDied, got %v", err)
	}
}

// An apply over a running manager acts on nothing, so it takes one look and
// does not hold for the settle window: `lever apply` and `lever reload` are
// the everyday reconfigure commands.
func TestApplyOverRunningManagerTakesOneLookNotTheSettle(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 4 days"}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 500 * time.Millisecond},
	}
	start := time.Now()
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if el := time.Since(start); el >= 500*time.Millisecond {
		t.Fatalf("apply over a running manager took %s: it must not hold for the settle window", el)
	}
	if r.startCalls != 0 || r.resumeCalls != 0 || r.listCalls > 2 {
		t.Fatalf("start=%d resume=%d list=%d; want 0/0/<=2 (observe + one look)", r.startCalls, r.resumeCalls, r.listCalls)
	}
}

// A resume DID act, so it is held for the settle window like a create.
func TestStartManagerResumeIsHeldForTheSettleWindow(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped", dieAfterLiveLists: 1}
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 60 * time.Millisecond},
	}
	err := runApply(app, deps)
	if !errors.Is(err, scion.ErrAgentDied) || r.resumeCalls != 1 {
		t.Fatalf("want ErrAgentDied after a resume (resumeCalls=%d), got %v", r.resumeCalls, err)
	}
}

// RetryBudget.or fills Attempts/Interval per field, so a budget that sets
// only Settle keeps it; the zero value is the default entirely.
func TestRetryBudgetOrFillsPerField(t *testing.T) {
	def := RetryBudget{Attempts: 15, Interval: time.Second, Settle: 10 * time.Second}
	if got := (RetryBudget{}).or(def); got != def {
		t.Fatalf("zero value must be the default, got %+v", got)
	}
	if got := (RetryBudget{Settle: 3 * time.Second}).or(def); got != (RetryBudget{Attempts: 15, Interval: time.Second, Settle: 3 * time.Second}) {
		t.Fatalf("settle-only budget must keep its settle and inherit the rest, got %+v", got)
	}
	if got := (RetryBudget{Attempts: 3, Interval: time.Millisecond}).or(def); got.Settle != 0 {
		t.Fatalf("an explicit zero settle beside a set budget must stay zero, got %+v", got)
	}
}

// The production manager gate holds for ten seconds. Pinned here because the
// CHANGELOG promises it and every other test zeroes it.
func TestDefaultManagerLiveRetryHoldsForTenSeconds(t *testing.T) {
	if defaultManagerLiveRetry.Settle < 10*time.Second {
		t.Fatalf("default settle = %s, want >= 10s", defaultManagerLiveRetry.Settle)
	}
}

func tarStep(d Deps) error {
	return (&run{app: &config.App{}, d: fillDeps(d)}).step(context.Background(),
		Step{Kind: KindLoadImage, Target: "img", TarPath: "/inst/images/img.tar"})
}

// TestLoadImageStepUsesTarSourceWhenSet: with a TarPath the step loads from
// the archive and never consults host docker; the prune after a load is
// unchanged.
func TestLoadImageStepUsesTarSourceWhenSet(t *testing.T) {
	var dockerCalls, tarLoads, prunes int
	var gotRef, gotTar string
	d := Deps{
		LoadImage:   func(context.Context, string) error { dockerCalls++; return nil },
		ImageLoaded: func(context.Context, string) bool { dockerCalls++; return false },
		LoadImageTar: func(_ context.Context, ref, tar string, _ func(string) error) error {
			tarLoads++
			gotRef, gotTar = ref, tar
			return nil
		},
		ImageLoadedTar: func(context.Context, string, string) bool { return false },
		PruneImages:    func(context.Context) error { prunes++; return nil },
	}
	if err := tarStep(d); err != nil {
		t.Fatalf("step: %v", err)
	}
	if dockerCalls != 0 {
		t.Errorf("host-docker collaborators called %d times, want 0", dockerCalls)
	}
	if tarLoads != 1 || gotRef != "img" || gotTar != "/inst/images/img.tar" {
		t.Errorf("LoadImageTar calls = %d (%q, %q), want 1 (img, /inst/images/img.tar)", tarLoads, gotRef, gotTar)
	}
	if prunes != 1 {
		t.Errorf("PruneImages calls = %d, want 1", prunes)
	}
}

// TestLoadImageStepSkipsWhenTarAlreadyLoaded: the tar-digest guard skips the
// stream and the prune exactly as the host-docker guard does.
func TestLoadImageStepSkipsWhenTarAlreadyLoaded(t *testing.T) {
	var loads, prunes int
	d := Deps{
		LoadImageTar:   func(context.Context, string, string, func(string) error) error { loads++; return nil },
		ImageLoadedTar: func(context.Context, string, string) bool { return true },
		PruneImages:    func(context.Context) error { prunes++; return nil },
	}
	if err := tarStep(d); err != nil {
		t.Fatalf("step: %v", err)
	}
	if loads != 0 || prunes != 0 {
		t.Errorf("loads = %d, prunes = %d, want 0/0 (already loaded)", loads, prunes)
	}
}

// TestLoadImageStepTarLoadErrorIsFatal: a tar load failure propagates and
// the prune does not run.
func TestLoadImageStepTarLoadErrorIsFatal(t *testing.T) {
	var prunes int
	d := Deps{
		LoadImageTar: func(context.Context, string, string, func(string) error) error { return errors.New("tag mismatch") },
		PruneImages:  func(context.Context) error { prunes++; return nil },
	}
	err := tarStep(d)
	testutil.WantErrContaining(t, err, "tag mismatch")
	if prunes != 0 {
		t.Errorf("PruneImages calls = %d, want 0 after a failed load", prunes)
	}
}

// The tar collaborators are required like every other: a wiring gap fails
// loudly before the first step.
func TestRunRefusesMissingTarDeps(t *testing.T) {
	d := fillDeps(Deps{Scion: scion.New(proc.NewFakeRunner(), scion.Options{})})
	d.LoadImageTar = nil
	testutil.WantErrContaining(t, d.check(), "LoadImageTar")
	d = fillDeps(Deps{Scion: scion.New(proc.NewFakeRunner(), scion.Options{})})
	d.ImageLoadedTar = nil
	testutil.WantErrContaining(t, d.check(), "ImageLoadedTar")
}

// runApplyFresh carries --fresh into the plan: `up` cannot delete a record
// while the hub is down, so the converge step does it once the hub is up.
func runApplyFresh(app *config.App, deps Deps) error {
	return Run(context.Background(), app, fillDeps(withContainerGone(deps)), PlanOpts{Fresh: true})
}

// goneContainer is podman knowing no container by any name.
type goneContainer struct{}

func (goneContainer) State(context.Context, string) (jail.ContainerState, error) {
	return jail.ContainerState{}, jail.ErrNoContainer
}
func (goneContainer) Remove(context.Context, string) error { return nil }

// withContainerGone wires podman, when the test left it out, as showing the
// discarded manager's container gone, which a fresh start needs before it
// stages a ticket. Only for a run that discards the manager: a kept running
// record over no container is recovered, not kept.
func withContainerGone(d Deps) Deps {
	if d.StaleContainer == nil {
		d.StaleContainer = goneContainer{}
	}
	return d
}

// TestStartManagerFreshDiscardsPresentRecord: with Fresh set, any present
// record (suspended, running, error, transitional or unknown) is deleted and
// the manager created anew; nothing is resumed.
func TestStartManagerFreshDiscardsPresentRecord(t *testing.T) {
	// --fresh is an explicit discard, so transitional and unknown phases are
	// deleted without the settle wait or the refusal a plain up applies.
	for _, phase := range []string{"suspended", "stopped", "running", "error", "starting", "definitely-not-a-phase"} {
		t.Run(phase, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			cs := "stopped"
			if phase == "running" {
				cs = "Up 4 days"
			}
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: phase, initContainerStatus: cs}
			var logged []string
			deps := Deps{
				Scion: scion.New(r, scion.Options{}),
				Log:   func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) },
			}
			if err := runApplyFresh(app, deps); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if r.deleteCalls != 1 || r.startCalls != 1 || r.resumeCalls != 0 {
				t.Fatalf("delete/start/resume = %d/%d/%d, want 1/1/0 (fresh must discard the %s record and create)", r.deleteCalls, r.startCalls, r.resumeCalls, phase)
			}
			if !strings.Contains(strings.Join(logged, "\n"), "--fresh") {
				t.Errorf("the discard must be announced, got %q", logged)
			}
			// One extra list confirms the record is gone before the create.
			if r.listCalls > 3 {
				t.Errorf("listCalls = %d, want <= 3 (--fresh must not wait for a %s record to settle)", r.listCalls, phase)
			}
		})
	}
}

// Fresh over an ABSENT record is a plain create: nothing to delete.
func TestStartManagerFreshWithNoRecordJustCreates(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	if err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.deleteCalls != 0 || r.startCalls != 1 {
		t.Fatalf("delete/start = %d/%d, want 0/1", r.deleteCalls, r.startCalls)
	}
}

// TestStartManagerFreshBacksUpBeforeTheDelete: the backup runs
// on the record about to be deleted, before the delete; a failed backup stops
// the fresh start with the record kept and nothing created.
func TestStartManagerFreshBacksUpBeforeTheDelete(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 2 days"}
	var seen *scion.Agent
	var deletesAtBackup int
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		BackupManagerConversation: func(_ context.Context, rec *scion.Agent) error {
			seen, deletesAtBackup = rec, r.deleteCalls
			return nil
		},
	}
	if err := runApplyFresh(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen == nil || seen.Phase != "running" || deletesAtBackup != 0 || r.deleteCalls != 1 {
		t.Fatalf("backup saw %+v with %d deletes before it; deletes %d", seen, deletesAtBackup, r.deleteCalls)
	}

	app, f = newObserveFirstApp(t)
	r = &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps = Deps{
		Scion:                     scion.New(r, scion.Options{}),
		BackupManagerConversation: func(context.Context, *scion.Agent) error { return errors.New("disk full") },
	}
	err := runApplyFresh(app, deps)
	testutil.WantErrContaining(t, err, "disk full")
	testutil.WantErrContaining(t, err, "NOT deleted")
	testutil.WantErrContaining(t, err, "--no-backup")
	if r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 {
		t.Fatalf("delete/start/resume = %d/%d/%d after a failed backup, want 0/0/0", r.deleteCalls, r.startCalls, r.resumeCalls)
	}

	// No record: nothing to back up, and the hook is not called.
	app, f = newObserveFirstApp(t)
	r = &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	called := false
	deps = Deps{
		Scion:                     scion.New(r, scion.Options{}),
		BackupManagerConversation: func(context.Context, *scion.Agent) error { called = true; return nil },
	}
	if err := runApplyFresh(app, deps); err != nil || called {
		t.Fatalf("absent record: err %v, backup called %v", err, called)
	}
}

// A delete that fails under --fresh is fatal: with the record still present,
// resuming it would silently defeat the flag.
func TestStartManagerFreshDeleteFailureIsFatal(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped", deleteErr: errors.New("hub: 500")}
	err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})})
	testutil.WantErrContaining(t, err, "delete failed")
	if r.resumeCalls != 0 || r.startCalls != 0 {
		t.Fatalf("resume/start = %d/%d, want 0/0 after a failed --fresh delete", r.resumeCalls, r.startCalls)
	}
}

// TestStartManagerFreshBypassesPreRoleGuard: the pre-role guard's own remedy
// is to delete the agent so lever recreates it, which is what --fresh does,
// so the guard must stand aside for a record about to be discarded.
func TestStartManagerFreshBypassesPreRoleGuard(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps := Deps{
		Scion:           scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error { return errPreRoleRefusal },
	}
	if err := runApplyFresh(app, deps); err != nil {
		t.Fatalf("--fresh must discard a role-defective record, not be refused by the guard: %v", err)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Fatalf("delete/start = %d/%d, want 1/1", r.deleteCalls, r.startCalls)
	}
	// Without --fresh the guard still refuses.
	r = &agentLifecycleRunner{FakeRunner: scionOKRunner(), slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps.Scion = scion.New(r, scion.Options{})
	if err := runApply(app, deps); !errors.Is(err, errPreRoleRefusal) {
		t.Fatalf("plain up must still be refused by the guard, got %v", err)
	}
}

// TestRunLoadImageTarForwardsImageTagPolicy: every tag in the archive is held
// to Deps.ImageTagPolicy and refused when it names another configured image,
// before a byte is streamed.
func TestRunLoadImageTarForwardsImageTagPolicy(t *testing.T) {
	sentinel := errors.New("policy sentinel")
	var got func(string) error
	var calls int
	d := fillDeps(Deps{
		ImageTagPolicy: func(string) error { return sentinel },
		LoadImageTar: func(_ context.Context, _, _ string, allow func(string) error) error {
			calls++
			got = allow
			return nil
		},
	})
	app := &config.App{Manager: config.Manager{Image: "lever.local/mgr:arm64"},
		Workers: []config.Worker{{Name: "w", Image: "lever.local/x:arm64"}}}
	step := Step{Target: "lever.local/x:arm64", TarPath: "/inst/images/x.tar"}
	if err := runLoadImage(context.Background(), step, d, app); err != nil {
		t.Fatalf("runLoadImage: %v", err)
	}
	if calls != 1 || got == nil || !errors.Is(got("anything"), sentinel) {
		t.Fatalf("the configured policy must reach LoadImageTar (calls=%d, got nil=%v)", calls, got == nil)
	}
	d.ImageTagPolicy = nil
	if err := runLoadImage(context.Background(), step, d, app); err != nil {
		t.Fatalf("runLoadImage: %v", err)
	}
	for _, ok := range []string{"lever.local/x:arm64", "x:arm64", "docker.io/x:arm64", "lever.local/other:1", "scionlocal/mgr:arm64"} {
		if err := got(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"lever.local/mgr:arm64", "docker.io/mgr:arm64", "docker.io/library/mgr:arm64", "mgr:arm64"} {
		if err := got(bad); err == nil || !strings.Contains(err.Error(), "configured for another agent") {
			t.Errorf("%s: want refused, got %v", bad, err)
		}
	}
}

// TestRegisterEnsuresTheAgentRoleCeilingOnBothPaths: the ceiling is a
// project setting the hub enforces at agent create, so it must be written on
// both the already-registered and the re-init path.
func TestRegisterEnsuresTheAgentRoleCeilingOnBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
	}{
		{"already registered", true},
		{"fresh registration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			f := scionOKRunner()
			app := helloApp(tree)
			var calls []string
			deps := Deps{
				JailMount: "/lever",
				Scion:     hubScion(f, app),
				ScionProjectRegistered: func(context.Context, string) (bool, error) {
					return tc.registered, nil
				},
				EnsureAgentRoleCeiling: func(_ context.Context, project string) error {
					calls = append(calls, project)
					return nil
				},
			}
			if err := runApply(app, deps); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(calls) != 1 || calls[0] != "lever" {
				t.Fatalf("EnsureAgentRoleCeiling calls = %+v, want [lever]", calls)
			}
		})
	}
}

// A ceiling the hub refused is a bring-up failure, or lever's own --role
// stamp would be the only role bound while the operator believes the hub
// enforces one too.
func TestRegisterFailsWhenTheCeilingFails(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		EnsureAgentRoleCeiling: func(context.Context, string) error {
			return errForbidden
		},
	}
	testutil.WantErrIs(t, runApply(app, deps), errForbidden)
}

type sessionSpy struct{ begun, committed []string }

func (s *sessionSpy) begin(agent string) func() error {
	s.begun = append(s.begun, agent)
	return func() error { s.committed = append(s.committed, agent); return nil }
}

// TestStartManagerRecordsOnlyFreshSessions: a create (absent record,
// --fresh) records the manager's fresh session; a resume, a no-op or a failed
// resume (which keeps the record) never does, so a resumed conversation is
// never taken for one that loaded the current skill.
func TestStartManagerRecordsOnlyFreshSessions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		r          func(f *proc.FakeRunner) *agentLifecycleRunner
		fresh      bool
		wantCommit bool
	}{
		{"absent", func(f *proc.FakeRunner) *agentLifecycleRunner {
			return &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
		}, false, true},
		{"suspended", func(f *proc.FakeRunner) *agentLifecycleRunner {
			return &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
		}, false, false},
		{"running", func(f *proc.FakeRunner) *agentLifecycleRunner {
			return &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 6 seconds"}
		}, false, false},
		{"fresh over suspended", func(f *proc.FakeRunner) *agentLifecycleRunner {
			return &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
		}, true, true},
		{"failed resume", func(f *proc.FakeRunner) *agentLifecycleRunner {
			return &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped",
				resumeErr: fmt.Errorf("cannot resume agent 'hello': agent does not exist")}
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			spy := &sessionSpy{}
			deps := Deps{Scion: scion.New(tc.r(f), scion.Options{}), BeginSession: spy.begin, Log: func(string, ...any) {}}
			run := runApply
			if tc.fresh {
				run = runApplyFresh
			}
			if err := run(app, deps); err != nil && tc.name != "failed resume" {
				t.Fatalf("Run: %v", err)
			}
			if got := len(spy.committed) == 1 && spy.committed[0] == "hello"; got != tc.wantCommit {
				t.Fatalf("begun %v committed %v, want a commit: %v", spy.begun, spy.committed, tc.wantCommit)
			}
			if !tc.wantCommit && len(spy.begun) != 0 {
				t.Fatalf("a resume or no-op began a session record: %v", spy.begun)
			}
		})
	}
}

// TestStartManagerCreateRaceIsNotRecorded: when scion answers the create
// with "already exists" (a record the observe step missed), the existing
// session is not recorded as fresh.
func TestStartManagerCreateRaceIsNotRecorded(t *testing.T) {
	app, f := newObserveFirstApp(t)
	spy := &sessionSpy{}
	lr := &agentLifecycleRunner{FakeRunner: f, slug: "hello", startErr: errors.New("agent 'hello' already exists")}
	r := &run{app: app, d: fillDeps(Deps{Scion: scion.New(lr, scion.Options{}), BeginSession: spy.begin}),
		brokerStart: RetryBudget{Attempts: 1, Interval: time.Millisecond}, minted: true}
	if err := r.startManagerCreate(context.Background(), scion.StartOpts{Worker: "hello"}, false); err != nil {
		t.Fatalf("create race: %v", err)
	}
	if len(spy.committed) != 0 {
		t.Fatalf("an existing session was recorded as fresh: %v", spy.committed)
	}
}

// TestStartManagerFreshNeverReportsADiscardThatDidNotHappen: `--fresh` evicts
// a session, and the observed record decides, not an error's wording: a
// delete failing "not found", a delete that reports success but leaves the
// record, and a create meeting an existing record all end in an error, never
// "is up" over the old session.
func TestStartManagerFreshNeverReportsADiscardThatDidNotHappen(t *testing.T) {
	for _, text := range []string{"project not found", "sh: scion: command not found",
		"Failed to delete agent on runtime broker: container not found"} {
		app, f := newObserveFirstApp(t)
		r := &keptRecordRunner{agentLifecycleRunner: agentLifecycleRunner{FakeRunner: f, slug: "hello",
			initPhase: "running", initContainerStatus: "Up 3 days"}, second: text}
		var logged logSink
		err := runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(5), Scion: scion.New(r, scion.Options{}), Log: logged.logf})
		if err == nil {
			t.Fatalf("%q: --fresh reported success with the record still there", text)
		}
		testutil.WantErrContaining(t, err, "delete failed")
		if r.startCalls != 0 {
			t.Fatalf("%q: startCalls=%d, want 0 (nothing is created over a kept record)", text, r.startCalls)
		}
	}

	// The delete reports success and the record stays.
	app, f := newObserveFirstApp(t)
	silent := &keptRecordRunner{agentLifecycleRunner: agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped"}, lies: true}
	err := runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(3), Scion: scion.New(silent, scion.Options{}), Log: func(string, ...any) {}})
	testutil.WantErrContaining(t, err, "still there", "NOT discarded")
	if silent.startCalls != 0 {
		t.Fatalf("startCalls=%d after a delete that removed nothing", silent.startCalls)
	}

	// The record is gone, and the create still meets "already exists".
	app, f = newObserveFirstApp(t)
	race := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped",
		startErr: errors.New("agent 'hello' already exists")}
	err = runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(3), Scion: scion.New(race, scion.Options{}), Log: func(string, ...any) {}})
	testutil.WantErrContaining(t, err, "--fresh expected no manager record", "did NOT report a fresh manager")

	// --fresh with NO record observed (scion's lazy sync hid a live one): the
	// create's "already exists" is that session, not a success.
	app, f = newObserveFirstApp(t)
	hidden := &agentLifecycleRunner{FakeRunner: f, slug: "hello", startErr: errors.New("agent 'hello' already exists")}
	err = runApplyFresh(app, Deps{BrokerStartRetry: fastRetry(3), Scion: scion.New(hidden, scion.Options{}), Log: func(string, ...any) {}})
	testutil.WantErrContaining(t, err, "--fresh expected no manager record")
	if hidden.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d, want 0 (nothing was observed to delete)", hidden.deleteCalls)
	}
	// Without --fresh the same answer stays an idempotent success.
	app, f = newObserveFirstApp(t)
	again := &agentLifecycleRunner{FakeRunner: f, slug: "hello", startErr: errors.New("agent 'hello' already exists")}
	_ = runApply(app, Deps{BrokerStartRetry: fastRetry(3), Scion: scion.New(again, scion.Options{}), Log: func(string, ...any) {}})
	if again.startCalls != 1 {
		t.Fatalf("plain apply over a create race: startCalls=%d, want 1", again.startCalls)
	}
}

// keptRecordRunner models a delete that never removes the record: the first
// attempt reports the runtime-broker race and the second reports `second`
// (or, with lies set, every attempt reports success).
type keptRecordRunner struct {
	agentLifecycleRunner
	second  string
	lies    bool
	deletes int
}

func (r *keptRecordRunner) intercept(args []string) (proc.Result, error, bool) {
	if r.verb(args) != "delete" {
		return proc.Result{}, nil, false
	}
	r.ensureInit()
	r.deletes++
	if r.lies {
		return proc.Result{Stdout: "ok"}, nil, true
	}
	text := r.second
	if r.deletes == 1 {
		text = "context deadline exceeded"
	}
	err := errors.New(text)
	return proc.Result{Code: 1, Stderr: text}, err, true
}

func (r *keptRecordRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	if res, err, ok := r.intercept(args); ok {
		return res, err
	}
	return r.agentLifecycleRunner.Run(ctx, env, name, args...)
}

func (r *keptRecordRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if res, err, ok := r.intercept(args); ok {
		return res, err
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

// deleteThenGoneRunner models a delete whose first attempt removes the record
// and still reports the runtime-broker race; later attempts find no agent.
type deleteThenGoneRunner struct {
	agentLifecycleRunner
	deletes int
}

func (r *deleteThenGoneRunner) intercept(args []string) (proc.Result, error, bool) {
	if r.verb(args) != "delete" {
		return proc.Result{}, nil, false
	}
	r.deletes++
	if r.deletes == 1 {
		r.ensureInit()
		r.phase, r.containerStatus = "", ""
		err := fmt.Errorf("context deadline exceeded")
		return proc.Result{Code: 1, Stderr: err.Error()}, err, true
	}
	err := fmt.Errorf("agent 'hello' not found")
	return proc.Result{Code: 1, Stderr: err.Error()}, err, true
}

func (r *deleteThenGoneRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	if res, err, ok := r.intercept(args); ok {
		return res, err
	}
	return r.agentLifecycleRunner.Run(ctx, env, name, args...)
}

func (r *deleteThenGoneRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if res, err, ok := r.intercept(args); ok {
		return res, err
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func TestWarnManagerNestedVirt(t *testing.T) {
	var logs []string
	r := &run{app: &config.App{Name: "assistant", Backend: config.BackendLima, NestedVirt: true}, d: Deps{Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}}
	r.d.ProbeContainerDevice = func(ctx context.Context, ref, p string) (bool, error) { return false, nil }
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
	if len(logs) != 1 || !strings.Contains(logs[0], "has no /dev/kvm") || !strings.Contains(logs[0], "lever up --fresh --keep-conversation") {
		t.Fatalf("on, lacks: logs = %q", logs)
	}
	logs = nil
	r.d.ProbeContainerDevice = func(ctx context.Context, ref, p string) (bool, error) { return true, nil }
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("on, has: no warning expected, got %q", logs)
	}

	// Off: a manager created while it was on keeps the device.
	r.app.NestedVirt = false
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
	if len(logs) != 1 || !strings.Contains(logs[0], "still has /dev/kvm") || !strings.Contains(logs[0], "lever up --fresh") {
		t.Fatalf("off, has: logs = %q", logs)
	}
	logs = nil
	r.d.ProbeContainerDevice = func(ctx context.Context, ref, p string) (bool, error) { return false, nil }
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("off, lacks: no warning expected, got %q", logs)
	}
	r.d.ProbeContainerDevice = func(ctx context.Context, ref, p string) (bool, error) { return false, errors.New("boom") }
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("off, probe error: stays quiet, got %q", logs)
	}

	r.app.Backend = config.BackendOrbstack
	r.d.ProbeContainerDevice = func(ctx context.Context, ref, p string) (bool, error) {
		t.Fatal("must not probe a non-Lima instance with nested_virt off")
		return false, nil
	}
	r.warnManagerNestedVirt(context.Background(), "/lever/proj")
}

func TestManagerVolumesNestedVirt(t *testing.T) {
	app := &config.App{Name: "m"}
	if got := managerVolumes("/lever", app); got != nil {
		t.Fatalf("off, no read_only: want no volumes (no inline config), got %v", got)
	}
	app.NestedVirt = true
	want := []scion.VolumeMount{{Source: "/dev/kvm", Target: "/dev/kvm"}}
	if got := managerVolumes("/lever", app); !reflect.DeepEqual(got, want) {
		t.Fatalf("on: volumes = %v, want %v", got, want)
	}
}

func TestManagerStartOptsNestedVirtVolume(t *testing.T) {
	r := &run{app: &config.App{Name: "m", Backend: config.BackendLima, NestedVirt: true, Broker: config.Broker{LLMAuth: config.LLMAuthSubscription}}}
	opts, err := r.managerStartOpts(context.Background(), "/lever/proj", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(opts.Volumes, scion.VolumeMount{Source: KVMDevice, Target: KVMDevice}) {
		t.Fatalf("the manager create must carry the /dev/kvm volume, got %v", opts.Volumes)
	}
}

// The manager is created on a qualified image name: scion rewrites a bare one
// to the hub's image registry, which any agent can set.
func TestManagerStartOptsQualifiedImage(t *testing.T) {
	for _, img := range []string{"lever-claude:arm64", "lever.local/lever-claude:arm64"} {
		r := &run{app: &config.App{Name: "m", Manager: config.Manager{Image: img}, Broker: config.Broker{LLMAuth: config.LLMAuthSubscription}}}
		opts, err := r.managerStartOpts(context.Background(), "/lever/proj", "task", "")
		if err != nil {
			t.Fatal(err)
		}
		if opts.Image != "lever.local/lever-claude:arm64" {
			t.Fatalf("manager.image %s: the create runs %q, want lever.local/lever-claude:arm64", img, opts.Image)
		}
	}
}

// manager.resources reaches the manager's create as scion limits, the
// memory in scion's notation; unset sends none.
func TestManagerStartOptsResources(t *testing.T) {
	app := &config.App{Name: "m", Broker: config.Broker{LLMAuth: config.LLMAuthSubscription}}
	r := &run{app: app}
	opts, err := r.managerStartOpts(context.Background(), "/lever/proj", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Resources != (scion.Resources{}) {
		t.Fatalf("unset: resources = %+v, want none", opts.Resources)
	}
	app.Manager.Resources = config.Resources{Memory: "3GiB", CPU: "1.5"}
	if opts, err = r.managerStartOpts(context.Background(), "/lever/proj", "task", ""); err != nil {
		t.Fatal(err)
	}
	if want := (scion.Resources{Memory: "3Gi", CPU: "1.5"}); opts.Resources != want {
		t.Fatalf("resources = %+v, want %+v", opts.Resources, want)
	}
}

// The manager mounts each shared folder at /shared/<name>: read-only, and
// over its tree path read-only too, unless it writes the folder.
func TestManagerVolumesSharedFolders(t *testing.T) {
	app := &config.App{Name: "m", Workers: []config.Worker{{Name: "w", Dir: "workers/w"}},
		SharedFolders: []config.SharedFolder{
			{Path: "tools/releases", Writers: []string{"w"}},
			{Path: "out", Writers: []string{"m"}, Readers: []string{"w"}},
		}}
	want := []scion.VolumeMount{
		{Source: "/lever/out", Target: "/workspace/out"}, // pinned: the manager writes it
		{Source: "/lever/tools", Target: "/workspace/tools"},
		{Source: "/lever/workers", Target: "/workspace/workers"},
		{Source: "/lever/tools/releases", Target: "/workspace/tools/releases", ReadOnly: true},
		{Source: "/lever/workers/w", Target: "/workspace/workers/w"},
		{Source: "/lever/tools/releases", Target: "/shared/releases", ReadOnly: true},
		{Source: "/lever/out", Target: "/shared/out"},
	}
	if got := managerVolumes("/lever", app); !reflect.DeepEqual(got, want) {
		t.Fatalf("volumes = %+v\nwant %+v", got, want)
	}
}

// TestRegisterPinsTheHubProjectBeforeTheLink: the re-init generates a new
// local project ID, and a hub link under the controller PAT refuses an ID the
// hub does not know (it cannot register a project), so the hub's record of the
// project is pinned in the tree's settings first. A name two hub projects
// share fails before the link rather than guess.
func TestRegisterPinsTheHubProjectBeforeTheLink(t *testing.T) {
	for _, tc := range []struct {
		name, projects string
		wantPin        string
		wantErr        bool
	}{
		{"hub knows the project", `[{"id": "hub-id-1", "name": "lever"}, {"id": "x", "name": "other"}]`, "hub-id-1", false},
		{"hub has no record yet", `[{"id": "x", "name": "other"}]`, "", false},
		{"two projects of that name", `[{"id": "a", "name": "lever"}, {"id": "b", "name": "lever"}]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := scionOKRunner()
			f.Script("scion hub projects", proc.Result{Stdout: tc.projects})
			app := helloApp(t.TempDir())
			err := runApply(app, Deps{JailMount: "/lever", Scion: hubScion(f, app)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run err = %v, wantErr %v", err, tc.wantErr)
			}
			pin := f.CallIndex(proc.ArgvPrefix("scion", "config", "set", "--non-interactive", "hub.projectId"))
			link := f.CallIndex(proc.ArgvPrefix("scion", "hub", "link"))
			switch {
			case tc.wantErr:
				testutil.WantErrContaining(t, err, "will not guess")
				if pin >= 0 || link >= 0 {
					t.Fatalf("pin at %d, link at %d; want neither after a failed lookup", pin, link)
				}
			case tc.wantPin == "":
				if pin >= 0 || link < 0 {
					t.Fatalf("pin at %d, link at %d; want no pin and the link", pin, link)
				}
			default:
				if pin < 0 || link < 0 || pin > link {
					t.Fatalf("pin at %d, link at %d; want the pin before the link", pin, link)
				}
				c := f.Calls[pin]
				if c.Args[len(c.Args)-1] != tc.wantPin || c.Dir != "/lever" {
					t.Fatalf("pin call %+v, want hub.projectId %s set in /lever", c, tc.wantPin)
				}
			}
		})
	}
}

// unconfirmedDeleteRunner answers `scion delete` the way scion does for a
// delete the hub accepted but its wait never saw finish: exit 0 and a notice.
// gone decides whether the record has in fact gone by the next list.
type unconfirmedDeleteRunner struct {
	*agentLifecycleRunner
	gone bool
}

func (r *unconfirmedDeleteRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) > 0 && args[0] == "delete" {
		r.deleteCalls++
		r.record(dir, env, name, args)
		if r.gone {
			r.phase, r.containerStatus = "", ""
		}
		return proc.Result{Stdout: "Agent 'hello': delete accepted; cannot observe completion; local worktree kept (still running when the wait ended).\n"}, nil
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *unconfirmedDeleteRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestStartManagerFreshAfterAnUnconfirmedDelete: scion exits 0 for a delete
// it could not confirm. The record decides: gone, the fresh manager is
// created; still there, nothing is created and the error says the hub may
// still be deleting it.
func TestStartManagerFreshAfterAnUnconfirmedDelete(t *testing.T) {
	for _, gone := range []bool{true, false} {
		app, f := newObserveFirstApp(t)
		r := &unconfirmedDeleteRunner{agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
			initPhase: "suspended", initContainerStatus: "stopped"}, gone: gone}
		err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})})
		if gone {
			if err != nil || r.startCalls != 1 {
				t.Fatalf("gone: err = %v, starts = %d; want the create", err, r.startCalls)
			}
			continue
		}
		testutil.WantErrContaining(t, err, "the hub accepted the delete but the manager record is still there")
		testutil.WantErrContaining(t, err, "lever up --fresh")
		if r.startCalls != 0 {
			t.Fatalf("still there: %d start(s); nothing may be created over the record", r.startCalls)
		}
	}
}

// A manager an older lever created keeps its unqualified image name, which
// the hub's image registry still applies to: keeping or resuming it warns.
func TestActUpWarnsOnAnUnqualifiedImage(t *testing.T) {
	for _, c := range []struct {
		image string
		warn  bool
	}{
		{"scionlocal/lever-claude:arm64", true},
		{"localhost/scionlocal/lever-claude:arm64", true},
		{"lever.local/lever-claude:arm64", false},
		{"", false},
	} {
		var logs []string
		r := &run{app: &config.App{Name: "m"}, d: Deps{Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}}
		rec := &scion.Agent{Slug: "m", Phase: scion.PhaseRunning, Image: c.image}
		if err := r.actUp(context.Background(), "/lever", rec, managerPlan{verb: verbNone}, scion.StartOpts{Image: "lever.local/lever-claude:arm64"}); err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(strings.Join(logs, "\n"), "unqualified image name")
		if got != c.warn {
			t.Errorf("%q: warned = %v, want %v (%q)", c.image, got, c.warn, logs)
		}
	}
}
