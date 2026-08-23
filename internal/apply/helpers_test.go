package apply

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// testHubEndpoint is the hub address the Run tests point scion.Client at.
const testHubEndpoint = "http://127.0.0.1:8080"

// helloApp returns the minimal app every Run test shares: name "hello"
// (matching agentLifecycleRunner's slug), orbstack, one manager image.
// Tests that need more set the extra fields on the result.
func helloApp(tree string) *config.App {
	return &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
}

// scionOKRunner returns a FakeRunner whose blanket script answers every
// scion argv with "ok", so only the verbs a test intercepts need scripting.
func scionOKRunner() *proc.FakeRunner {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	return f
}

// hubScion wraps f in an agentLifecycleRunner for app's manager slug and
// builds the scion client the real apply would, pointed at testHubEndpoint.
func hubScion(f *proc.FakeRunner, app *config.App) *scion.Client {
	return scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: testHubEndpoint})
}

// joinedCalls renders f's recorded argv as "a b|c d|" so a test can assert
// that one exact scion call happened with strings.Contains.
func joinedCalls(f *proc.FakeRunner) string {
	var b strings.Builder
	for _, c := range f.Calls {
		b.WriteString(strings.Join(c.Args, " "))
		b.WriteString("|")
	}
	return b.String()
}

// logSink collects Deps.Log lines; wire it with Log: sink.logf.
type logSink struct{ lines []string }

func (s *logSink) logf(format string, args ...any) {
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

// setRetryBudget overrides one of the package's (attempts, interval) retry
// pairs for the test — n attempts, a millisecond apart — and restores it.
func setRetryBudget(t *testing.T, attempts *int, interval *time.Duration, n int) {
	t.Helper()
	origAtt, origInt := *attempts, *interval
	*attempts, *interval = n, time.Millisecond
	t.Cleanup(func() { *attempts, *interval = origAtt, origInt })
}

// absentThenRunningList records the observe-List call on f and answers it:
// the first call reports slug absent (so start-manager takes the create path),
// every later call reports it running/running (so the liveness verify
// converges). listCalls is the caller's counter.
func absentThenRunningList(f *proc.FakeRunner, listCalls *int, slug string, c proc.Call) (proc.Result, error) {
	*listCalls++
	f.Calls = append(f.Calls, c)
	if *listCalls == 1 {
		return proc.Result{Stdout: "[]"}, nil
	}
	return proc.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":"running","containerStatus":"running"}]`, slug)}, nil
}

// countRearm returns a RearmBootstrap that increments *n and succeeds.
func countRearm(n *int) func(context.Context) error {
	return func(context.Context) error {
		*n++
		return nil
	}
}

// sawScionCall reports whether any recorded argv on f contains fragment
// (e.g. "init --non-interactive") as a space-joined substring.
func sawScionCall(f *proc.FakeRunner, fragment string) bool {
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), fragment) {
			return true
		}
	}
	return false
}
