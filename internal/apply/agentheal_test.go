package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// fakeSessionProbe is an AgentSessionProbe with canned answers. HubToken
// answers tok on its first call and afterReset (when set) on later ones;
// ReportSessionRunning runs onReport, which a test uses to flip the fake
// hub record the way sciontool's report would.
type fakeSessionProbe struct {
	tok, afterReset jail.HubTokenTimes
	tokErr          error
	alive           bool
	aliveErr        error
	reportErr       error
	onReport        func()

	refs                                []string
	tokenCalls, aliveCalls, reportCalls int
}

func (p *fakeSessionProbe) HubToken(_ context.Context, ref string) (jail.HubTokenTimes, error) {
	p.refs = append(p.refs, ref)
	p.tokenCalls++
	if p.tokErr != nil {
		return jail.HubTokenTimes{}, p.tokErr
	}
	if p.tokenCalls > 1 && !p.afterReset.Expiry.IsZero() {
		return p.afterReset, nil
	}
	return p.tok, nil
}

func (p *fakeSessionProbe) HarnessAlive(_ context.Context, ref string) (bool, error) {
	p.refs = append(p.refs, ref)
	p.aliveCalls++
	return p.alive, p.aliveErr
}

func (p *fakeSessionProbe) ReportSessionRunning(_ context.Context, ref string) error {
	p.refs = append(p.refs, ref)
	p.reportCalls++
	if p.reportErr != nil {
		return p.reportErr
	}
	if p.onReport != nil {
		p.onReport()
	}
	return nil
}

var healNow = time.Date(2026, 10, 6, 19, 30, 0, 0, time.UTC)

func validToken() jail.HubTokenTimes {
	return jail.HubTokenTimes{Expiry: healNow.Add(8 * time.Hour), Now: healNow}
}

func expiredToken() jail.HubTokenTimes {
	return jail.HubTokenTimes{Expiry: healNow.Add(-20 * time.Minute), Now: healNow}
}

func countCalls(f *proc.FakeRunner, verb string) int {
	n := 0
	for _, c := range f.Calls {
		if c.Name == "scion" && len(c.Args) > 0 && c.Args[0] == verb {
			n++
		}
	}
	return n
}

func logged(sink *logSink, sub string) bool {
	for _, l := range sink.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// TestHealAgentSessionResetsAnExpiredToken: a running agent whose hub token
// expired gets `scion reset-auth` for ITS slug, in its project — the
// controller-PAT verb that mints a new token of the agent's own stored role.
func TestHealAgentSessionResetsAnExpiredToken(t *testing.T) {
	f := scionOKRunner()
	sc := scion.New(f, scion.Options{})
	probe := &fakeSessionProbe{tok: expiredToken(), afterReset: validToken()}
	var sink logSink
	rec := &scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 3 hours"}
	got := HealAgentSession(context.Background(), sc, probe, sink.logf, "/lever", rec)
	if got != rec {
		t.Fatal("a token heal leaves the record as it was")
	}
	if !sawScionCall(f, "reset-auth assistant -g /lever --non-interactive") {
		t.Fatalf("no reset-auth for the agent: %q", joinedCalls(f))
	}
	if probe.refs[0] != "lever--assistant" {
		t.Fatalf("probed %q, want the agent's own container by name", probe.refs[0])
	}
	if !logged(&sink, "expired at 2026-10-06T19:10:00Z") || !logged(&sink, "new hub token, valid until") {
		t.Fatalf("log = %q", sink.lines)
	}
	if probe.aliveCalls != 0 {
		t.Fatal("a running record needs no harness probe")
	}
}

func TestHealAgentSessionLeavesAValidTokenAlone(t *testing.T) {
	f := scionOKRunner()
	probe := &fakeSessionProbe{tok: validToken()}
	var sink logSink
	HealAgentSession(context.Background(), scion.New(f, scion.Options{}), probe, sink.logf, "/lever",
		&scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 3 hours"})
	if countCalls(f, "reset-auth") != 0 || len(sink.lines) != 0 {
		t.Fatalf("a valid token must not be reset: calls %q, log %q", joinedCalls(f), sink.lines)
	}
}

// TestHealAgentSessionResetFailureNamesTheManualFix: a failed reset is a
// warning naming the command to run by hand, never fatal.
func TestHealAgentSessionResetFailureNamesTheManualFix(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion reset-auth", proc.Result{Code: 1, Stderr: "hub unreachable"})
	sc := scion.New(failingScionRunner{f}, scion.Options{})
	var sink logSink
	HealAgentSession(context.Background(), sc, &fakeSessionProbe{tok: expiredToken()}, sink.logf, "/lever",
		&scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 3 hours"})
	if !logged(&sink, "scion reset-auth failed") || !logged(&sink, "scion reset-auth assistant -g /lever") {
		t.Fatalf("log = %q", sink.lines)
	}
}

// failingScionRunner turns a non-zero scripted code into an error, as a real
// process exit would (FakeRunner returns a nil error for any script).
type failingScionRunner struct{ *proc.FakeRunner }

func (r failingScionRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := r.FakeRunner.RunIn(ctx, dir, env, name, args...)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit status %d", res.Code)
	}
	return res, err
}

func (r failingScionRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestHealAgentSessionSkips: no probe, no live container, or a phase other
// than running/stopped — nothing is probed and nothing is called.
func TestHealAgentSessionSkips(t *testing.T) {
	for _, rec := range []*scion.Agent{
		nil,
		{Slug: "assistant", Phase: "running", ContainerStatus: "Exited (0) 2 minutes ago"},
		{Slug: "assistant", Phase: "stopped", ContainerStatus: "stopped"},
		{Slug: "assistant", Phase: "suspended", ContainerStatus: "Up 1 second"},
		{Slug: "assistant", Phase: "error", ContainerStatus: "Up 1 second"},
	} {
		f := scionOKRunner()
		probe := &fakeSessionProbe{tok: expiredToken(), alive: true}
		var sink logSink
		HealAgentSession(context.Background(), scion.New(f, scion.Options{}), probe, sink.logf, "/lever", rec)
		if len(probe.refs) != 0 || len(f.Calls) != 0 {
			t.Fatalf("%+v: probed %v, called %q", rec, probe.refs, joinedCalls(f))
		}
	}
	f := scionOKRunner()
	got := HealAgentSession(context.Background(), scion.New(f, scion.Options{}), nil, (&logSink{}).logf, "/lever",
		&scion.Agent{Slug: "assistant", Phase: "stopped", ContainerStatus: "Up 1 hour"})
	if got == nil || len(f.Calls) != 0 {
		t.Fatal("a nil probe heals nothing")
	}
}

// TestStartManagerStoppedOverALiveHarnessContinues (the 2026-10-05 case): the
// hub reads phase stopped because another claude process in the container
// fired the SessionEnd hook, while the manager's claude still runs. apply has
// the agent report its session running and keeps it — no resume, which would
// have restarted claude in a new session.
func TestStartManagerStoppedOverALiveHarnessContinues(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "Up 2 hours"}
	probe := &fakeSessionProbe{tok: validToken(), alive: true, onReport: func() { r.phase = "running" }}
	var sink logSink
	deps := Deps{Scion: scion.New(r, scion.Options{}), AgentSession: probe, Log: sink.logf}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.reportCalls != 1 {
		t.Fatalf("reportCalls = %d, want 1", probe.reportCalls)
	}
	if r.resumeCalls != 0 || r.startCalls != 0 || r.deleteCalls != 0 {
		t.Fatalf("resume/start/delete = %d/%d/%d, want 0/0/0 (the live session is kept)", r.resumeCalls, r.startCalls, r.deleteCalls)
	}
	if !logged(&sink, "still runs in its container") || !logged(&sink, "conversation continues") {
		t.Fatalf("log = %q", sink.lines)
	}
}

// TestStartManagerStoppedWithClaudeGoneResumes: a claude that really exited
// leaves the old path — resume — and apply says it starts a new session and
// how to get the old conversation back.
func TestStartManagerStoppedWithClaudeGoneResumes(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "Up 2 hours"}
	probe := &fakeSessionProbe{tok: validToken(), alive: false}
	var sink logSink
	if err := runApply(app, Deps{Scion: scion.New(r, scion.Options{}), AgentSession: probe, Log: sink.logf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.reportCalls != 0 || r.resumeCalls != 1 {
		t.Fatalf("reportCalls=%d resumeCalls=%d, want 0/1", probe.reportCalls, r.resumeCalls)
	}
	if !logged(&sink, "new claude session") || !logged(&sink, "/resume") {
		t.Fatalf("log = %q", sink.lines)
	}
}

// TestStartManagerStoppedReportThatDoesNotStickWarns: the report ran but the
// hub still reads stopped (its token was refused, say): apply warns and falls
// back to the resume, naming the way back to the old conversation.
func TestStartManagerStoppedReportThatDoesNotStickWarns(t *testing.T) {
	old := sessionHealSettle
	sessionHealSettle = RetryBudget{Attempts: 2, Interval: time.Millisecond}
	t.Cleanup(func() { sessionHealSettle = old })
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "Up 2 hours"}
	probe := &fakeSessionProbe{tok: validToken(), alive: true}
	var sink logSink
	if err := runApply(app, Deps{Scion: scion.New(r, scion.Options{}), AgentSession: probe, Log: sink.logf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 1 || !logged(&sink, `still reads phase "stopped"`) {
		t.Fatalf("resumeCalls=%d log=%q", r.resumeCalls, sink.lines)
	}
}

// TestStartManagerHealsTokenOfARunningManager: a running manager is kept as
// before, and its expired token reset on the way.
func TestStartManagerHealsTokenOfARunningManager(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 9 hours"}
	probe := &fakeSessionProbe{tok: expiredToken(), afterReset: validToken()}
	var sink logSink
	if err := runApply(app, Deps{Scion: scion.New(r, scion.Options{}), AgentSession: probe, Log: sink.logf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawScionCall(f, "reset-auth hello") || r.resumeCalls != 0 {
		t.Fatalf("calls %q resumeCalls=%d", joinedCalls(f), r.resumeCalls)
	}
}

// TestStartManagerFreshSkipsTheHeal: --fresh discards the record, so its
// session is not worth a heal.
func TestStartManagerFreshSkipsTheHeal(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "stopped", initContainerStatus: "Up 2 hours"}
	probe := &fakeSessionProbe{tok: expiredToken(), alive: true}
	if err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{}), AgentSession: probe, Log: (&logSink{}).logf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if probe.reportCalls != 0 || sawScionCall(f, "reset-auth hello") {
		t.Fatalf("--fresh healed the record it discards: reports=%d calls=%q", probe.reportCalls, joinedCalls(f))
	}
}

// TestHealSessionsCoversWorkers: `lever up` on a running manager heals the
// manager and every configured worker from one listing; an undeclared agent
// is left alone.
func TestHealSessionsCoversWorkers(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list", proc.Result{Stdout: `[{"slug":"hello","phase":"running","containerStatus":"Up 1 hour"},` +
		`{"slug":"scratch","phase":"running","containerStatus":"Up 5 minutes"},` +
		`{"slug":"stray","phase":"running","containerStatus":"Up 5 minutes"}]`})
	f.Script("scion reset-auth", proc.Result{Stdout: "ok"})
	app := helloApp(t.TempDir())
	app.Workers = []config.Worker{{Name: "scratch", Dir: "workers/scratch"}, {Name: "idle", Dir: "workers/idle"}}
	probe := &fakeSessionProbe{tok: expiredToken()}
	HealSessions(context.Background(), Deps{Scion: scion.New(f, scion.Options{}), AgentSession: probe, Log: (&logSink{}).logf}, app, "/lever")
	if !sawScionCall(f, "reset-auth hello") || !sawScionCall(f, "reset-auth scratch") || sawScionCall(f, "reset-auth stray") {
		t.Fatalf("calls %q", joinedCalls(f))
	}
	if countCalls(f, "list") != 1 {
		t.Fatalf("listed %d times, want once", countCalls(f, "list"))
	}
}

// TestHealAgentSessionTokenReadFailureIsAWarning: an unreadable token never
// triggers a reset, only a warning.
func TestHealAgentSessionTokenReadFailureIsAWarning(t *testing.T) {
	f := scionOKRunner()
	var sink logSink
	HealAgentSession(context.Background(), scion.New(f, scion.Options{}), &fakeSessionProbe{tokErr: errors.New("exit status 1")}, sink.logf, "/lever",
		&scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 3 hours"})
	if countCalls(f, "reset-auth") != 0 || !logged(&sink, "could not read agent \"assistant\"'s hub token") {
		t.Fatalf("calls %q log %q", joinedCalls(f), sink.lines)
	}
}
