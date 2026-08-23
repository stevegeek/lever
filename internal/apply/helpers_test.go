package apply

import (
	"errors"
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

// wantErrIs fails the test unless errors.Is(err, target).
func wantErrIs(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error %v does not wrap %v", err, target)
	}
}

// wantErrContaining fails the test unless err is non-nil and its message
// contains every substr.
func wantErrContaining(t testing.TB, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want one containing %q", substrs)
		return
	}
	for _, s := range substrs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q does not contain %q", err, s)
			return
		}
	}
}
