package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
)

// TestStopSuspendsManager: with a reachable jail, `lever stop` suspends the
// manager before powering off. It must be `scion suspend`: resume relaunches
// `claude --continue` from the persistent agent home, while `scion stop`
// removes the container and discards the session.
func TestStopSuspendsManager(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	// The suspend client must keep its HubTokenSource: without it the call goes
	// anonymously, and the hub refuses it.
	if err := state.ForConfig(dir).SaveControllerPAT("pat-stop-suspend"); err != nil {
		t.Fatal(err)
	}

	f := scionOKRunner()
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must call Backend.Stop")
	}
	// A list (the park's phase check), then the suspend.
	if len(f.Calls) != 2 || f.Calls[0].Args[0] != "list" {
		t.Fatalf("expected a list then the suspend, got %+v", f.Calls)
	}
	call := f.Calls[1]
	if call.Name != "scion" || len(call.Args) == 0 || call.Args[0] != "suspend" {
		t.Fatalf("expected `scion suspend ...`, got %+v", call)
	}
	if got := call.Env["SCION_HUB_TOKEN"]; got != "pat-stop-suspend" {
		t.Fatalf("suspend env SCION_HUB_TOKEN = %q, want %q (HubTokenSource dropped)", got, "pat-stop-suspend")
	}
}

const twoWorkersYAML = scratchWorkerYAML + "  - name: idle\n    dir: workers/idle\n"

// stopFleetJSON: the manager and scratch running, idle suspended, and an
// agent the config does not declare.
const stopFleetJSON = `[{"slug":"demo","phase":"running"},{"slug":"scratch","phase":"running"},` +
	`{"slug":"idle","phase":"suspended"},{"slug":"stray","phase":"running"}]`

// TestStopSuspendsRunningWorkers: a worker left running across the power-off
// comes back in phase error and does not resume, so stop suspends every
// configured running worker. Suspended and undeclared agents are left alone.
// Worker-pass failures only warn.
func TestStopSuspendsRunningWorkers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scripts  map[string]string
		wantArgv []string
		wantOut  string
		alsoOut  string // a second line the output must contain, when set
	}{
		{
			name:     "running worker suspended after the manager",
			scripts:  map[string]string{"scion suspend": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"list", "suspend demo", "list", "suspend scratch"},
			wantOut:  `worker "scratch" suspended`,
		},
		{
			name:     "list fails",
			scripts:  map[string]string{"scion suspend": "ok"},
			wantArgv: []string{"list", "suspend demo", "list"},
			wantOut:  "warning: listing agents failed",
		},
		{
			// The manager's suspend failing only warns; the worker pass still runs.
			name: "manager suspend fails",
			// The extra list checks whether the record was already suspended.
			scripts:  map[string]string{"scion suspend scratch": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"list", "suspend demo", "list", "list", "suspend scratch"},
			wantOut:  "warning: scion suspend failed",
			alsoOut:  `worker "scratch" suspended`,
		},
		{
			name:     "worker suspend fails",
			scripts:  map[string]string{"scion suspend demo": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"list", "suspend demo", "list", "suspend scratch"},
			wantOut:  `warning: scion suspend of worker "scratch" failed`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeInstance(t, managerYAML+twoWorkersYAML)
			t.Chdir(dir)
			f := proc.NewFakeRunner()
			for key, out := range tc.scripts {
				f.Script(key, proc.Result{Stdout: out})
			}
			sb := &stubBackend{runner: f}
			root := stubRoot(sb)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"stop"})

			if err := root.Execute(); err != nil {
				t.Fatalf("stop must succeed whatever the worker pass does: %v\n%s", err, out.String())
			}
			if !sb.stopped {
				t.Fatal("stop must power the machine off")
			}
			if len(f.Calls) != len(tc.wantArgv) {
				t.Fatalf("scion calls = %+v, want %v", f.Calls, tc.wantArgv)
			}
			for i, want := range tc.wantArgv {
				if got := f.Calls[i].Argv(); !strings.HasPrefix(got, "scion "+want+" ") && got != "scion "+want {
					t.Fatalf("call %d = %q, want `scion %s …`", i, got, want)
				}
			}
			if !strings.Contains(out.String(), tc.wantOut) || !strings.Contains(out.String(), tc.alsoOut) {
				t.Fatalf("output %q does not contain %q and %q", out.String(), tc.wantOut, tc.alsoOut)
			}
			if !strings.Contains(out.String(), "stopped — disk preserved") {
				t.Fatalf("stop did not report the power-off: %q", out.String())
			}
		})
	}
}

// TestStopDoesNotClearStagedState: unlike destroy, stop keeps the staged
// bootstrap ticket so `lever up` can resume without re-applying.
func TestStopDoesNotClearStagedState(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	tree := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(tree, ".lever", "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up")}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must still call Backend.Stop")
	}
	if _, err := os.Stat(bootstrap); err != nil {
		t.Fatalf("bootstrap.json must survive `lever stop`, stat err = %v", err)
	}
}

// TestStopSkipsSuspendWhenJailUnreachable: an unreachable jail skips the
// suspend and still powers off rather than failing.
func TestStopSkipsSuspendWhenJailUnreachable(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	f := proc.NewFakeRunner() // no scripts: any call would error loudly
	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up"), runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop must still succeed when the jail is unreachable: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must power off even when suspend is skipped")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("suspend must be skipped when ResolveRunUser errors, got calls: %+v", f.Calls)
	}
}

// TestStopAlsoStopsRemoteProxy pins that stop calls StopRemoteProxy; the
// mechanism is tested in internal/brokerctl.
func TestStopAlsoStopsRemoteProxy(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, done := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(st.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.pid should be removed after stop, stat err = %v", err)
	}
	waitStopped(t, done, "the remote proxy")
}

func waitStopped(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not stopped", what)
	}
}

// An explicit --machine must not touch the current instance's host broker.
func TestStopWithExplicitMachineDoesNotStopBroker(t *testing.T) {
	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop", "--machine", "lever-other"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("stop must call Backend.Stop")
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("broker is not stopped")) {
		t.Fatalf("expected a note that the broker is not stopped, got: %q", got)
	}
}

// TestStopHostDaemonsIsQuietWhenNothingRuns: with no broker.pid and no
// remote.pid both stops are no-ops and nothing is warned about.
func TestStopHostDaemonsIsQuietWhenNothingRuns(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errOut)
	stopHostDaemons(cmd, state.ForConfig(t.TempDir()))
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", errOut.String())
	}
}

// fakeSuspender tracks the peak number of suspends in flight.
type fakeSuspender struct {
	agents  []scion.Agent
	suspend map[string]func(ctx context.Context) error

	mu       sync.Mutex
	inFlight int
	peak     int
}

func (f *fakeSuspender) List(context.Context, string) ([]scion.Agent, error) { return f.agents, nil }

func (f *fakeSuspender) Suspend(ctx context.Context, worker, _ string) error {
	f.mu.Lock()
	f.inFlight++
	f.peak = max(f.peak, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if do := f.suspend[worker]; do != nil {
		return do(ctx)
	}
	return nil
}

// The worker suspends run side by side, never more than the pool allows,
// and each error lands at its worker's index.
func TestSuspendWorkersIsBoundedParallel(t *testing.T) {
	slow := func(context.Context) error { time.Sleep(30 * time.Millisecond); return nil }
	f := &fakeSuspender{suspend: map[string]func(context.Context) error{}}
	var names []string
	for i := range 7 {
		name := fmt.Sprintf("w%d", i)
		names = append(names, name)
		f.suspend[name] = slow
	}
	f.suspend["w4"] = func(context.Context) error { time.Sleep(30 * time.Millisecond); return errors.New("w4 failed") }
	errs := suspendWorkers(context.Background(), f, names, "/lever", 3, time.Second)
	if f.peak < 2 || f.peak > 3 {
		t.Fatalf("peak suspends in flight = %d, want 2..3 (pool of 3)", f.peak)
	}
	for i, err := range errs {
		if (err != nil) != (i == 4) {
			t.Fatalf("errs = %v, want only index 4 to fail", errs)
		}
	}
}

// One hung suspend costs only its own timeout: the others finish, and the
// hung one fails with the deadline.
func TestSuspendWorkersTimesOutEachWorker(t *testing.T) {
	f := &fakeSuspender{suspend: map[string]func(context.Context) error{
		"hung": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}}
	start := time.Now()
	errs := suspendWorkers(context.Background(), f, []string{"a", "hung", "b"}, "/lever", 2, 30*time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the pass took %s", d)
	}
	if errs[0] != nil || errs[2] != nil || !errors.Is(errs[1], context.DeadlineExceeded) {
		t.Fatalf("errs = %v, want only hung to fail with the deadline", errs)
	}
}

// The pass reports in config order whatever order the suspends finish in,
// and a failure's text is sanitised before it reaches the terminal.
func TestSuspendRunningWorkersReportsInConfigOrder(t *testing.T) {
	app := &config.App{Workers: []config.Worker{{Name: "first"}, {Name: "second"}, {Name: "third"}, {Name: "asleep"}}}
	f := &fakeSuspender{
		agents: []scion.Agent{
			{Slug: "first", Phase: "running"}, {Slug: "second", Phase: "running"},
			{Slug: "third", Phase: "resumed"}, {Slug: "asleep", Phase: "suspended"},
		},
		suspend: map[string]func(context.Context) error{
			"first":  func(context.Context) error { time.Sleep(40 * time.Millisecond); return nil },
			"second": func(context.Context) error { return errors.New("hub said \x1b[31mno\x1b[0m") },
		},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	suspendRunningWorkers(cmd, f, app, "/lever")
	got := out.String()
	i1, i2, i3 := strings.Index(got, `"first"`), strings.Index(got, `"second"`), strings.Index(got, `"third"`)
	if i1 < 0 || i2 < 0 || i3 < 0 || i1 > i2 || i2 > i3 {
		t.Fatalf("output not in config order:\n%s", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("an escape sequence reached the output: %q", got)
	}
	if !strings.Contains(got, `warning: scion suspend of worker "second" failed`) || strings.Contains(got, `"asleep"`) {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

// reportProbe is an apply.AgentSessionProbe for parkManager's heal: a valid
// token, a live harness, and a count of session reports.
type reportProbe struct{ reports int }

func (p *reportProbe) HubToken(context.Context, string) (jail.HubTokenTimes, error) {
	now := time.Now()
	return jail.HubTokenTimes{Expiry: now.Add(time.Hour * 5), Now: now}, nil
}
func (p *reportProbe) HarnessAlive(context.Context, string) (bool, error) { return true, nil }
func (p *reportProbe) ReportSessionRunning(context.Context, string) error {
	p.reports++
	return nil
}

// TestParkHealsAStoppedManagerBeforeSuspend: a manager whose hub phase reads
// stopped while its claude still runs is reported running before `lever
// stop` suspends it, so the next `lever up` resumes the conversation; any
// other phase is left to the suspend alone.
func TestParkHealsAStoppedManagerBeforeSuspend(t *testing.T) {
	for _, tc := range []struct {
		list        string
		wantReports int
	}{
		{`[{"slug":"demo","phase":"stopped","containerStatus":"Up 2 hours"}]`, 1},
		{`[{"slug":"demo","phase":"running","containerStatus":"Up 2 hours"}]`, 0},
		{`[{"slug":"demo","phase":"stopped","containerStatus":"stopped"}]`, 0},
		{`[]`, 0},
	} {
		f := proc.NewFakeRunner()
		f.Script("scion list", proc.Result{Stdout: tc.list})
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetErr(&out)
		p := &reportProbe{}
		_ = parkManager(context.Background(), cmd, &config.App{Name: "demo", Tree: t.TempDir()}, scion.New(f, scion.Options{}), p, nil, nil, "/lever")
		if p.reports != tc.wantReports {
			t.Fatalf("%s: reports = %d, want %d (%s)", tc.list, p.reports, tc.wantReports, out.String())
		}
		if tc.wantReports > 0 && !strings.Contains(out.String(), "lever stop: agent \"demo\"") {
			t.Fatalf("%s: output %q", tc.list, out.String())
		}
	}
}

// blockingProbe's harness probe hangs until its context ends, like a hung
// podman exec.
type blockingProbe struct{ deadline time.Time }

func (p *blockingProbe) HubToken(context.Context, string) (jail.HubTokenTimes, error) {
	now := time.Now()
	return jail.HubTokenTimes{Expiry: now.Add(5 * time.Hour), Now: now}, nil
}
func (p *blockingProbe) HarnessAlive(ctx context.Context, _ string) (bool, error) {
	p.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return false, ctx.Err()
}
func (p *blockingProbe) ReportSessionRunning(context.Context, string) error { return nil }

type ctxRecordingRunner struct {
	*proc.FakeRunner
	suspendCtxErr []error
}

func (r *ctxRecordingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if len(args) > 0 && args[0] == "suspend" {
		r.suspendCtxErr = append(r.suspendCtxErr, ctx.Err())
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}
func (r *ctxRecordingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestStopHealCannotEatTheSuspendBudget: a heal that hangs runs out its own
// budget, and the manager's suspend still runs with a live context.
func TestStopHealCannotEatTheSuspendBudget(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)
	oldBudget, oldProbe := stopHealBudget, stopSessionProbe
	p := &blockingProbe{}
	stopHealBudget = 50 * time.Millisecond
	stopSessionProbe = func(proc.Runner) apply.AgentSessionProbe { return p }
	t.Cleanup(func() { stopHealBudget, stopSessionProbe = oldBudget, oldProbe })

	f := proc.NewFakeRunner()
	f.Script("scion list", proc.Result{Stdout: `[{"slug":"demo","phase":"stopped","containerStatus":"Up 2 hours"}]`})
	f.Script("scion suspend", proc.Result{Stdout: "ok"})
	r := &ctxRecordingRunner{FakeRunner: f}
	sb := &stubBackend{runner: r}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})
	start := time.Now()
	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v\n%s", err, out.String())
	}
	if p.deadline.IsZero() || p.deadline.Sub(start) > 5*time.Second {
		t.Fatalf("the heal ran without its own short budget (deadline %v)", p.deadline)
	}
	if len(r.suspendCtxErr) != 1 || r.suspendCtxErr[0] != nil {
		t.Fatalf("suspend calls / ctx errors = %v, want one live-context suspend", r.suspendCtxErr)
	}
}

// expiredThenReportProbe: the token reads expired until a reset-auth has run,
// and the session report records whether the reset came first.
type expiredThenReportProbe struct {
	f                *proc.FakeRunner
	reportAfterReset []bool
}

func (p *expiredThenReportProbe) reset() bool {
	return p.f.Called(proc.ArgvContains("reset-auth demo"))
}
func (p *expiredThenReportProbe) HubToken(context.Context, string) (jail.HubTokenTimes, error) {
	now := time.Now()
	if p.reset() {
		return jail.HubTokenTimes{Expiry: now.Add(10 * time.Hour), Now: now}, nil
	}
	return jail.HubTokenTimes{Expiry: now.Add(-time.Hour), Now: now}, nil
}
func (p *expiredThenReportProbe) HarnessAlive(context.Context, string) (bool, error) {
	return true, nil
}
func (p *expiredThenReportProbe) ReportSessionRunning(context.Context, string) error {
	p.reportAfterReset = append(p.reportAfterReset, p.reset())
	return nil
}

// TestParkResetsAnExpiredTokenFirst: an expired token is reset
// (behind the role guard) before the session report, which the hub would
// otherwise refuse; a guard refusal resets nothing.
func TestParkResetsAnExpiredTokenFirst(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		f := proc.NewFakeRunner()
		f.Script("scion list", proc.Result{Stdout: `[{"slug":"demo","phase":"stopped","containerStatus":"Up 2 hours"}]`})
		f.Script("scion reset-auth", proc.Result{Stdout: "ok"})
		p := &expiredThenReportProbe{f: f}
		var guarded []string
		verify := func(_ context.Context, project, agent string) error {
			guarded = append(guarded, project+"/"+agent)
			if refuse {
				return errors.New("record stores no role")
			}
			return nil
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetErr(&out)
		_ = parkManager(context.Background(), cmd, &config.App{Name: "demo", Tree: t.TempDir()}, scion.New(f, scion.Options{}), p, verify, nil, "/lever")
		if len(guarded) != 1 || guarded[0] != "lever/demo" {
			t.Fatalf("refuse=%v: guard calls %v", refuse, guarded)
		}
		if refuse {
			if p.reset() || !strings.Contains(out.String(), "NOT reset") {
				t.Fatalf("a refused guard must reset nothing: %q", out.String())
			}
			continue
		}
		if len(p.reportAfterReset) != 1 || !p.reportAfterReset[0] {
			t.Fatalf("the session report must follow the reset: %v (%s)", p.reportAfterReset, out.String())
		}
	}
}

// TestStopKeepVM: --keep-vm suspends the manager and stops the hub and host
// daemons but never powers off. The suspend comes first: it needs the hub.
func TestStopKeepVM(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy, proxyDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(fmt.Sprintf("%d\n", proxy.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	f := scionOKRunner()
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop", "--keep-vm"})

	if err := root.Execute(); err != nil {
		t.Fatalf("stop --keep-vm: %v", err)
	}
	if sb.stopped || sb.down {
		t.Fatal("--keep-vm must leave the VM running")
	}
	suspend := f.CallIndex(proc.Subcommand("scion", "suspend"))
	hubStop := f.CallIndex(proc.ArgvPrefix("scion", "server", "stop"))
	if suspend < 0 || hubStop < 0 || suspend > hubStop {
		t.Fatalf("want the suspend, then the hub stop; got %+v", f.Calls)
	}
	if _, err := os.Stat(st.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host daemons must be stopped too: remote.pid stat err = %v", err)
	}
	waitStopped(t, proxyDone, "the remote proxy")
	got := out.String()
	for _, want := range []string{"left running", "keeps its memory on the host", "`lever up`"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// A hub stop that fails is a warning: the agents are suspended, the VM is
// left running, and the command succeeds.
func TestStopKeepVMHubStopFailureWarns(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	if err := stopKeepVM(cmd, failingHub{}, "lever-demo", true); err != nil {
		t.Fatalf("stopKeepVM: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "warning: stopping the jail's hub failed") || !strings.Contains(got, "left running") {
		t.Errorf("output:\n%s", got)
	}
}

type failingHub struct{}

func (failingHub) ServerStop(context.Context) error { return errors.New("scion: no answer") }

// With the jail not answering, --keep-vm suspended nothing: it never powers
// off and fails naming plain `lever stop` (an up VM still runs its agents).
func TestStopKeepVMJailUnreachable(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	f := proc.NewFakeRunner()
	sb := &stubBackend{resolveRunUserErr: errors.New("machine not up"), runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop", "--keep-vm"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "no agent was suspended") || !strings.Contains(err.Error(), "`lever stop`") {
		t.Fatalf("err = %v", err)
	}
	if sb.stopped || len(f.Calls) != 0 {
		t.Fatalf("stopped=%t calls=%+v", sb.stopped, f.Calls)
	}
}

// --keep-vm acts on the current instance only: with --machine, or outside
// an instance, it refuses before touching anything.
func TestStopKeepVMRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"machine", []string{"stop", "--keep-vm", "--machine", "lever-other"}, "without --machine"},
		{"no instance", []string{"stop", "--keep-vm"}, "needs an instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			sb := &stubBackend{}
			root := stubRoot(sb)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if sb.stopped || sb.resolvedRunUser {
				t.Fatal("a refused --keep-vm must touch nothing")
			}
		})
	}
}

// A plain stop after `lever stop --keep-vm` finds the hub down: the
// suspend warning names that case, and the VM is powered off.
func TestStopAfterKeepVMHubDown(t *testing.T) {
	dir := writeInstance(t, twoWorkersYAML)
	t.Chdir(dir)

	f := hubDownRunner{proc.NewFakeRunner()}
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop"})
	if err := root.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !sb.stopped {
		t.Fatal("the VM must still be powered off")
	}
	got := out.String()
	if !strings.Contains(got, "warning: scion suspend failed: the jail's hub did not answer (after `lever stop --keep-vm`") {
		t.Errorf("output:\n%s", got)
	}
}

type hubDownRunner struct{ *proc.FakeRunner }

func (r hubDownRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r hubDownRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" {
		_, _ = r.FakeRunner.RunIn(ctx, dir, env, name, args...)
		return proc.Result{Code: 1, Stderr: "Error: dial tcp 127.0.0.1:8080: connect: connection refused"}, errors.New("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// A failed suspend under --keep-vm fails the command: that container goes on
// running in the VM.
func TestStopKeepVMSuspendFailureFails(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	f := suspendFailsRunner{proc.NewFakeRunner()}
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"stop", "--keep-vm"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not every agent was suspended") {
		t.Fatalf("err = %v", err)
	}
	if sb.stopped {
		t.Fatal("--keep-vm must leave the VM running")
	}
	if !f.Called(proc.ArgvPrefix("scion", "server", "stop")) {
		t.Errorf("the hub must still be stopped: %+v", f.Calls)
	}
	if strings.Contains(out.String(), "agents suspended") {
		t.Errorf("must not claim the agents are suspended:\n%s", out.String())
	}
}

type suspendFailsRunner struct{ *proc.FakeRunner }

func (r suspendFailsRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r suspendFailsRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, _ := r.FakeRunner.RunIn(ctx, dir, env, name, args...)
	if name != "scion" {
		return res, nil
	}
	if len(args) > 0 && args[0] == "suspend" {
		return proc.Result{Code: 1, Stderr: "Error: the hub said no"}, errors.New("exit status 1")
	}
	return proc.Result{Stdout: "ok"}, nil
}

// TestReportManagerSuspendErrorQuietWhenSuspended: a suspend refused because
// the record is already suspended is a note, not a warning; any other
// failure still warns.
func TestReportManagerSuspendErrorQuietWhenSuspended(t *testing.T) {
	for _, c := range []struct {
		phase string
		warn  bool
	}{{scion.PhaseSuspended, false}, {scion.PhaseRunning, true}, {"", true}} {
		var out, errOut strings.Builder
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetContext(context.Background())
		f := &fakeSuspender{}
		if c.phase != "" {
			f.agents = []scion.Agent{{Slug: "demo", Phase: c.phase}}
		}
		suspended := reportManagerSuspendError(cmd, f, "demo", "/lever", errors.New("cannot suspend: phase suspended"))
		if suspended == c.warn {
			t.Errorf("phase %q: reported suspended=%v", c.phase, suspended)
		}
		if got := strings.Contains(errOut.String(), "warning"); got != c.warn {
			t.Errorf("phase %q: warned=%v (stderr %q, stdout %q)", c.phase, got, errOut.String(), out.String())
		}
	}
}

// TestStopEndsTheBrokersHold: while the broker holds the lifecycle lock (a
// heal of the manager the manager can draw out), stop stops the broker
// first, which ends its hold; any other holder still turns stop away.
func TestStopEndsTheBrokersHold(t *testing.T) {
	for _, c := range []struct {
		holder      string
		stopsBroker bool
	}{
		{"the broker's certificate heal of the manager (pid 1)", true},
		{"`lever up` (pid 1, since 10:00:00 UTC)", false},
	} {
		dir := writeInstance(t, managerYAML)
		t.Chdir(dir)
		st := state.ForConfig(dir)
		if err := os.MkdirAll(st.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, err := hostledger.TryLockFileAs(st.ConversationLock(), c.holder)
		if err != nil {
			t.Fatal(err)
		}
		if got := brokerHoldsLock(st); got != c.stopsBroker {
			t.Fatalf("%s: brokerHoldsLock = %v", c.holder, got)
		}
		sb := &stubBackend{runner: scionOKRunner()}
		root := stubRoot(sb)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"stop"})
		// The lock stays held here (this process holds it, not a broker), so
		// stop is still refused; what differs is that it tried the broker.
		if err := root.Execute(); err == nil || sb.stopped {
			t.Fatalf("%s: stop went on under a held lock: %v", c.holder, err)
		}
		if strings.Contains(out.String(), "stopping the broker first") != c.stopsBroker {
			t.Fatalf("%s: output %q", c.holder, out.String())
		}
		unlock()
	}
}
