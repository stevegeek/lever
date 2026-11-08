package scion

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// stubCurlRunner runs commands for real with a stub curl first on PATH.
type stubCurlRunner struct {
	proc.RealRunner
	path string
}

func (r stubCurlRunner) Run(ctx context.Context, _ map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RealRunner.Run(ctx, map[string]string{"PATH": r.path}, name, args...)
}

// withStubCurl puts a curl whose body is script first on PATH.
func withStubCurl(t *testing.T, script string) stubCurlRunner {
	t.Helper()
	for _, tool := range []string{"sh", "date", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubCurlRunner{path: dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// hangingCurl is a hub that accepts and never answers: curl runs until its
// --max-time and fails with 28, as the real one does.
const hangingCurl = `mt=2
while [ $# -gt 0 ]; do
  [ "$1" = "--max-time" ] && mt=$2
  shift
done
sleep "$mt"
exit 28`

// Against a hub that accepts and hangs, the in-guest wait ends within about
// its window, not after one 2 s request per 150 ms step of it (a 2 s window
// used to run for about 30 s).
func TestHealthzWaitEndsWithinItsWindowAgainstAHangingHub(t *testing.T) {
	r := withStubCurl(t, hangingCurl)
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080"})
	const d = 2 * time.Second
	start := time.Now()
	got := c.awaitHealthz(context.Background(), d)
	elapsed := time.Since(start)
	if got != healthzDown {
		t.Fatalf("result %v, want healthzDown", got)
	}
	if elapsed > d+2*time.Second {
		t.Fatalf("the %s wait ran %s", d, elapsed.Round(time.Millisecond))
	}
}

// A hub that answers 200 ends the wait at once.
func TestHealthzWaitEndsWhenTheHubAnswers(t *testing.T) {
	r := withStubCurl(t, "exit 0")
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080"})
	start := time.Now()
	if got := c.awaitHealthz(context.Background(), 5*time.Second); got != healthzUp {
		t.Fatalf("result %v, want healthzUp", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("an answering hub took %s", elapsed)
	}
}

// deadlineRunner records the deadline of the in-guest wait's call.
type deadlineRunner struct {
	*proc.FakeRunner
	deadline time.Time
	has      bool
}

func (r *deadlineRunner) Run(ctx context.Context, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	r.deadline, r.has = ctx.Deadline()
	return proc.Result{}, nil
}

func (r *deadlineRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.Run(ctx, env, name, args...)
}

// The jail call itself is bounded too, in case the guest's shell or clock
// misbehaves: the window plus healthzCallSlack.
func TestHealthzWaitCallIsBounded(t *testing.T) {
	r := &deadlineRunner{FakeRunner: proc.NewFakeRunner()}
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080"})
	const d = 3 * time.Second
	start := time.Now()
	c.awaitHealthz(context.Background(), d)
	if got := r.deadline.Sub(start); !r.has || got > d+healthzCallSlack+time.Second || got < d+healthzCallSlack-time.Second {
		t.Fatalf("call deadline %v (set %v), want within %s", r.deadline.Sub(start), r.has, d+healthzCallSlack)
	}
}

func TestHealthzWindowRoundsUp(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "1", 100 * time.Millisecond: "1", time.Second: "1", 1500 * time.Millisecond: "2", 5 * time.Second: "5",
	} {
		if got := healthzWindow(d); got != want {
			t.Errorf("healthzWindow(%s) = %s, want %s", d, got, want)
		}
	}
}
