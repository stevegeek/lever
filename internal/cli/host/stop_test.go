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
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
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
	if len(f.Calls) != 1 {
		t.Fatalf("expected exactly one scion call (suspend), got %+v", f.Calls)
	}
	call := f.Calls[0]
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
		alsoOut  string // a second line the output must contain, when set
	}{
		{
			name:     "running worker suspended after the manager",
			scripts:  map[string]string{"scion suspend": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"suspend demo", "list", "suspend scratch"},
			wantOut:  `worker "scratch" suspended`,
		},
		{
			name:     "list fails",
			scripts:  map[string]string{"scion suspend": "ok"},
			wantArgv: []string{"suspend demo", "list"},
			wantOut:  "warning: listing agents failed",
		},
		{
			// The manager's suspend failing is a warning too: the worker pass
			// still runs under its own budget, then the power-off.
			name:     "manager suspend fails",
			scripts:  map[string]string{"scion suspend scratch": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"suspend demo", "list", "suspend scratch"},
			wantOut:  "warning: scion suspend failed",
			alsoOut:  `worker "scratch" suspended`,
		},
		{
			name:     "worker suspend fails",
			scripts:  map[string]string{"scion suspend demo": "ok", argvScionList: stopFleetJSON},
			wantArgv: []string{"suspend demo", "list", "suspend scratch"},
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

// fakeSuspender is a workerSuspender: List answers agents; Suspend runs
// suspend for the worker (nil = succeed at once). It tracks the peak number
// of suspends in flight.
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
