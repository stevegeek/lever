package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// One autostart step per worker that sets autostart, last; none in a
// broker-only plan.
func TestPlanAutostartSteps(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
		Workers: []config.Worker{{Name: "a", Dir: "w/a", Autostart: true}, {Name: "b", Dir: "w/b"}, {Name: "c", Dir: "w/c", Autostart: true}},
		Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"},
	}
	steps := Plan(app, PlanOpts{})
	got := planStepNames(steps)
	want := []string{"broker-up", "load-image", "init-machine", "config-registry", "scion-server", "hub-identity", "register-project", "agent-template", "mint-manager-bootstrap", "start-manager", "remote-proxy", "worker-autostart", "worker-autostart"}
	if !slices.Equal(got, want) {
		t.Fatalf("plan = %v\nwant   %v", got, want)
	}
	if steps[11].Target != "a" || steps[12].Target != "c" || stepLabel(steps[11]) != "worker-autostart a" {
		t.Fatalf("targets %q %q label %q", steps[11].Target, steps[12].Target, stepLabel(steps[11]))
	}
	if contains(planStepNames(Plan(app, PlanOpts{BrokerOnly: true})), "worker-autostart") {
		t.Fatal("a broker-only plan starts no worker")
	}
}

// The step asks for each autostart worker once the manager is up; a failure
// warns, names the worker and the retry, and the run still succeeds.
func TestRunAutostartWarnsAndContinues(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Workers = []config.Worker{{Name: "fw1", Dir: "w/fw1", Autostart: true}, {Name: "fw2", Dir: "w/fw2", Autostart: true}, {Name: "idle", Dir: "w/idle"}}
	var asked []string
	var logs []string
	deps := Deps{
		Scion: hubScion(f, app),
		AutostartWorker: func(_ context.Context, w string) error {
			asked = append(asked, w)
			if w == "fw1" {
				return errors.New("too many workers are running (reason worker-cap)")
			}
			return nil
		},
		Log: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(asked, []string{"fw1", "fw2"}) {
		t.Fatalf("asked %v", asked)
	}
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, `autostart of worker "fw1" failed`) || !strings.Contains(all, "worker-cap") || !strings.Contains(all, "lever worker start fw1") {
		t.Fatalf("no warning naming fw1:\n%s", all)
	}
	if strings.Contains(all, `"fw2" failed`) {
		t.Fatalf("fw2 did not fail:\n%s", all)
	}
}

// Without a way to reach the broker the step warns and the run succeeds.
func TestRunAutostartUnwired(t *testing.T) {
	f := scionOKRunner()
	app := helloApp(t.TempDir())
	app.Workers = []config.Worker{{Name: "fw1", Dir: "w/fw1", Autostart: true}}
	var logs []string
	deps := Deps{Scion: hubScion(f, app), Log: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "lever worker start fw1") {
		t.Fatalf("logs %v", logs)
	}
}
