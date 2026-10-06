package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// TestStopSuspendsManager verifies the happy path: with a reachable jail,
// `lever stop` SUSPENDS the manager (best-effort, via scion) before powering
// the machine off. It must be `scion suspend`, not `scion stop`: the
// conversation is durable (the agent home is a persistent bind-mount, and
// scion resume relaunches the harness with `claude --continue`, restoring
// the session — live-proven 2026-07-04), so suspend keeps the record
// resumable for the next `lever up`, while `scion stop` would REMOVE the
// container and discard the session.
func TestStopSuspendsManager(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	// Seed the controller PAT so the suspend client's HubTokenSource resolves it:
	// this guards that stop's scion client keeps its HubTokenSource wiring (a
	// dropped source would authenticate anonymously against the dev-auth-off hub).
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
	// A list (is the manager's phase stopped over a live harness? see
	// healStoppedManager), then the suspend.
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

// twoWorkersYAML declares the workers the worker-suspend tests list: scratch
// and idle.
const twoWorkersYAML = scratchWorkerYAML + "  - name: idle\n    dir: workers/idle\n"

// stopFleetJSON is a `scion list --format json` answer: the manager and
// scratch running, idle suspended, and an agent the config does not declare.
const stopFleetJSON = `[{"slug":"demo","phase":"running"},{"slug":"scratch","phase":"running"},` +
	`{"slug":"idle","phase":"suspended"},{"slug":"stray","phase":"running"}]`

// TestStopSuspendsRunningWorkers: after the manager, `lever stop` suspends
// every configured worker the hub shows running — a worker left running
// across the power-off comes back in phase error and does not resume. A
// suspended worker and an undeclared agent are left alone. Every failure of
// the worker pass is a warning: the machine is powered off regardless.
func TestStopSuspendsRunningWorkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scripts map[string]string
		// wantArgv are the leading words of every scion call, in order.
		wantArgv []string
		wantOut  string
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
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tc.wantOut)
			}
			if !strings.Contains(out.String(), "stopped — disk preserved") {
				t.Fatalf("stop did not report the power-off: %q", out.String())
			}
		})
	}
}

// TestStopDoesNotClearStagedState is the behavioral contrast with `destroy`:
// stop must preserve the staged bootstrap ticket + manifest so a following
// `lever up` can resume fast, without re-applying.
func TestStopDoesNotClearStagedState(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	tree := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(tree, ".lever", "bootstrap.json")
	manifest := filepath.Join(tree, config.ManifestName)
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	// Jail unreachable: skips the suspend branch entirely, isolating this test
	// to the staged-state behavior.
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
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest must survive `lever stop`, stat err = %v", err)
	}
}

// TestStopSkipsSuspendWhenJailUnreachable covers the DECISION documented in
// stop.go: if ResolveRunUser fails (jail unreachable — already halted, or
// never came up), stop skips the best-effort suspend and still proceeds to
// power off, rather than failing the command.
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

// TestStopAlsoStopsRemoteProxy proves `lever stop` tears the remote-access
// proxy down alongside the broker: a live pid recorded in remote.pid must be
// killed and the pid file removed (state.State.StopRemoteProxy mirrors
// StopBroker exactly — see its doc; the mechanism itself is unit-tested in
// internal/brokerctl, this only pins that stop.go actually calls it).
func TestStopAlsoStopsRemoteProxy(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
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
	_ = cmd.Wait()
}

// TestStopWithExplicitMachineDoesNotStopBroker mirrors destroy's --machine
// escape hatch: targeting an explicit machine must not touch the host broker
// for the current instance.
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

// reportProbe is an apply.AgentSessionProbe for healStoppedManager: a valid
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

// TestHealStoppedManagerBeforeSuspend: a manager whose hub phase reads
// stopped while its claude still runs is reported running before `lever
// stop` suspends it, so the next `lever up` resumes the conversation; any
// other phase is left to the suspend alone.
func TestHealStoppedManagerBeforeSuspend(t *testing.T) {
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
		healStoppedManager(context.Background(), cmd, scion.New(f, scion.Options{}), p, "demo", "/lever")
		if p.reports != tc.wantReports {
			t.Fatalf("%s: reports = %d, want %d (%s)", tc.list, p.reports, tc.wantReports, out.String())
		}
		if tc.wantReports > 0 && !strings.Contains(out.String(), "lever stop: agent \"demo\"") {
			t.Fatalf("%s: output %q", tc.list, out.String())
		}
	}
}

// blockingProbe's harness probe waits for its context to end, like a hung
// podman exec; it records that context's deadline.
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

// ctxRecordingRunner records, for every scion suspend, whether its context
// was already done when the call ran.
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
