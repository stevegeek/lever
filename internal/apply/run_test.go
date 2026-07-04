package apply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
)

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
// `scion list --format json` itself (Task 4's observe-first start-manager
// calls List before AND after Start): the very first (observe-first) list call
// reports slug ABSENT (so the create path — and thus Start — is actually
// taken), and every call after that reports it running/running (so the
// post-start liveness verify converges as soon as Start finally succeeds).
type flakyStartRunner struct {
	*exec.FakeRunner
	slug       string
	startFails int
	startCalls int
	listCalls  int
}

func (r *flakyStartRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" {
		if isObserveList(args) {
			r.listCalls++
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, exec.Call{Name: name, Args: args, Env: env, Dir: dir})
			if r.listCalls == 1 {
				return exec.Result{Stdout: "[]"}, nil
			}
			return exec.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":"running","containerStatus":"running"}]`, r.slug)}, nil
		}
		hasStart, hasServer := false, false
		for _, a := range args {
			if a == "start" {
				hasStart = true
			}
			if a == "server" {
				hasServer = true
			}
		}
		if hasStart && !hasServer { // agent start, not `scion server start`
			r.startCalls++
			if r.startCalls <= r.startFails {
				// Client.run builds its error from Stdout+Stderr, so the marker must
				// live there, not just in the Go error.
				return exec.Result{Code: 1, Stderr: "no_runtime_broker: No runtime brokers available for this project"}, fmt.Errorf("exit status 1")
			}
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *flakyStartRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// alreadyUpRunner simulates a fully-up instance on re-apply: `scion server
// start` and agent `start` return "already running"; everything else
// succeeds. It also answers `scion list --format json` itself (Task 4's
// observe-first start-manager calls List before AND after Start): the first
// list call reports slug ABSENT (so start-manager still takes the create path
// and calls Start, exercising the AlreadyRunning tolerance below), and every
// call after that reports it running/running (so the post-start liveness
// verify converges once AlreadyRunning is tolerated as success).
type alreadyUpRunner struct {
	*exec.FakeRunner
	slug      string
	listCalls int
}

func (r *alreadyUpRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" {
		if isObserveList(args) {
			r.listCalls++
			r.FakeRunner.Calls = append(r.FakeRunner.Calls, exec.Call{Name: name, Args: args, Env: env, Dir: dir})
			if r.listCalls == 1 {
				return exec.Result{Stdout: "[]"}, nil
			}
			return exec.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":"running","containerStatus":"running"}]`, r.slug)}, nil
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
			return exec.Result{Code: 1, Stderr: "Error: server is already running (PID: 123)"}, fmt.Errorf("exit status 1")
		}
		if hasStart && !hasServer {
			return exec.Result{Code: 1, Stderr: "Error: agent already running"}, fmt.Errorf("exit status 1")
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *alreadyUpRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// agentLifecycleRunner fakes `scion list/start/resume/delete` around a SINGLE
// manager-agent record, for Task 4's observe-first start-manager tests. list
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
	*exec.FakeRunner
	slug string

	initPhase, initContainerStatus   string
	liveWhenPhase, liveWhenContainer string

	resumeErr, deleteErr, startErr error
	// resumeFailsThenSucceed, when > 0, makes resume fail (with resumeErr) for
	// exactly this many calls, then succeed from the next call onward — models
	// a transient broker-unavailable race resolving mid-retry. Zero (the
	// default) preserves the original all-tests-so-far behavior: if resumeErr
	// is set, resume fails EVERY call (no eventual success).
	resumeFailsThenSucceed int

	phase, containerStatus string
	inited                 bool

	startCalls, resumeCalls, deleteCalls, listCalls int
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
	r.FakeRunner.Calls = append(r.FakeRunner.Calls, exec.Call{Name: name, Args: args, Env: env, Dir: dir})
}

// verb extracts the scion subcommand from args, skipping the leading `-g
// <project>` pair Start puts first (see scion.Client.Start) — list/resume/
// delete all put their verb at args[0] directly (see scion.Client.List /
// Resume / Delete).
func (r *agentLifecycleRunner) verb(args []string) string {
	if len(args) == 0 {
		return ""
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

func (r *agentLifecycleRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name != "scion" {
		return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
	}
	r.ensureInit()
	switch r.verb(args) {
	case "list":
		r.listCalls++
		r.record(dir, env, name, args)
		if r.phase == "" {
			return exec.Result{Stdout: "[]"}, nil
		}
		return exec.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":%q,"containerStatus":%q}]`, r.slug, r.phase, r.containerStatus)}, nil
	case "start":
		r.startCalls++
		r.record(dir, env, name, args)
		if r.startErr != nil {
			return exec.Result{Code: 1, Stderr: r.startErr.Error()}, r.startErr
		}
		r.goLive()
		return exec.Result{Stdout: "ok"}, nil
	case "resume":
		r.resumeCalls++
		r.record(dir, env, name, args)
		if r.resumeErr != nil && (r.resumeFailsThenSucceed == 0 || r.resumeCalls <= r.resumeFailsThenSucceed) {
			return exec.Result{Code: 1, Stderr: r.resumeErr.Error()}, r.resumeErr
		}
		r.goLive()
		return exec.Result{Stdout: "ok"}, nil
	case "delete":
		r.deleteCalls++
		r.record(dir, env, name, args)
		if r.deleteErr != nil {
			return exec.Result{Code: 1, Stderr: r.deleteErr.Error()}, r.deleteErr
		}
		r.phase, r.containerStatus = "", ""
		return exec.Result{Stdout: "ok"}, nil
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *agentLifecycleRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// newObserveFirstApp returns a minimal app + fresh FakeRunner for Task 4's
// start-manager observe-first tests, sharing one shape across the matrix
// (name "hello", matching agentLifecycleRunner's slug in each test below).
func newObserveFirstApp(t *testing.T) (*config.App, *exec.FakeRunner) {
	t.Helper()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img"},
	}
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
// records too (per the plan's Evidence base: "scion resume help: 'Resume a
// stopped scion agent' — covers stopped as well as suspended").
func TestStartManagerObserveFirstResumesStopped(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "stopped"}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
	origAtt, origInt := managerLiveAttempts, managerLiveInterval
	managerLiveAttempts, managerLiveInterval = 3, time.Millisecond
	defer func() { managerLiveAttempts, managerLiveInterval = origAtt, origInt }()

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "stopped"}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a running record whose container never comes up must fail, not silently pass")
	}
	if !strings.Contains(err.Error(), `phase "running"`) || !strings.Contains(err.Error(), `container "stopped"`) {
		t.Fatalf("error should report the last observed phase/container, got: %v", err)
	}
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
	var logged []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		Log: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
	if len(logged) != 1 {
		t.Fatalf("expected exactly one loud log line, got %+v", logged)
	}
	if !strings.Contains(logged[0], "resume failed") || !strings.Contains(logged[0], "FRESH") || !strings.Contains(logged[0], "previous session lost") {
		t.Fatalf("recovery log line missing expected wording, got %q", logged[0])
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a failed resume AND a failed delete must be a hard error")
	}
	if !strings.Contains(err.Error(), "cannot resume") || !strings.Contains(err.Error(), "delete: agent locked") {
		t.Fatalf("error should mention BOTH the resume and delete failures, got: %v", err)
	}
	if r.startCalls != 0 {
		t.Errorf("startCalls = %d, want 0 (must not attempt a fresh create over an undeleted record)", r.startCalls)
	}
}

// TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds proves the
// CRITICAL fix (resume-branch-review.md C1): `scion resume` shares Start's
// runtime-broker-registration race, so a resume that fails with the
// broker-unavailable wording must be RETRIED (same brokerStartAttempts/
// brokerStartInterval budget as Start) before any loud recovery — a transient
// blip must never destroy a resumable conversation.
func TestStartManagerResumeRetriesOnBrokerUnavailableThenSucceeds(t *testing.T) {
	origAtt, origInt := brokerStartAttempts, brokerStartInterval
	brokerStartAttempts, brokerStartInterval = 5, time.Millisecond
	defer func() { brokerStartAttempts, brokerStartInterval = origAtt, origInt }()

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		// scion resume's own transient wording (singular "broker" — see
		// isBrokerUnavailable's doc), failing the first 2 calls then resolving.
		resumeErr:              fmt.Errorf("cannot resume agent: no runtime broker available"),
		resumeFailsThenSucceed: 2,
	}
	var logged []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		Log: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run should succeed once the broker race resolves within the retry budget: %v", err)
	}
	if r.resumeCalls != 3 {
		t.Fatalf("resumeCalls = %d, want 3 (2 transient failures + 1 success)", r.resumeCalls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 0/0 — a resume that eventually succeeds must NOT delete/recreate (conversation preserved)", r.deleteCalls, r.startCalls)
	}
	if len(logged) != 0 {
		t.Errorf("no loud recovery log expected when resume eventually succeeds, got %+v", logged)
	}
}

// TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRecovers is C1's
// complement: if the broker-unavailable race NEVER resolves within the retry
// budget, start-manager must still fall back to the loud delete+fresh
// recovery — the retry absorbs a transient blip, it does not turn a
// permanently-unavailable broker into an infinite hang.
func TestStartManagerResumeBrokerUnavailableExhaustsRetriesThenRecovers(t *testing.T) {
	origAtt, origInt := brokerStartAttempts, brokerStartInterval
	brokerStartAttempts, brokerStartInterval = 3, time.Millisecond
	defer func() { brokerStartAttempts, brokerStartInterval = origAtt, origInt }()

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: fmt.Errorf("cannot resume agent: no runtime broker available"),
		// resumeFailsThenSucceed left 0: resume fails on EVERY call, exercising
		// full budget exhaustion.
	}
	var logged []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		Log: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("an exhausted-but-transient resume must still recover fresh, not fail the apply: %v", err)
	}
	if r.resumeCalls != 3 {
		t.Fatalf("resumeCalls = %d, want 3 (the full retry budget burned)", r.resumeCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1 (loud recovery only AFTER the retry budget exhausts)", r.deleteCalls, r.startCalls)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "resume failed") || !strings.Contains(logged[0], "FRESH") {
		t.Fatalf("expected exactly one loud recovery log line, got %+v", logged)
	}
}

// TestStartManagerLivenessNeverGreenAfterCreate: `scion start` reports success
// but the container never actually comes up (scion's own false-success — see
// the plan's Evidence base). The liveness verify must exhaust its attempts and
// fail loudly with the last observed phase/container, rather than trusting
// the CLI's exit code.
func TestStartManagerLivenessNeverGreenAfterCreate(t *testing.T) {
	origAtt, origInt := managerLiveAttempts, managerLiveInterval
	managerLiveAttempts, managerLiveInterval = 3, time.Millisecond
	defer func() { managerLiveAttempts, managerLiveInterval = origAtt, origInt }()

	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		liveWhenContainer: "stopped", // Start "succeeds" but the container never lives
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a manager whose container never comes up must fail the apply, not report success")
	}
	if !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), `container "stopped"`) {
		t.Fatalf("error should say the manager did not come up and report the last container status, got: %v", err)
	}
	if r.startCalls != 1 {
		t.Errorf("startCalls = %d, want 1 (Start itself must still have been attempted)", r.startCalls)
	}
}

// TestStartManagerUnexpectedPhaseRecoversFresh proves the IMPORTANT fix
// (resume-branch-review.md I1): an unhandled-but-real scion phase (here
// "error" — a crashed manager, e.g. an OOM/harness crash) must NOT hard-fail
// (brick) `lever up` with no path forward but `lever destroy`. It takes the
// SAME loud delete+fresh recovery as a failed resume, so `up` converges.
func TestStartManagerUnexpectedPhaseRecoversFresh(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "stopped"}
	var logged []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		Log: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("an unexpected/error phase must recover fresh, not hard-fail the apply: %v", err)
	}
	if r.resumeCalls != 0 {
		t.Errorf("resumeCalls = %d, want 0 (an error phase is not resumable — resume is only for suspended/stopped)", r.resumeCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1 (delete the unrecoverable record, then fresh create)", r.deleteCalls, r.startCalls)
	}
	if len(logged) != 1 {
		t.Fatalf("expected exactly one loud recovery log line, got %+v", logged)
	}
	if !strings.Contains(logged[0], `phase "error"`) || !strings.Contains(logged[0], "FRESH") || !strings.Contains(logged[0], "previous session lost") {
		t.Fatalf("recovery log line missing expected wording, got %q", logged[0])
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
// absent-record create path, with no MintManagerBootstrap wired (so boot is
// untouched/"empty" exactly as it would be after a tolerated spent latch),
// must call RearmBootstrap exactly once and then proceed with Start.
func TestStartManagerCreateRearmsSpentLatchWhenNoFreshMintThisRun(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"} // initPhase "" == absent -> create path
	rearmCalls := 0
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			rearmCalls++
			return BootstrapMaterial{Ticket: "fresh-ticket"}, nil
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{Ticket: "minted-this-run"}, nil // fresh mint, no latch
		},
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			rearmCalls++
			return BootstrapMaterial{}, nil
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		Log:       func(string, ...any) {},
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			rearmCalls++
			return BootstrapMaterial{Ticket: "fresh-after-recovery"}, nil
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 1 {
		t.Errorf("RearmBootstrap calls = %d, want 1 (the post-recovery-delete create must re-arm too)", rearmCalls)
	}
	if r.deleteCalls != 1 || r.startCalls != 1 {
		t.Errorf("deleteCalls=%d startCalls=%d, want 1/1", r.deleteCalls, r.startCalls)
	}
}

// TestStartManagerResumeNeverRearms: a record that resumes successfully never
// reaches the create path at all — its agent home (and thus its enrol cert)
// already exists, so re-arming would be pointless (and would needlessly
// bounce the broker).
func TestStartManagerResumeNeverRearms(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	rearmCalls := 0
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			rearmCalls++
			return BootstrapMaterial{}, nil
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearmCalls != 0 {
		t.Errorf("RearmBootstrap calls = %d, want 0 (a successful resume never creates, so it never re-arms)", rearmCalls)
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			rearmCalls++
			return BootstrapMaterial{}, nil
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		RearmBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, fmt.Errorf("broker restart failed: connection refused")
		},
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a failed re-arm must fail the apply — a create over a spent latch is guaranteed to 403")
	}
	if !strings.Contains(err.Error(), "bootstrap") || !strings.Contains(err.Error(), "latch") {
		t.Fatalf("error should mention bootstrap/latch, got: %v", err)
	}
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		// RearmBootstrap intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(fe, scion.Options{}),
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a List error observing agents must fail start-manager")
	}
	if !strings.Contains(err.Error(), "observing agents") {
		t.Fatalf("error should mention observing agents, got: %v", err)
	}
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

func (r *firstListErrRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" && isObserveList(args) {
		r.listSeen++
		if r.listSeen == 1 {
			return exec.Result{Code: 1, Stderr: "hub: internal server error"}, fmt.Errorf("exit status 1")
		}
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *firstListErrRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
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

func (r *failNListsRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" && isObserveList(args) {
		r.listSeen++
		if r.listSeen > 1 && r.listSeen <= 1+r.failCount {
			return exec.Result{Code: 1, Stderr: "hub: transient blip"}, fmt.Errorf("exit status 1")
		}
	}
	return r.inner.RunIn(ctx, dir, env, name, args...)
}

func (r *failNListsRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestWaitManagerLiveToleratesMidPollListErrors proves the MINOR fix
// (resume-branch-review.md M1): two transient List errors during the
// post-action liveness poll must not abort the apply — they are consumed
// within the existing retry budget, and the poll succeeds as soon as a List
// call reports the manager running/running.
func TestWaitManagerLiveToleratesMidPollListErrors(t *testing.T) {
	origAtt, origInt := managerLiveAttempts, managerLiveInterval
	managerLiveAttempts, managerLiveInterval = 5, time.Millisecond
	defer func() { managerLiveAttempts, managerLiveInterval = origAtt, origInt }()

	app, f := newObserveFirstApp(t)
	// Already running/live — the no-op branch — so start-manager's OWN observe
	// (list call 1) must succeed; failNListsRunner then fails exactly the next
	// two list calls (2 and 3, both inside waitManagerLive's poll) before
	// deferring back to a live running/running record on call 4.
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "running"}
	fe := &failNListsRunner{inner: r, failCount: 2}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(fe, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner(), slug: "demo"}
	r.Script("scion", exec.Result{Stdout: "ok"})
	mintCalled := false
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		// Same broker process as a prior apply ⇒ latch spent ⇒ ErrBootstrapLatched.
		// The mint step must tolerate it (the manager already has its bootstrap).
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			mintCalled = true
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner(), slug: "demo"}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: connection refused")
		},
	}
	if err := Run(context.Background(), app, deps); err == nil {
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
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner(), slug: "demo"}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a spent latch with no staged bootstrap must fail loudly (stale broker)")
	}
	if !strings.Contains(err.Error(), "lever down") {
		t.Fatalf("error should guide the user to `lever down`, got: %v", err)
	}
}

func TestStartManagerRetriesOnBrokerUnavailable(t *testing.T) {
	// Make the retry fast for the test.
	origAtt, origInt := brokerStartAttempts, brokerStartInterval
	brokerStartAttempts, brokerStartInterval = 5, time.Millisecond
	defer func() { brokerStartAttempts, brokerStartInterval = origAtt, origInt }()

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
	r := &flakyStartRunner{FakeRunner: exec.NewFakeRunner(), slug: "hello", startFails: 2}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run should succeed after the broker race resolves: %v", err)
	}
	if r.startCalls != 3 {
		t.Fatalf("start attempted %d times, want 3 (2 transient failures + 1 success)", r.startCalls)
	}
}
func TestRunDispatchesStepsInOrder(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "scionlocal/lever-claude:latest"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	var jailUp, loadImg bool
	deps := Deps{
		JailUp: func(context.Context, *config.App) error { jailUp = true; return nil },
		LoadImage: func(_ context.Context, ref string) error {
			loadImg = (ref == "scionlocal/lever-claude:latest")
			return nil
		},
		Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !jailUp || !loadImg {
		t.Fatalf("host steps not called: jailUp=%v loadImg=%v", jailUp, loadImg)
	}
	j := ""
	for _, c := range f.Calls {
		j += strings.Join(c.Args, " ") + "|"
	}
	for _, want := range []string{"init --machine", "config set --global image_registry scionlocal", "server start", "init --non-interactive", "hub link", "start hello"} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing scion call %q in: %q", want, j)
		}
	}
}

func TestRunCredentialStep(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img", CredentialFile: "/x/token"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		ReadCred:  func(string) (string, error) { return "sk-ant-raw", nil },
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := ""
	for _, c := range f.Calls {
		j += strings.Join(c.Args, " ") + "|"
	}
	// secret value is base64-encoded (scion >= da49e14): b64("sk-ant-raw")
	if want := "hub secret set CLAUDE_CODE_OAUTH_TOKEN c2stYW50LXJhdw=="; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

func TestStartManagerPassesPrompt(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace", "groves", "worker"), 0o755)
	// prompt lives at the instance ROOT (host-only), NOT under the mounted tree.
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte("Dispatch the worker grove to create HELLO."), 0o644); err != nil {
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawPrompt bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "start hello") && strings.Contains(j, "Dispatch the worker grove to create HELLO.") {
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawEnvSet, sawPlaceholder bool
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if argv == "hub env set --project LEVER_LLM_AUTH=api-key" {
			sawEnvSet = true
		}
		// SecretSet base64-encodes the value, so match on the verb + key, not value.
		if len(c.Args) >= 4 && c.Args[0] == "hub" && c.Args[1] == "secret" && c.Args[2] == "set" && c.Args[3] == "ANTHROPIC_API_KEY" {
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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
		{"/tmp/foo/groves/worker", "/tmp/foo", "/lever", "/lever/groves/worker"},
		{"/tmp/foo", "/tmp/foo", "", "/tmp/foo"},
		{"/elsewhere", "/tmp/foo", "/lever", "/elsewhere"},
	}
	for _, c := range cases {
		if got := jailPath(c.host, c.tree, c.mount); got != c.want {
			t.Errorf("jailPath(%q, %q, %q) = %q, want %q", c.host, c.tree, c.mount, got, c.want)
		}
	}
}

func TestRemoveStaleMarker(t *testing.T) {
	// marker FILE is removed
	d1 := t.TempDir()
	mf := filepath.Join(d1, ".scion")
	if err := os.WriteFile(mf, []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleMarker(d1); err != nil {
		t.Fatalf("removeStaleMarker(file): %v", err)
	}
	if _, err := os.Stat(mf); !os.IsNotExist(err) {
		t.Errorf("marker file should be gone, stat err=%v", err)
	}

	// .scion DIRECTORY is left untouched (in-repo git-mode project)
	d2 := t.TempDir()
	md := filepath.Join(d2, ".scion")
	if err := os.Mkdir(md, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleMarker(d2); err != nil {
		t.Fatalf("removeStaleMarker(dir): %v", err)
	}
	if info, err := os.Stat(md); err != nil || !info.IsDir() {
		t.Errorf("marker DIR should be preserved, err=%v", err)
	}

	// absent .scion is a no-op
	if err := removeStaleMarker(t.TempDir()); err != nil {
		t.Errorf("removeStaleMarker(absent): %v", err)
	}
}

func TestRegisterRemovesStaleMarkerBeforeInit(t *testing.T) {
	// A stale marker in the tree must be gone by the time `scion init` runs,
	// so init creates a fresh project (writing workspace_path) rather than
	// resolving the stale marker and skipping it.
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale marker should have been removed before init, stat err=%v", err)
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	var calls []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		RemoveJailFile: func(_ context.Context, jailPath string) error {
			calls = append(calls, jailPath)
			return nil // deliberately does NOT touch the host file
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
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

// TestRegisterHostFallbackWhenRemoveJailFileNil pins the pre-existing
// host-side behavior (RemoveJailFile nil, e.g. tests / the broker-only VM
// gate): removeStaleMarker(s.Target) still runs and the marker is gone by the
// time `scion init` runs. This is a regression guard alongside the existing
// TestRegisterRemovesStaleMarkerBeforeInit test.
func TestRegisterHostFallbackWhenRemoveJailFileNil(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		// RemoveJailFile intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("host-side fallback should have removed the marker, stat err=%v", err)
	}
}

// TestRegisterRemovesStaleScionProjectConfigsBeforeInit proves that both
// register-manager and register-grove call Deps.RemoveScionProjectConfigs
// with the target's JAIL workspace path BEFORE `scion init` runs — the
// removal counterpart to the marker-removal race fix above. Without this,
// every apply mints a fresh ~/.scion/project-configs/<uuid> registration and
// the old ones accumulate (the `lever doctor` "duplicate registrations"
// finding).
func TestRegisterRemovesStaleScionProjectConfigsBeforeInit(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	var removeCalls []string
	var initCalls []string
	// Ordering proof: at the moment each RemoveScionProjectConfigs call fires,
	// count how many `scion init --non-interactive` calls the SAME fake runner
	// has already recorded. Since FakeRunner appends to f.Calls synchronously
	// in call order, a count of 0 for the manager's remove call proves it ran
	// before manager init, and a count of 1 for the grove's remove call proves
	// it ran after manager init but before grove init.
	var initCountAtRemove []int
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
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

	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(removeCalls) != 2 {
		t.Fatalf("RemoveScionProjectConfigs calls = %+v, want exactly 2 (manager + grove)", removeCalls)
	}
	if removeCalls[0] != "/lever" {
		t.Errorf("manager remove call path = %q, want /lever", removeCalls[0])
	}
	if removeCalls[1] != "/lever/groves/worker" {
		t.Errorf("grove remove call path = %q, want /lever/groves/worker", removeCalls[1])
	}
	// Manager's remove call must precede ANY init (count 0); the grove's remove
	// call runs after the manager's own init (which already ran, since
	// register-manager completes as one step before register-grove starts) but
	// still before the grove's OWN init (count exactly 1, not 2).
	wantCounts := []int{0, 1}
	for i, n := range initCountAtRemove {
		if n != wantCounts[i] {
			t.Errorf("remove call %d (%s): %d init call(s) had already fired, want %d — it must run before its OWN init", i, removeCalls[i], n, wantCounts[i])
		}
	}

	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			initCalls = append(initCalls, c.Dir)
		}
	}
	if len(initCalls) != 2 {
		t.Fatalf("init calls = %+v, want exactly 2", initCalls)
	}
}

// TestRegisterToleratesNilRemoveScionProjectConfigs proves the Deps field is
// optional: leaving it nil (as every pre-existing Deps literal in this file
// does) must not crash Run, and `scion init` still runs.
func TestRegisterToleratesNilRemoveScionProjectConfigs(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		// RemoveScionProjectConfigs intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawInit bool
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
			sawInit = true
		}
	}
	if !sawInit {
		t.Fatal("scion init should still run when RemoveScionProjectConfigs is nil")
	}
}

// TestRegisterSkipsDestructivePathWhenAlreadyRegistered proves the idempotent-
// register gate: when Deps.ScionProjectRegistered reports the workspace is
// already validly registered, register-manager/register-grove must skip its
// destructive clean+init path ENTIRELY — no marker removal, no
// RemoveScionProjectConfigs, no `scion init`/`hub link`. This is the fix for
// the resume-orphaning bug: a suspended manager (or grove) agent record's
// project linkage must survive a re-apply when nothing is actually wrong with
// the registration.
func TestRegisterSkipsDestructivePathWhenAlreadyRegistered(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	var removeJailCalls, removeConfigCalls, registeredCalls []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
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
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(registeredCalls) != 2 || registeredCalls[0] != "/lever" || registeredCalls[1] != "/lever/groves/worker" {
		t.Fatalf("ScionProjectRegistered calls = %+v, want [/lever /lever/groves/worker]", registeredCalls)
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

// TestRegisterRunsDestructivePathWhenNotRegistered pins the complement: when
// Deps.ScionProjectRegistered reports false, the full existing destructive
// path (marker removal, RemoveScionProjectConfigs, init, hub link) still runs
// exactly as it does today.
func TestRegisterRunsDestructivePathWhenNotRegistered(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	var removeJailCalls, removeConfigCalls []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
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
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(removeJailCalls) != 1 || removeJailCalls[0] != "/lever/.scion" {
		t.Fatalf("RemoveJailFile calls = %+v, want exactly one call with \"/lever/.scion\"", removeJailCalls)
	}
	if len(removeConfigCalls) != 1 || removeConfigCalls[0] != "/lever" {
		t.Fatalf("RemoveScionProjectConfigs calls = %+v, want exactly one call with \"/lever\"", removeConfigCalls)
	}
	var sawInit, sawHubLink bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			sawInit = true
		}
		if strings.Contains(j, "hub link") {
			sawHubLink = true
		}
	}
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	var removeConfigCalls []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
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
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v (an observe error must fail OPEN to the destructive path, not fail the apply)", err)
	}
	if len(removeConfigCalls) != 1 {
		t.Fatalf("observe error must fall through to the destructive path; RemoveScionProjectConfigs calls = %+v", removeConfigCalls)
	}
	var sawInit bool
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
			sawInit = true
		}
	}
	if !sawInit {
		t.Fatal("scion init should still run when the observe read errors")
	}
}

// TestRegisterToleratesNilScionProjectRegistered proves the Deps field is
// optional: leaving it nil (as every pre-existing Deps literal in this file
// does, before this task) must not crash Run, and `scion init` still runs.
func TestRegisterToleratesNilScionProjectRegistered(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		// ScionProjectRegistered intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawInit bool
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
			sawInit = true
		}
	}
	if !sawInit {
		t.Fatal("scion init should still run when ScionProjectRegistered is nil")
	}
}

func TestRegisterUsesJailPaths(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var managerInit, groveInit bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			switch c.Dir {
			case "/lever":
				managerInit = true
			case "/lever/groves/worker":
				groveInit = true
			default:
				t.Errorf("init call used host dir %q, want jail path", c.Dir)
			}
		}
		if strings.Contains(j, "hub link") {
			if c.Dir != "/lever" && c.Dir != "/lever/groves/worker" {
				t.Errorf("hub link call used host dir %q, want jail path", c.Dir)
			}
		}
	}
	if !managerInit {
		t.Errorf("manager init not run with dir /lever")
	}
	if !groveInit {
		t.Errorf("grove init not run with dir /lever/groves/worker")
	}
}

func TestStartUsesJailPath(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
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

// TestContainerLive pins the accepted containerStatus shapes: podman's live
// status text ("Up …") and the canonical "running"; everything else — incl.
// exited/stopped — is not live.
func TestContainerLive(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"running", true},
		{"Up 6 seconds", true},
		{"Up About a minute", true},
		{"stopped", false},
		{"Exited (1) 4 minutes ago", false},
		{"", false},
	} {
		if got := containerLive(tc.status); got != tc.want {
			t.Errorf("containerLive(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
