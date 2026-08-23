package apply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// runApply runs the full plan over deps completed by fillDeps.
func runApply(app *config.App, deps Deps) error {
	return Run(context.Background(), app, fillDeps(deps), PlanOpts{})
}

// fillDeps completes d with an inert implementation of every required
// collaborator the test left nil, so a test names only what it asserts on.
// The inert mint returns fresh material (boot.minted = true) and stages it
// into the app tree exactly as the real mint step would; a test that models a
// spent latch wires spentLatchMint instead.
func fillDeps(d Deps) Deps {
	if d.LoadImage == nil {
		d.LoadImage = func(context.Context, string) error { return nil }
	}
	if d.ImageLoaded == nil {
		d.ImageLoaded = func(context.Context, string) bool { return false }
	}
	if d.PruneImages == nil {
		d.PruneImages = func(context.Context) error { return nil }
	}
	if d.StartBroker == nil {
		d.StartBroker = func(context.Context) error { return nil }
	}
	if d.BrokerHealthy == nil {
		d.BrokerHealthy = func(context.Context) error { return nil }
	}
	if d.EnsureControllerPAT == nil {
		d.EnsureControllerPAT = func(context.Context) error { return nil }
	}
	if d.WaitBrokerReady == nil {
		d.WaitBrokerReady = func(context.Context, string) error { return nil }
	}
	if d.MintManagerBootstrap == nil {
		d.MintManagerBootstrap = func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{Ticket: "minted-this-run"}, nil
		}
	}
	if d.RearmBootstrap == nil {
		d.RearmBootstrap = func(context.Context) error { return nil }
	}
	if d.EnsureHubLogin == nil {
		d.EnsureHubLogin = func(context.Context) (bool, error) { return false, nil }
	}
	if d.DisableHubLogin == nil {
		d.DisableHubLogin = func(context.Context) (bool, error) { return false, nil }
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
	if d.RemoveJailFile == nil {
		d.RemoveJailFile = func(context.Context, string) error { return nil }
	}
	if d.RemoveScionProjectConfigs == nil {
		d.RemoveScionProjectConfigs = func(context.Context, string) error { return nil }
	}
	if d.ScionProjectRegistered == nil {
		d.ScionProjectRegistered = func(context.Context, string) (bool, error) { return false, nil }
	}
	if d.StripProjectSharedDirs == nil {
		d.StripProjectSharedDirs = func(context.Context, string) error { return nil }
	}
	if d.RepairScionHubEndpoint == nil {
		d.RepairScionHubEndpoint = func(context.Context, string) error { return nil }
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

// isObserveList reports whether args is scion.Client.List's shape (`list
// --format json ... --non-interactive`), as opposed to waitHubReady's
// project-independent liveness probe (`list --all --format json`, no
// --non-interactive — see internal/scion/bringup.go). Both start with the
// literal verb "list", so the fakes below must distinguish them: the hub-
// ready probe fires once per apply during the scion-server step, BEFORE
// start-manager's real observe-first List even runs, and must be left to
// fall through to the plain blanket "ok" script rather than being consumed by
// (or corrupting the state of) the observe-first fakes.
func isObserveList(args []string) bool {
	if len(args) == 0 || args[0] != "list" {
		return false
	}
	for _, a := range args {
		if a == "--non-interactive" {
			return true
		}
	}
	return false
}

// flakyStartRunner fails the first startFails `scion start` calls with the
// runtime-broker-unavailable error (the registration race), then defers to the
// wrapped FakeRunner. Used to prove start-manager retries. It also answers
// `scion list --format json` itself (the observe-first start-manager
// calls List before AND after Start): the very first (observe-first) list call
// reports slug ABSENT (so the create path — and thus Start — is actually
// taken), and every call after that reports it running/running (so the
// post-start liveness verify converges as soon as Start finally succeeds).
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
			// (internal/scion Client.roleFlagSupported), not an agent start.
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
// start` and agent `start` return "already running"; everything else
// succeeds. It also answers `scion list --format json` itself (the
// observe-first start-manager calls List before AND after Start): the first
// list call reports slug ABSENT (so start-manager still takes the create path
// and calls Start, exercising the AlreadyRunning tolerance below), and every
// call after that reports it running/running (so the post-start liveness
// verify converges once AlreadyRunning is tolerated as success).
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

// agentLifecycleRunner fakes `scion list/start/resume/delete` around a SINGLE
// manager-agent record, for the observe-first start-manager tests. list
// reports whatever the record's current phase/containerStatus is (a ""
// initPhase reports no agents at all, i.e. absent); a successful start/resume
// advances the record to liveWhenPhase/liveWhenContainer (both default to
// "running" when left zero, so most tests need not set them); delete clears
// the record. resumeErr/deleteErr/startErr, when set, make the corresponding
// verb fail (leaving the record unchanged) instead of advancing it. Falls
// through to the wrapped FakeRunner (and its blanket f.Script("scion", ...))
// for every other scion verb (init/hub/config/server/etc.), and records every
// intercepted call into f.Calls too, so tests that scan f.Calls for a call's
// exact argv (the manager prompt, --harness-auth, --workspace, ...) keep
// working whether or not this fake happens to intercept that particular verb.
//
// The zero value (only slug set) models the "nothing running yet" steady
// state most pre-existing apply tests need: the very first list reads absent,
// so start-manager still takes its normal create-then-verify path (Start's
// argv is still exercised and recorded), and the post-create liveness poll
// converges on the very next list call.
type agentLifecycleRunner struct {
	*proc.FakeRunner
	slug string

	initPhase, initContainerStatus   string
	liveWhenPhase, liveWhenContainer string

	resumeErr, deleteErr, startErr, listErr error
	// resumeFailsThenSucceed, when > 0, makes resume fail (with resumeErr) for
	// exactly this many calls, then succeed from the next call onward — models
	// a transient broker-unavailable race resolving mid-retry. Zero (the
	// default) preserves the original all-tests-so-far behavior: if resumeErr
	// is set, resume fails EVERY call (no eventual success).
	resumeFailsThenSucceed int
	// listFailsThenSucceed, when > 0, makes the observe List fail (with listErr)
	// for exactly this many calls, then succeed — models the runtime-broker race
	// biting the FIRST hub call of start-manager. Only the observe-first List is
	// affected (waitHubReady's `list --all` falls through verb() to the blanket
	// script). Zero with listErr set = fail every call.
	listFailsThenSucceed int
	// healerRecoversOnResumeFail, when true, advances the record to live even
	// though resume FAILS — modelling the broker daemon's auto-re-enrol healer
	// (#22) bouncing the same record concurrently: apply's own verb errors, but
	// the agent comes up anyway.
	healerRecoversOnResumeFail bool

	phase, containerStatus string
	inited                 bool

	startCalls, resumeCalls, deleteCalls, listCalls int
	resumeForceCalls                                int // subset of resumeCalls carrying --force
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

// verb extracts the scion subcommand from args, skipping the leading `-g
// <project>` pair Start puts first (see scion.Client.Start) — list/resume/
// delete all put their verb at args[0] directly (see scion.Client.List /
// Resume / Delete).
func (r *agentLifecycleRunner) verb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	// `start --help` is the agent-role capability probe (internal/scion
	// Client.roleFlagSupported), not an agent start. It must not be counted as
	// one, nor mutate this fake's tracked record.
	if len(args) == 2 && args[0] == "start" && args[1] == "--help" {
		return "role-probe"
	}
	if args[0] == "-g" && len(args) > 2 {
		return args[2]
	}
	if args[0] == "list" && !isObserveList(args) {
		// waitHubReady's project-independent `list --all --format json` probe
		// (see isObserveList) — not our observe-first List; let it fall
		// through to the blanket "ok" script instead of being treated as (or
		// mutating the state of) this fake's single tracked record.
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
	case "delete":
		r.deleteCalls++
		r.record(dir, env, name, args)
		if r.deleteErr != nil {
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

// newObserveFirstApp returns a minimal app + fresh FakeRunner for the
// start-manager observe-first tests, sharing one shape across the matrix
// (name "hello", matching agentLifecycleRunner's slug in each test below).
func newObserveFirstApp(t *testing.T) (*config.App, *proc.FakeRunner) {
	t.Helper()
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	return app, f
}

// TestStartManagerObserveFirstCreatesWhenAbsent: no existing record -> Start
// is called (the create path), Resume/Delete are not, and the post-start
// liveness verify (seeing the fake's default running/running once Start
// succeeds) is what lets Run return nil.
func TestStartManagerObserveFirstCreatesWhenAbsent(t *testing.T) {
	app, f := newObserveFirstApp(t)
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
}

// TestStartManagerObserveFirstResumesSuspended: a suspended record must be
// RESUMED (conversation restored), never blind-Started (which would 409 and,
// pre-Task-4, falsely succeed).
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
// records too ("scion resume help: 'Resume a
// stopped scion agent' — covers stopped as well as suspended").
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

// TestStartManagerObserveFirstNoOpWhenRunning: an already-running, actually-
// live record needs neither Start nor Resume — the liveness verify sees it
// green on the very first poll.
func TestStartManagerObserveFirstNoOpWhenRunning(t *testing.T) {
	app, f := newObserveFirstApp(t)
	// containerStatus carries podman's live status TEXT ("Up 6 seconds"), not
	// a canonical "running" — the real-world shape (live-observed 2026-07-04);
	// this fixture pins that the liveness gate accepts it.
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
	setRetryBudget(t, &managerLiveAttempts, &managerLiveInterval, 3)

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "stopped"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, `phase "running"`, `container "stopped"`)
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (the no-op branch must not start/resume)", r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerResumeFailsRecoversFresh: when Resume cannot restore the
// conversation, start-manager must log the loss LOUDLY (Deps.Log), then
// Delete the orphaned record and create a fresh manager — never fail the
// whole apply just because the OLD session is unrecoverable. The failure here
// is NON-transient ("agent does not exist" — not the broker-unavailable
// wording), so this also pins C1's other half: a non-transient resume error
// must recover immediately (resumeCalls == 1), never burning the broker-race
// retry budget on an error retrying could never fix.
func TestStartManagerResumeFailsRecoversFresh(t *testing.T) {
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
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run should recover from a failed resume by starting fresh: %v", err)
	}
	if r.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", r.resumeCalls)
	}
	if r.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1 (clearing the unresumable record)", r.deleteCalls)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (fresh create after the failed resume)", r.startCalls)
	}
	if len(logged.lines) != 1 {
		t.Fatalf("expected exactly one loud log line, got %+v", logged.lines)
	}
	if !strings.Contains(logged.lines[0], "resume failed") || !strings.Contains(logged.lines[0], "FRESH") || !strings.Contains(logged.lines[0], "previous session lost") {
		t.Fatalf("recovery log line missing expected wording, got %q", logged.lines[0])
	}
}

// TestStartManagerResumeFailsAndDeleteFailsReturnsError: if the record can be
// neither resumed NOR deleted, start-manager must surface a hard error naming
// BOTH failures — there is no safe fallback (a fresh Start over an
// undeleted, un-resumable record would just 409 again).
func TestStartManagerResumeFailsAndDeleteFailsReturnsError(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent 'hello': agent does not exist"),
		deleteErr: fmt.Errorf("delete: agent locked"),
	}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, "cannot resume", "delete: agent locked")
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (must not attempt a fresh create over an undeleted record)", r.startCalls)
	}
}

// TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds proves the
// CRITICAL fix: `scion resume` shares Start's
// runtime-broker-registration race, so a resume that fails with the
// broker-unavailable wording must be RETRIED (same brokerStartAttempts/
// brokerStartInterval budget as Start) before any loud recovery — a transient
// blip must never destroy a resumable conversation.
// TestStartManagerErrorPhaseForcedResumeFailsAndDeleteFailsReturnsError pins
// the ERROR branch's distinct delete-fail error wrap ("forced resume failed
// (%v) and delete failed: %w"), which is worded differently from the
// suspended branch's ("resume failed (%v) and delete failed") — an
// error-phase record whose `resume --force` fails AND whose delete then fails
// must surface a hard error naming BOTH failures and must not attempt a fresh
// create over the undeleted record.
func TestStartManagerErrorPhaseForcedResumeFailsAndDeleteFailsReturnsError(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "error", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("forced resume: container state corrupt"),
		deleteErr: fmt.Errorf("delete: agent locked"),
	}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, "forced resume failed")
	wantErrContaining(t, err, "container state corrupt", "delete: agent locked")
	if r.resumeForceCalls != 1 {
		t.Errorf("resumeForceCalls = %d, want 1 (error phase must TRY resume --force first)", r.resumeForceCalls)
	}
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (must not attempt a fresh create over an undeleted record)", r.startCalls)
	}
}

func TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds(t *testing.T) {
	setRetryBudget(t, &brokerStartAttempts, &brokerStartInterval, 5)

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		// scion resume's own transient wording (singular "broker" — see
		// scion.IsBrokerUnavailable's doc), failing the first 2 calls then resolving.
		resumeErr:              fmt.Errorf("cannot resume agent: no runtime broker available"),
		resumeFailsThenSucceed: 2,
	}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
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

// TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRecovers is C1's
// complement: if the broker-unavailable race NEVER resolves within the retry
// budget, start-manager must still fall back to the loud delete+fresh
// recovery — the retry absorbs a transient blip, it does not turn a
// permanently-unavailable broker into an infinite hang.
func TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRecovers(t *testing.T) {
	setRetryBudget(t, &brokerStartAttempts, &brokerStartInterval, 3)

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent: no runtime broker available"),
		// resumeFailsThenSucceed left 0: resume fails on EVERY call, exercising
		// full budget exhaustion.
	}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("an exhausted-but-transient resume must still recover fresh, not fail the apply: %v", err)
	}
	if r.resumeCalls != 3 {
		t.Fatalf("resumeCalls = %d, want 3 (the full retry budget burned)", r.resumeCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1 (loud recovery only AFTER the retry budget exhausts)", r.deleteCalls, r.startCalls)
	}
	if len(logged.lines) != 1 || !strings.Contains(logged.lines[0], "resume failed") || !strings.Contains(logged.lines[0], "FRESH") {
		t.Fatalf("expected exactly one loud recovery log line, got %+v", logged.lines)
	}
}

// TestIsBrokerUnavailable pins the transient runtime-broker wordings that must
// be retried during start-manager — including the cold-VM "deadline exceeded"
// surfacing that previously slipped through and forced a second `up`.
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

// TestStartManagerObserveListRetriesOnTransientThenSucceeds: the observe-first
// List is the FIRST hub call of start-manager, so on a cold VM it can hit the
// runtime-broker registration window and come back "deadline exceeded". It must
// ride the same bounded retry as Start/Resume, not fail the whole apply on the
// first blip (which is what forced a second `up`). Fail the List twice, then
// succeed onto an already-running record (pure no-op path) so the test isolates
// the observe retry.
func TestStartManagerObserveListRetriesOnTransientThenSucceeds(t *testing.T) {
	setRetryBudget(t, &brokerStartAttempts, &brokerStartInterval, 5)

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "running", initContainerStatus: "Up 3 seconds",
		listErr:              fmt.Errorf("Get \"http://hub/agents\": context deadline exceeded"),
		listFailsThenSucceed: 2,
	}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("a transient observe-List blip must be retried, not fail the apply: %v", err)
	}
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (already-running record is a no-op once observed)", r.startCalls, r.resumeCalls)
	}
	// 2 transient failures + at least one success before the liveness poll.
	if r.listCalls < 3 {
		t.Errorf("listCalls = %d, want >= 3 (2 retried failures + success)", r.listCalls)
	}
}

// TestStartManagerWaitsForBrokerReadyBeforeActing: the readiness gate must run
// BEFORE the observe/create — that's the whole point of gating rather than
// racing the broker's async registration.
func TestStartManagerWaitsForBrokerReadyBeforeActing(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent record → create
	var waitCalls, startCallsAtGate int
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		WaitBrokerReady: func(ctx context.Context, project string) error {
			waitCalls++
			startCallsAtGate = r.startCalls
			return nil
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

// TestStartManagerBrokerReadyErrorAbortsBeforeActing: a gate error (e.g. ctx
// cancellation — the gate's only non-nil return, since it is otherwise
// fail-soft) aborts start-manager before it touches any record.
func TestStartManagerBrokerReadyErrorAbortsBeforeActing(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		WaitBrokerReady: func(ctx context.Context, project string) error {
			return context.Canceled
		},
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, "runtime broker")
	if r.startCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("startCalls=%d resumeCalls=%d, want 0/0 (no action when the gate errors)", r.startCalls, r.resumeCalls)
	}
}

// TestStartManagerLivenessNeverGreenAfterCreate: `scion start` reports success
// but the container never actually comes up (scion's own false-success).
// The liveness verify must exhaust its attempts and
// fail loudly with the last observed phase/container, rather than trusting
// the CLI's exit code.
func TestStartManagerLivenessNeverGreenAfterCreate(t *testing.T) {
	setRetryBudget(t, &managerLiveAttempts, &managerLiveInterval, 3)

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		liveWhenContainer: "stopped", // Start "succeeds" but the container never lives
	}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, "did not come up", `container "stopped"`)
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (Start itself must still have been attempted)", r.startCalls)
	}
}

// TestStartManagerUnexpectedPhaseRecoversFresh proves the IMPORTANT fix:
// an unhandled-but-real scion phase (here
// "error" — a crashed manager, e.g. an OOM/harness crash) must NOT hard-fail
// (brick) `lever up` with no path forward but `lever destroy`. It takes the
// SAME loud delete+fresh recovery as a failed resume, so `up` converges.
func TestStartManagerUnexpectedPhaseRecoversFresh(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("container state corrupt")}
	var logged logSink
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   logged.logf,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("an unrecoverable error phase must recover fresh, not hard-fail the apply: %v", err)
	}
	if r.resumeForceCalls != 1 {
		t.Errorf("resumeForceCalls = %d, want 1 (error phase must TRY resume --force before discarding)", r.resumeForceCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1 (delete the unrecoverable record, then fresh create)", r.deleteCalls, r.startCalls)
	}
	if len(logged.lines) != 1 {
		t.Fatalf("expected exactly one loud recovery log line, got %+v", logged.lines)
	}
	if !strings.Contains(logged.lines[0], `phase "error"`) || !strings.Contains(logged.lines[0], "FRESH") || !strings.Contains(logged.lines[0], "previous session lost") {
		t.Fatalf("recovery log line missing expected wording, got %q", logged.lines[0])
	}
}

// TestStartManagerErrorPhaseForcedResumeRecovers (#3): an error-phase record
// is first re-ticketed and `resume --force`d (scion#895); when that succeeds
// the conversation SURVIVES — no delete, no fresh create, no loud loss notice.
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

// TestStartManagerResumeFailButHealerRecovered: apply's resume/resume --force
// fails, but re-observation shows the record RUNNING — the broker daemon's
// auto-re-enrol healer (a separate process, started by broker-up before this
// step) bounced the same record concurrently. Apply must NOT delete+recreate:
// that would destroy the conversation the healer just restored. Covers the
// apply-vs-healer race from the recovery-arc review (finding 1).
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

// TestStartManagerStartingPhaseRecoversFresh covers the OTHER unhandled-phase
// shape: a "starting" record left behind by a `lever up` that was interrupted
// mid-`scion start` on a prior run. WHY this also takes the loud
// delete+fresh path rather than something smarter: `scion resume` is
// documented for suspended/stopped records only (there is no verb to "finish
// starting" or safely probe whether a half-started record is salvageable),
// and `scion list --format json`'s phase field is the canonical state we
// observe — we cannot be cleverer without scion exposing more verbs. So a
// half-started record gets the same safe-floor recovery as any other
// unhandled phase, converging `up` instead of bricking it.
func TestStartManagerStartingPhaseRecoversFresh(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "starting", initContainerStatus: ""}
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("a half-started (\"starting\") record must recover fresh, not hard-fail: %v", err)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1", r.deleteCalls, r.startCalls)
	}
	if r.resumeCalls != 0 {
		t.Errorf("resumeCalls = %d, want 0 (a starting record is not resumable)", r.resumeCalls)
	}
}

// --- RearmBootstrap (fix/rearm-bootstrap-on-create) ---
//
// A freshly-created scion agent record has no agent home to reuse (unlike
// resume, which restores the existing one), so lever-agent boot ALWAYS
// re-enrols after a create. If mint-manager-bootstrap tolerated a spent
// /bootstrap latch (idempotent re-apply against the same broker process — see
// ErrBootstrapLatched), a create-path Start is guaranteed to 403. The tests
// below pin start-manager's fix: the shared create helper re-arms (restarts
// the broker, re-mints, re-stages) whenever no fresh material was minted
// earlier in THIS apply run, across every path that can reach a create (the
// absent-record branch, and both post-delete recovery branches), and never
// re-arms when it isn't needed (a fresh mint already happened this run, or
// the branch taken is resume/no-op, which never creates at all).

// TestStartManagerCreateRearmsSpentLatchWhenNoFreshMintThisRun: the plain
// absent-record create path after a tolerated spent latch (spentLatchMint, so
// boot.minted stays false) must call RearmBootstrap exactly once and then proceed with Start.
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

// TestStartManagerCreateSkipsRearmWhenFreshMintAlreadyHappened: when
// mint-manager-bootstrap actually minted fresh material this run (no latch
// to tolerate), the create path already has enrolable material — RearmBootstrap
// must NOT be called even though it's wired.
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

// TestStartManagerRecoveryRearmsBeforeFreshCreate: the post-recovery-delete
// create path (a non-transient resume failure -> delete -> fresh create) must
// ALSO re-arm before its Start — it takes the identical startManagerCreate
// helper as the absent-record branch, so it must get the identical guarantee.
func TestStartManagerRecoveryRearmsBeforeFreshCreate(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent 'hello': agent does not exist"),
	}
	rearmCalls := 0
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		Log:                  func(string, ...any) {},
		RearmBootstrap:       countRearm(&rearmCalls),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 1 {
		t.Errorf("RearmBootstrap calls = %d, want 1 (the post-recovery-delete create must re-arm too)", rearmCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1", r.deleteCalls, r.startCalls)
	}
}

// TestStartManagerResumeRearmsWhenNoFreshMaterial: resume must self-heal a
// possibly-expired leaf. When this run has NOT minted fresh bootstrap material
// (no open latch — the broker outlived a spent latch across the manager's
// downtime, the expired-leaf scenario), the resume path re-arms and stages a
// fresh enrolment ticket so lever-agent boot can re-enrol the dead leaf. An
// existing agent home is NOT proof the leaf is still valid — an expired leaf is
// exactly the outage this closes.
func TestStartManagerResumeRearmsWhenNoFreshMaterial(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	rearmCalls := 0
	deps := Deps{
		MintManagerBootstrap: spentLatchMint(t, app.Tree),
		Scion:                scion.New(r, scion.Options{}),
		// A spent latch -> no fresh material minted this run (boot.minted
		// stays false), modelling the persisted-broker state in which an
		// expired leaf would otherwise stay dead.
		RearmBootstrap: countRearm(&rearmCalls),
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

// TestStartManagerResumeSkipsRearmWhenAlreadyMinted: the normal stop→up path
// (broker-up reopened the latch, mint-manager-bootstrap already staged a fresh
// ticket this run) must NOT re-arm again on resume — no needless second broker
// bounce. ensureFreshBootstrap is a no-op once boot.minted is set.
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

// TestStartManagerNoOpRunningNeverRearms: an already-running, actually-live
// record is a pure no-op — it never reaches the create path, so
// RearmBootstrap must never be called.
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
// bootstrap material is guaranteed to 403 (crash-loop the container), so a
// RearmBootstrap failure must hard-fail the step — naming bootstrap/latch —
// rather than let Start run anyway.
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
	wantErrContaining(t, err, "bootstrap", "latch")
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (Start must never be attempted without enrolable bootstrap material)", r.startCalls)
	}
}

// TestStartManagerCreateProceedsWithoutRearmWhenNilBackCompat: nil
// RearmBootstrap (every pre-fix test, and the broker-only acceptance gate,
// which never reaches start-manager at all) must leave the create path
// exactly as it behaved before this fix — Start proceeds unguarded.
func TestStartManagerCreateProceedsWithoutRearmWhenNilBackCompat(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // absent -> create path
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		// RearmBootstrap intentionally left nil.
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (nil RearmBootstrap must not block the create path)", r.startCalls)
	}
}

// TestStartManagerObserveListErrorIsHardFailure: the hub is already up by the
// time start-manager runs (scion-server precedes it in Plan()), so a List
// error observing agents is real, not a "hub not ready yet" race — it must
// fail the step outright.
func TestStartManagerObserveListErrorIsHardFailure(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", startErr: nil}
	// Force the FIRST list call (the observe) to error by wrapping further:
	// simplest is a tiny extra fake that fails exactly the first `list` call
	// and otherwise defers to r.
	fe := &firstListErrRunner{inner: r}
	deps := Deps{
		Scion: scion.New(fe, scion.Options{}),
	}
	err := runApply(app, deps)
	wantErrContaining(t, err, "observing agents")
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (must not guess an action past an observe failure)", r.startCalls)
	}
}

// firstListErrRunner fails exactly the first observe-first `scion list` call
// (see isObserveList — this must NOT be waitHubReady's earlier, unrelated
// `list --all` probe during the scion-server step) with a synthetic hub
// error, then defers every call (including later list calls) to inner. Used
// to prove start-manager's initial observe treats a List error as a hard
// failure.
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

// failNListsRunner fails a WINDOW of "scion list" (observe-first shape) calls
// with a transient error — specifically the calls numbered 2..1+failCount
// (call 1, the very first observe-first List, is left untouched so the
// pre-action observe still succeeds normally) — then defers every other call
// to inner. Used to prove waitManagerLive's post-action liveness poll
// tolerates a mid-poll List blip: it must consume the failed attempt and keep
// polling within the remaining budget, not abort the whole apply immediately.
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

// TestWaitManagerLiveToleratesMidPollListErrors proves the MINOR fix:
// two transient List errors during the
// post-action liveness poll must not abort the apply — they are consumed
// within the existing retry budget, and the poll succeeds as soon as a List
// call reports the manager running/running.
func TestWaitManagerLiveToleratesMidPollListErrors(t *testing.T) {
	setRetryBudget(t, &managerLiveAttempts, &managerLiveInterval, 5)

	app, f := newObserveFirstApp(t)
	// Already running/live — the no-op branch — so start-manager's OWN observe
	// (list call 1) must succeed; failNListsRunner then fails exactly the next
	// two list calls (2 and 3, both inside waitManagerLive's poll) before
	// deferring back to a live running/running record on call 4.
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "running"}
	fe := &failNListsRunner{inner: r, failCount: 2}
	deps := Deps{
		Scion: scion.New(fe, scion.Options{}),
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
	// latch on this re-apply is tolerable (the manager has what it needs).
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
		// Same broker process as a prior apply ⇒ latch spent ⇒ ErrBootstrapLatched.
		// The mint step must tolerate it (the manager already has its bootstrap).
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

// TestRunMintBootstrapPropagatesRealError: a non-latch mint error (e.g. the
// broker is down) must NOT be swallowed.
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

// TestRunLatchedWithoutStagedBootstrapFails: a spent latch with NO staged
// bootstrap ticket means a stale broker is being reused (its latch was consumed
// by an earlier run, but this tree has no ticket). The new manager could never
// enrol, so the mint step must fail loudly and point at `lever down`, rather than
// silently boot a doomed manager.
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
	wantErrContaining(t, err, "lever down")
}

func TestStartManagerRetriesOnBrokerUnavailable(t *testing.T) {
	// Make the retry fast for the test.
	setRetryBudget(t, &brokerStartAttempts, &brokerStartInterval, 5)

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
		Scion: scion.New(r, scion.Options{}),
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
		Manager: config.Manager{Image: "scionlocal/lever-claude:latest"},
		Workers: []config.Worker{{Name: "worker", Dir: "workers/worker"}},
	}
	var loadImg bool
	deps := Deps{
		LoadImage: func(_ context.Context, ref string) error {
			loadImg = (ref == "scionlocal/lever-claude:latest")
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
	for _, want := range []string{"init --machine", "config set --global image_registry scionlocal", "server start", "init --non-interactive", "hub link", "start hello"} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing scion call %q in: %q", want, j)
		}
	}
}

// serverStartOrderRunner wraps an agentLifecycleRunner (so `scion list`
// keeps reporting a real, eventually-live manager record — required for
// start-manager's liveness verify to converge) and appends "server-start" to
// *order the moment it sees a `scion server start` call — used alongside a
// Deps.EnsureControllerPAT closure that appends "bootstrap-token" to the same
// slice, so a test can assert relative ordering between an injected Deps func
// (which has no argv of its own to scan for) and a real scion call.
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

// TestRunBootstrapTokenRunsOnceBeforeScionServer pins the bootstrap-token
// step's executor wiring: a fake EnsureControllerPAT must be invoked
// exactly once, and strictly before the `scion server start` call — the
// controller PAT must exist before the real, dev-auth-off hub locks down.
func TestRunBootstrapTokenRunsOnceBeforeScionServer(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	var order []string
	alr := &agentLifecycleRunner{FakeRunner: f, slug: app.Name}
	sr := &serverStartOrderRunner{agentLifecycleRunner: alr, order: &order}
	ensureCalls := 0
	deps := Deps{
		Scion: scion.New(sr, scion.Options{}),
		EnsureControllerPAT: func(context.Context) error {
			ensureCalls++
			order = append(order, "bootstrap-token")
			return nil
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ensureCalls != 1 {
		t.Fatalf("EnsureControllerPAT calls = %d, want 1", ensureCalls)
	}
	btIdx, serverIdx := -1, -1
	for i, v := range order {
		if v == "bootstrap-token" && btIdx < 0 {
			btIdx = i
		}
		if v == "server-start" && serverIdx < 0 {
			serverIdx = i
		}
	}
	if btIdx < 0 || serverIdx < 0 {
		t.Fatalf("expected both bootstrap-token and server-start recorded; order=%v", order)
	}
	if !(btIdx < serverIdx) {
		t.Fatalf("bootstrap-token must run before scion-server; order=%v", order)
	}
}

// TestRunBootstrapTokenSkipsCleanlyWhenNil proves the dev-auth-open/legacy
// fallback: a nil Deps.EnsureControllerPAT (every pre-Task-4 test, and any
// caller that doesn't wire the mint window) must not error and must not
// block the rest of Run — the scion-server step still runs unguarded.
func TestRunBootstrapTokenSkipsCleanlyWhenNil(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
		// EnsureControllerPAT intentionally left nil.
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
		t.Fatalf("scion-server must still run when EnsureControllerPAT is nil; calls=%+v", f.Calls)
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
	// plaintext: scion stores the argument verbatim since ce96122c.
	if want := "hub env set --secret --always CLAUDE_CODE_OAUTH_TOKEN sk-ant-raw"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

// TestRunScionServerEmitsWebFlagsWhenRemoteEnabled pins the scion-server
// step's threading of App.RemoteEnabled() into scion.ServerOpts:
// with remote access configured on, the real hub's `server start` call must
// carry --enable-web. It must NOT carry the tailnet base_url; the
// consequence of that is asserted by the agent-endpoint test below.
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

// TestRunScionServerEmitsSessionSecret pins the scion-server step's threading
// of Deps.HubSessionSecret into the real hub's argv: the equals-form flag with
// the exact value, so the hub signs session cookies with the host-persisted
// key instead of a per-boot random one.
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

// TestRunScionServerPointsTheHubAtStagedAssets is the end of the staged-assets wire:
// a remote-enabled instance on a `version:` pin has no embedded SPA, so the
// scion-server step must point the hub at the directory the backend staged.
// The path is layout.WebAssetsDir at both ends by construction — this pins
// that the step actually emits it.
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

// Binary mode builds no assets (no scion source to build them from), so the
// flag must stay off: a non-empty value would make scion serve an empty
// directory INSTEAD of whatever the operator embedded in their own binary.
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

// containerBridgeHost is the only host name that resolves to the machine's
// loopback from inside an agent's netns: podman answers
// host.containers.internal with 169.254.1.2, and lever's pasta
// --map-host-loopback drop-in points that address at the machine
// (internal/backend/guest/guest.go). An agents' hub endpoint naming anything
// else is unreachable from the jail.
const containerBridgeHost = "host.containers.internal"

// agentHubEndpoint models the endpoint scion injects into EVERY agent
// container as SCION_HUB_ENDPOINT/SCION_HUB_URL, given the `scion server
// start` argv lever emits. The argv is the only input lever controls, so
// this is what lets a test assert the outcome agents see instead of the
// flags lever passed — asserting the flags is precisely what let a tailnet
// base_url reach agents unnoticed.
//
// The load-bearing precondition is that lever runs scion in WORKSTATION mode:
// it passes neither --hosted nor --enable-hub, so applyWorkstationDefaults
// (cmd/server_config.go:25-41, called from both the daemon launcher and the
// foreground runner whenever !hostedMode) force-sets enableHub = true. That is
// what puts --base-url on the resolution path at all — resolveHubEndpoint's
// !enableHub arm returns the project-settings endpoint and never consults the
// flag. If lever ever starts passing --hosted, re-derive this whole model.
//
// Two scion rules then apply in order (read at pin 066eeba9):
//
//  1. cmd/server_foreground.go resolveHubEndpoint — --base-url wins
//     outright; with the flag absent a combo server falls back to
//     http://localhost:<web-port>. The other precedence arms (hub.endpoint
//     in the global config, SCION_SERVER_BASE_URL, project settings) are
//     all left unset by lever, and lever actively repairs the
//     project-recorded endpoint back to loopback (Deps.RepairScionHubEndpoint).
//  2. pkg/runtimebroker/hubenv.go applyContainerBridgeOverride — a LOOPBACK
//     endpoint is rewritten onto the container bridge host, keeping its
//     port. A non-loopback endpoint reaches the agent verbatim, with no
//     rewrite at all.
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

// TestRunScionServerKeepsAgentHubEndpointJailReachable is the regression
// test for the live bug where `--base-url <tailnet URL>` became the agents'
// SCION_HUB_ENDPOINT: podman inspect on a remote-enabled instance showed
// every agent carrying https://<name>.ts.net, a name the jail cannot
// resolve and lever's egress drops, breaking status updates, notifications
// and the ~10-hourly agent token refresh.
//
// It asserts the AGENT-FACING outcome rather than the argv, because the
// argv-shaped tests passed throughout the bug. Turning remote access on
// must not change what agents get: a container-bridge endpoint, exactly as
// a headless instance produces.
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

// TestRunScionServerOmitsWebFlagsWhenRemoteDisabled is the inverse of the
// above: with remote access left off (the default), the real hub's
// `server start` call must NOT gain --enable-web or --base-url — a headless
// hub must not serve the SPA.
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

// TestRunRemoteProxyStepInvokesStartWhenEnabled proves the remote-proxy
// step's executor wiring: Plan only includes the step when
// app.RemoteEnabled() (see plan_test.go), and run.step must invoke the
// injected Deps.StartRemoteProxy for it — exactly once, exactly like every
// other step's Deps func.
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

// TestRunRemoteProxyStepOrderedAfterScionServer pins the ordering
// requirement at the executor level (plan_test.go pins it at the Plan
// level): the proxy needs the hub up, so its start must be observed
// strictly after `scion server start` actually runs.
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

// TestRunRemoteProxyStepSkipsCleanlyWhenNil mirrors
// TestRunBootstrapTokenSkipsCleanlyWhenNil: a nil Deps.StartRemoteProxy (any
// caller that hasn't wired the proxy controller) must not error even though
// the step is present in the plan.
func TestRunRemoteProxyStepSkipsCleanlyWhenNil(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Remote = config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}
	deps := Deps{
		Scion: hubScion(f, app),
		// StartRemoteProxy intentionally left nil.
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run must not fail when StartRemoteProxy is nil: %v", err)
	}
}

// TestRunConvergesRemoteProxyOffWhenDisabled is the config-off idempotence
// case: Plan omits the remote-proxy step
// entirely when remote is disabled, so Run itself — not a step — must call
// Deps.StopRemoteProxy to converge a stale proxy (left running from a prior
// apply with remote enabled) to stopped.
func TestRunConvergesRemoteProxyOffWhenDisabled(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	// Remote left disabled (the zero value).
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

// TestRunDoesNotStopRemoteProxyWhenEnabled is the positive-case complement:
// when remote IS enabled, Run must never call StopRemoteProxy — only the
// remote-proxy step's StartRemoteProxy runs.
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

// TestRunConvergeOffSkipsCleanlyWhenNil proves the nil-safe default: a
// config that never enables remote, with StopRemoteProxy left unwired (every
// pre-remote-access test, and any caller that doesn't need the proxy), must
// not error.
func TestRunConvergeOffSkipsCleanlyWhenNil(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	deps := Deps{
		Scion: hubScion(f, app),
		// StopRemoteProxy intentionally left nil.
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
		if strings.Contains(j, "start hello") && strings.Contains(j, "Dispatch the worker to create HELLO.") {
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
		if argv == "hub env set --project --always LEVER_LLM_AUTH=api-key" {
			sawEnvSet = true
		}
		// SecretSet base64-encodes the value, so match on the verb + key, not value.
		if strings.HasPrefix(argv, "hub env set --secret --always ANTHROPIC_API_KEY ") {
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
	// A stale marker in the tree must be gone by the time `scion init` runs,
	// so init creates a fresh project (writing workspace_path) rather than
	// resolving the stale marker and skipping it.
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

// TestRegisterRemovesMarkerThroughJailWhenProvided: when Deps.RemoveJailFile is
// set, the register step must remove the stale marker THROUGH it (jail-
// absolute path), NOT rely on the host-side removeStaleMarker fallback. We
// prove "not relied on" by making the fake RemoveJailFile a no-op on the real
// host file: if the code still worked correctly (init ran, no error) while the
// host-side marker file is left physically in place, the host-side remove was
// not part of the path taken.
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
	// The host-side marker must still be there — proving the host-side
	// removeStaleMarker fallback was NOT exercised alongside RemoveJailFile.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("host marker should be untouched when RemoveJailFile handles removal, stat err=%v", err)
	}
}

// TestRegisterRemovesStaleScionProjectConfigsBeforeInit proves register-project
// calls Deps.RemoveScionProjectConfigs with the target's JAIL workspace path
// BEFORE `scion init` runs — the removal counterpart to the marker-removal
// race fix above. Without this, every apply mints a fresh
// ~/.scion/project-configs/<uuid> registration and the old ones accumulate
// (the `lever doctor` "duplicate registrations" finding). Workers configured
// alongside the manager must NOT trigger their own registration — there is
// exactly ONE register-project step, for the instance tree.
func TestRegisterRemovesStaleScionProjectConfigsBeforeInit(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	app.Workers = []config.Worker{{Name: "worker", Dir: "workers/worker"}}
	var removeCalls []string
	var initCalls []string
	// Ordering proof: at the moment the RemoveScionProjectConfigs call fires,
	// count how many `scion init --non-interactive` calls the SAME fake runner
	// has already recorded. Since FakeRunner appends to f.Calls synchronously
	// in call order, a count of 0 proves the removal ran before init.
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
	// The remove call must precede ANY init (count 0).
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

// TestRegisterToleratesNilRemoveScionProjectConfigs proves the Deps field is
// optional: leaving it nil (as every pre-existing Deps literal in this file
// does) must not crash Run, and `scion init` still runs.
func TestRegisterToleratesNilRemoveScionProjectConfigs(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		// RemoveScionProjectConfigs intentionally left nil.
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawScionCall(f, "init --non-interactive") {
		t.Fatal("scion init should still run when RemoveScionProjectConfigs is nil")
	}
}

// TestRegisterSkipsDestructivePathWhenAlreadyRegistered proves the idempotent-
// register gate: when Deps.ScionProjectRegistered reports the workspace is
// already validly registered, register-project must skip its destructive
// clean+init path ENTIRELY — no marker removal, no RemoveScionProjectConfigs,
// no `scion init`/`hub link`. This is the fix for the resume-orphaning bug: a
// suspended manager agent record's project linkage must survive a re-apply
// when nothing is actually wrong with the registration.
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

// TestRegisterStripsSharedDirsOnBothPaths pins scion#925 containment: the
// default `scratchpad` shared dir is mounted read-write into EVERY agent of a
// project, so register-project must strip it whether the registration was
// already sound (early return) or was just re-inited. Missing it on the
// already-registered path would leave every re-applied instance sharing a
// writable directory between the manager and every worker.
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
			// The hub knows the project by the workspace basename — the same
			// name ensureControllerPAT passes to `hub token create`.
			if len(stripCalls) != 1 || stripCalls[0] != "lever" {
				t.Fatalf("StripProjectSharedDirs calls = %+v, want [lever]", stripCalls)
			}
		})
	}
}

// TestRegisterFailsWhenSharedDirStripFails pins the fail-loud contract: a strip
// failure (a 403 from a PAT without project:update, say) must abort apply. The
// alternative is a manager and workers that silently share a writable
// directory while the operator believes they do not.
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
	wantErrIs(t, runApply(app, deps), errForbidden)
}

// TestRegisterRunsDestructivePathWhenNotRegistered pins the complement: when
// Deps.ScionProjectRegistered reports false, the full existing destructive
// path (marker removal, RemoveScionProjectConfigs, init, hub link) still runs
// exactly as it does today.
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

// TestRegisterFallsThroughToDestructivePathOnObserveError proves the fail-open
// contract: an error from Deps.ScionProjectRegistered must NOT become a hard
// apply failure — it falls through to the existing destructive path exactly
// like a `false` result would.
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
		// Deliberately returns ok=true ALONGSIDE an error, to prove the error
		// (not the ok value) governs the fall-through.
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

// TestRegisterToleratesNilScionProjectRegistered proves the Deps field is
// optional: leaving it nil (as every pre-existing Deps literal in this file
// does, before this task) must not crash Run, and `scion init` still runs.
func TestRegisterToleratesNilScionProjectRegistered(t *testing.T) {
	tree := t.TempDir()
	f := scionOKRunner()
	app := helloApp(tree)
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		// ScionProjectRegistered intentionally left nil.
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawScionCall(f, "init --non-interactive") {
		t.Fatal("scion init should still run when ScionProjectRegistered is nil")
	}
}

// TestRegisterUsesJailPaths proves register-project registers ONLY the
// instance tree (via its jail path) — even with workers configured, workers
// no longer get their own scion registration (that per-worker fan-out was
// dropped; see register-project's single-step doc).
func TestRegisterUsesJailPaths(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
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

// TestSingleProjectRegisterRunsOnceAcrossTwoWorkers is the apply-side
// single-project integration proof. Every register-project test above
// configures at most ONE worker, which cannot distinguish "registration
// collapsed to one per instance" from "one per worker that happens to equal
// one because there's only one worker". With TWO workers configured
// (workers/a, workers/b) the distinction is decisive: under the OLD
// register-manager + N*register-worker model this would drive 3 separate
// `scion init`/`hub link` calls (manager + worker a + worker b); the
// collapsed single-project model must still show exactly ONE, entirely
// against the single instance jail path "/lever" — proving the fan-out is
// truly gone, not coincidentally absent.
func TestSingleProjectRegisterRunsOnceAcrossTwoWorkers(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
	f := scionOKRunner()
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Workers: []config.Worker{
			{Name: "a", Dir: "workers/a"},
			{Name: "b", Dir: "workers/b"},
		},
	}

	// Point 1: Plan emits exactly ONE register-project step, targeting the
	// instance tree — never one per worker.
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

	// Point 2: driving the register step invokes Deps.ScionProjectRegistered,
	// `scion init`, and `scion hub link` each exactly ONCE, all against the
	// single instance jail path "/lever" (never workers/a or workers/b).
	var registeredCalls []string
	deps := Deps{
		JailMount: "/lever",
		Scion:     hubScion(f, app),
		ScionProjectRegistered: func(_ context.Context, wp string) (bool, error) {
			registeredCalls = append(registeredCalls, wp)
			return false, nil // force the destructive path so init/hub-link actually run
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
	tree := t.TempDir() // real dir so file-writing steps can write into it
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
		if strings.Contains(j, "start hello") {
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

// loadImageStep drives just the load-image case of run.step with the given deps.
func loadImageStep(d Deps) error {
	return (&run{app: &config.App{}, d: fillDeps(d)}).step(context.Background(), Step{Kind: "load-image", Target: "img"})
}

// TestRunStepUnknownKind pins the switch's default arm: a Step whose Kind is
// not a known StepKind is a hard error (never a silent no-op), and the message
// echoes the offending kind. This guards the dispatch table against a Plan that
// emits a kind run.step has no case for.
func TestRunStepUnknownKind(t *testing.T) {
	err := (&run{app: &config.App{}}).step(context.Background(), Step{Kind: "no-such-kind"})
	wantErrContaining(t, err, "no-such-kind")
}

// TestLoadImageStepSkipsWhenAlreadyLoaded: the whole point of the guard — when
// the jail already holds the exact image, neither re-import nor prune runs.
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

// TestLoadImageStepNilGuardLoads: a guard that never reports the image present
// (fillDeps's inert ImageLoaded) always loads.
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
	wantErrIs(t, loadImageStep(d), errBoom)
	if prunes != 0 {
		t.Errorf("PruneImages calls = %d, want 0 (no prune after a failed load)", prunes)
	}
}

// TestLoadImageStepPruneErrorIsNonFatal: the image loaded, so a prune failure
// is logged.lines, not returned.
func TestLoadImageStepPruneErrorIsNonFatal(t *testing.T) {
	d := Deps{
		ImageLoaded: func(context.Context, string) bool { return false },
		PruneImages: func(context.Context) error { return fmt.Errorf("prune boom") },
	}
	if err := loadImageStep(d); err != nil {
		t.Fatalf("runStep: a prune failure must be non-fatal, got %v", err)
	}
}

// errPreRoleRefusal is what VerifyAgentRole returns for a record created before
// scion#1089: it stores no role, and a roles-aware scion reads that as `full`.
var errPreRoleRefusal = fmt.Errorf("agent %q has no stored role", "hello")

// Errors the tests inject into a Deps func and expect back from the step,
// asserted with errors.Is rather than by wording.
var (
	errForbidden = errors.New("403 Forbidden")
	errBoom      = errors.New("boom")
)

// TestStartManagerRefusesPreRoleRecordOnResume: the guard must stop the apply
// BEFORE the resume, and must not fall into the delete+create recovery — that
// recovery is what destroys the conversation, so a refusal that triggered it
// would cost the very thing the operator is being protected into keeping.
func TestStartManagerRefusesPreRoleRecordOnResume(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	deps := Deps{
		Scion:           scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error { return errPreRoleRefusal },
	}
	err := runApply(app, deps)
	wantErrIs(t, err, errPreRoleRefusal)
	if r.resumeCalls != 0 {
		t.Errorf("resumeCalls = %d, want 0 (the guard runs BEFORE the resume)", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 (refusing must never discard the session)", r.deleteCalls, r.startCalls)
	}
}

// TestStartManagerRefusesPreRoleRecordWhenRunning: the no-op branch is not
// exempt. A running agent refreshes its own token, and scion#1101 re-derives
// scopes from the stored role on every refresh, so an unrolled RUNNING record
// is promoted just the same.
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

// TestStartManagerVerifyAgentRoleSkippedWhenRecordAbsent: a fresh create stamps
// `--role baseline` itself, so there is nothing to refuse. Gating it would
// brick every first bring-up.
func TestStartManagerVerifyAgentRoleSkippedWhenRecordAbsent(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	called := 0
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		VerifyAgentRole: func(context.Context, string, string) error {
			called++
			return errPreRoleRefusal
		},
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called != 0 {
		t.Errorf("VerifyAgentRole called %d times, want 0 (no record to keep)", called)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1", r.startCalls)
	}
}

// TestStartManagerVerifyAgentRolePassesThrough: a record that DOES store a role
// is left entirely alone — the guard is invisible on a healthy instance.
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

// TestRegisterRepairsHubEndpointOnTheSkipPath: the skip path is exactly where a
// stale hub endpoint survives. Minting the controller PAT `hub link`s the
// project against a THROWAWAY hub on its own port; the destructive re-init
// would overwrite that, but it is skipped whenever the registration is sound.
// Live failure 2026-08-11: `lever attach` execs scion bare, read the project
// config, and dialled the dead throwaway port while every other verb worked.
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

// TestScionServerRestartsTheHubOnlyWhenTheLoginConfigChanged pins the
// condition on the restart. scion reads `oidc_login` once, at startup, so a
// hub that was already running when lever wrote the block would otherwise go
// on serving a configuration with no login in it — and an unconditional
// restart would drop every agent's hub connection on every apply.
func TestScionServerRestartsTheHubOnlyWhenTheLoginConfigChanged(t *testing.T) {
	run := func(t *testing.T, changed bool, ensureErr error) ([]proc.Call, error) {
		t.Helper()
		f := scionOKRunner()
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"},
			Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}}
		deps := Deps{
			Scion:          hubScion(f, app),
			EnsureHubLogin: func(context.Context) (bool, error) { return changed, ensureErr },
		}
		return f.Calls, runApply(app, deps)
	}
	stopped := func(calls []proc.Call) bool {
		for _, c := range calls {
			if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" && c.Args[1] == "stop" {
				return true
			}
		}
		return false
	}

	calls, err := run(t, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stopped(calls) {
		t.Fatal("an unchanged login configuration restarted the hub — every apply would drop the agents' connections")
	}

	calls, err = run(t, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stopped(calls) {
		t.Fatal("a changed login configuration did not restart the hub, so scion would never read it")
	}

	// A failure to provision the guest half must fail the apply, not start a
	// hub whose login path silently does not work.
	if _, err := run(t, false, errors.New("no go toolchain")); err == nil {
		t.Fatal("EnsureHubLogin's failure did not fail the apply")
	}
}

// TestRemoteDisabledConvergesTheGuestLoginPathOff: the forwarder is an
// unauthenticated bridge from guest loopback — reachable from every agent's
// netns — to a host loopback port beside lever's broker listeners. Stopping
// the proxy does not stop it, so an apply with remote access off must take it
// down, every time, whether or not this instance ever had one.
func TestRemoteDisabledConvergesTheGuestLoginPathOff(t *testing.T) {
	run := func(t *testing.T, remote config.Remote) (disabled, ensured int) {
		t.Helper()
		f := scionOKRunner()
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"}, Remote: remote}
		deps := Deps{
			Scion:           hubScion(f, app),
			EnsureHubLogin:  func(context.Context) (bool, error) { ensured++; return false, nil },
			DisableHubLogin: func(context.Context) (bool, error) { disabled++; return false, nil },
		}
		if err := runApply(app, deps); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return disabled, ensured
	}

	disabled, _ := run(t, config.Remote{})
	if disabled != 1 {
		t.Fatalf("remote off: DisableHubLogin called %d times, want 1 — the guest bridge outlives the feature otherwise", disabled)
	}

	disabled, ensured := run(t, config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"})
	if disabled != 0 {
		t.Fatalf("remote on: DisableHubLogin called %d times, want 0", disabled)
	}
	if ensured != 1 {
		t.Fatalf("remote on: EnsureHubLogin called %d times, want 1", ensured)
	}
}

// TestRemoteOffRestartsTheHubOnlyWhenTheGuestStillHadLoginState is the OFF
// half of TestScionServerRestartsTheHubOnlyWhenTheLoginConfigChanged, and it
// exists because the two halves were asymmetric: turning remote access off
// converged the guest and left the RUNNING hub exactly as it was — still
// serving a login whose provider had just been removed, still serving the SPA
// out of a staged directory. The scion-server step cannot repair that, because
// `scion server start` returns on an already-running daemon without touching
// its argv, so the state was never converged by any number of applies.
//
// Both directions are asserted, and the second is the expensive one to get
// wrong: a restart drops every agent's connection to the hub, so an apply that
// found nothing to remove must not stop anything and must not say anything.
func TestRemoteOffRestartsTheHubOnlyWhenTheGuestStillHadLoginState(t *testing.T) {
	run := func(t *testing.T, changed bool) (server, logs []string) {
		t.Helper()
		f := scionOKRunner()
		// Remote access off — the config this whole path is about.
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
			Manager: config.Manager{Image: "img"}}
		deps := Deps{
			Scion:           hubScion(f, app),
			DisableHubLogin: func(context.Context) (bool, error) { return changed, nil },
			Log:             func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		}
		if err := runApply(app, deps); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, c := range f.Calls {
			if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" {
				server = append(server, strings.Join(c.Args, " "))
			}
		}
		return server, logs
	}

	// The guest still carried the login: the hub is stopped and started again.
	// The order is the assertion — the step's own start comes first (and is
	// tolerated by a running daemon), then the stop, then the start that
	// actually replaces the argv. And what comes back carries no web flags:
	// dropping --web-assets-dir is the visible half of the convergence, since
	// the directory it named has been deleted.
	const plain = "server start --web-port 8080 --dev-auth=false"
	server, logs := run(t, true)
	want := []string{plain, "server stop", plain}
	if !slices.Equal(server, want) {
		t.Fatalf("server calls = %q, want %q — the hub was not restarted after the login was removed", server, want)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "restarting the hub") {
		t.Fatalf("logs = %q, want one line saying the hub is being restarted", logs)
	}

	// Nothing left to remove: an apply must be completely quiet.
	server, logs = run(t, false)
	if !slices.Equal(server, []string{plain}) {
		t.Fatalf("server calls = %q, want just the step's own start — a converged instance bounced the hub and every agent's connection to it", server)
	}
	if len(logs) != 0 {
		t.Fatalf("logs = %q, want none: a re-apply with remote access already off has nothing to report", logs)
	}
}

// TestRemoteOffSkipsTheHubRestartWhenThePlanDoesNotManageTheHub: the VM
// acceptance gate runs BrokerOnly, and its machine need not carry a scion
// binary at all (init-machine is one of the steps that plan drops). The guest
// still converges there — the forwarder is a bridge into the jail and comes
// down on every apply — but ordering a hub restart would fail an apply that
// was never driving scion in the first place.
func TestRemoteOffSkipsTheHubRestartWhenThePlanDoesNotManageTheHub(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img"}}
	disabled := 0
	deps := Deps{
		Scion:           hubScion(f, app),
		DisableHubLogin: func(context.Context) (bool, error) { disabled++; return true, nil },
	}
	if err := Run(context.Background(), app, fillDeps(deps), PlanOpts{BrokerOnly: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disabled != 1 {
		t.Fatalf("DisableHubLogin called %d times, want 1 — the jail→host bridge must come down on this path too", disabled)
	}
	for _, c := range f.Calls {
		if c.Name == "scion" {
			t.Fatalf("the broker-only plan drove scion: %q", strings.Join(c.Args, " "))
		}
	}
}

// stoppedHubRunner answers `scion server stop` the way scion answers it when
// nothing is running, and passes everything else to the agent-lifecycle runner
// the other apply tests use, so a whole Run can complete around it.
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

// TestScionServerRestartsAHubThatIsAlreadyStopped is the regression test for a
// live apply failure: the login configuration had changed, so the step went to
// restart the hub, but the hub was already down — after a `lever stop`, a VM
// reboot, a crashed hub, or an operator stopping it by hand. `scion server
// stop` exits non-zero with "server daemon is not running", and that error
// failed the whole apply, leaving the guest forwarder installed and running
// with no hub at all.
//
// Restarting something that is already stopped must be a no-op followed by a
// start.
func TestScionServerRestartsAHubThatIsAlreadyStopped(t *testing.T) {
	f := scionOKRunner()
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img"},
		Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"}}
	runner := &stoppedHubRunner{inner: &agentLifecycleRunner{FakeRunner: f, slug: app.Name}}
	deps := Deps{
		Scion:          scion.New(runner, scion.Options{HubEndpoint: testHubEndpoint}),
		EnsureHubLogin: func(context.Context) (bool, error) { return true, nil },
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

// TestAgentTemplateGetsTheJailPath: the closure behind EnsureAgentTemplate runs
// `scion config` INSIDE the jail, where the host tree exists only at the mount
// point. Passing the host path made a live apply fail with "cannot change
// directory to /Users/…: No such file or directory" — caught only because the
// step runs before start-manager and aborted the whole apply.
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

// TestRunRefusesIncompleteDeps pins Deps.check: a nil required collaborator
// fails Run by name before any step runs, and a Deps with every field set
// passes the check.
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
