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
	scriptRunningHub(f, "")
	return f
}

// runningHubKey is the FakeRunner key of scion.Client.RunningHub's probe,
// read off the call itself: the pid-file probe shares the script's first
// lines and must stay unanswered.
var runningHubKey = func() string {
	f := proc.NewFakeRunner()
	_, _ = scion.New(f, scion.Options{}).RunningHub(context.Background())
	if len(f.Calls) != 1 || f.Calls[0].Name != "sh" || len(f.Calls[0].Args) < 2 {
		panic(fmt.Sprintf("RunningHub probe call: %+v", f.Calls))
	}
	return "sh " + f.Calls[0].Args[0] + " " + f.Calls[0].Args[1]
}()

// scriptRunningHub answers RunningHub's probe with out ("" is no hub).
func scriptRunningHub(f *proc.FakeRunner, out string) {
	f.Script(runningHubKey, proc.Result{Stdout: out})
}

// runningHubOut renders the probe output of a hub started with argv and env.
func runningHubOut(pid int, argv []string, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", pid)
	for _, a := range argv {
		b.WriteString(a + "\n")
	}
	b.WriteString("--lever-env--\n")
	for k, v := range env {
		b.WriteString(k + "=" + v + "\n")
	}
	return b.String()
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
	return proc.Result{Stdout: fmt.Sprintf(`[{"slug":%q,"phase":"running","containerStatus":"running","image":"lever.local/img:amd64"}]`, slug)}, nil
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

// WaitManagerLive is the full gate (live, harness running, settle window)
// for a manager just resumed, with the default budget and the harness-death
// hint, as EnsureManager's gateResumed runs it. prog may be nil.
func WaitManagerLive(ctx context.Context, d Deps, name, project string, prog *Progress) error {
	err := gateManagerLive(ctx, d, d.ManagerLiveRetry.or(defaultManagerLiveRetry), name, project, "up", prog, gateOpts{})
	return withHarnessDeathHint(ctx, d, err, project, name)
}
