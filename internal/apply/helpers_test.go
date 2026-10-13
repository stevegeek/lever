package apply

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

const testHubEndpoint = "http://127.0.0.1:8080"

// helloApp's name "hello" matches agentLifecycleRunner's slug.
func helloApp(tree string) *config.App {
	return &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
}

// scionOKRunner answers every scion argv with "ok", so only the verbs a test
// intercepts need scripting.
func scionOKRunner() *proc.FakeRunner {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	return f
}

func hubScion(f *proc.FakeRunner, app *config.App) *scion.Client {
	return scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{HubEndpoint: testHubEndpoint})
}

// joinedCalls renders recorded argv as "a b|c d|" so a test can assert one
// exact scion call with strings.Contains.
func joinedCalls(f *proc.FakeRunner) string {
	var b strings.Builder
	for _, c := range f.Calls {
		b.WriteString(strings.Join(c.Args, " "))
		b.WriteString("|")
	}
	return b.String()
}

// logSink is safe for the steps Run runs concurrently.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func fastRetry(n int) RetryBudget { return RetryBudget{Attempts: n, Interval: time.Millisecond} }

// absentThenRunningList reports slug absent on the first List (so
// start-manager takes the create path) and running on every later one.
func absentThenRunningList(f *proc.FakeRunner, listCalls *int, slug string, c proc.Call) (proc.Result, error) {
	*listCalls++
	f.Calls = append(f.Calls, c)
	if *listCalls == 1 {
		return proc.Result{Stdout: "[]"}, nil
	}
	return proc.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":"running","containerStatus":"running"}]`, slug)}, nil
}

func countRearm(n *int) func(context.Context) error {
	return func(context.Context) error {
		*n++
		return nil
	}
}

func sawScionCall(f *proc.FakeRunner, fragment string) bool {
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), fragment) {
			return true
		}
	}
	return false
}
