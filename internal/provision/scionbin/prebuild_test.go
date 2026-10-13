package scionbin_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// TestStartPrebuildSkipsWhatNeedsNoBuild: a prebuilt binary needs no
// build, and a spec Validate refuses is left to the scion step to report.
func TestStartPrebuildSkipsWhatNeedsNoBuild(t *testing.T) {
	f := proc.NewFakeRunner()
	for _, spec := range []scionbin.Spec{{Binary: "/x/scion"}, {}, {Source: "/does/not/exist"}} {
		if p := scionbin.StartPrebuild(context.Background(), f, spec, "arm64", "m"); p != nil {
			p.Stop()
			t.Fatalf("spec %+v started a build", spec)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("calls %+v", f.Calls)
	}
	var p *scionbin.Prebuild
	if _, _, ok := p.Result(context.Background(), scionbin.Spec{}, "arm64", "m"); ok {
		t.Fatal("a nil Prebuild has a result")
	}
	p.Stop()
}

// TestPrebuildResultIsTakenForTheSameArch: the scion step takes the early
// build when the guest's architecture, the spec and the machine match.
func TestPrebuildResultIsTakenForTheSameArch(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	spec := scionbin.Spec{Source: t.TempDir()}
	p := scionbin.StartPrebuild(context.Background(), f, spec, "arm64", "m")
	defer p.Stop()
	path, err, ok := p.Result(context.Background(), spec, "arm64", "m")
	if !ok || err != nil {
		t.Fatalf("Result: %q %v %v", path, err, ok)
	}
	want, _ := scionbin.OutputPath("m")
	if path != want {
		t.Fatalf("path %q, want %q", path, want)
	}
	if n := len(f.Calls); n != 1 || f.Calls[0].Env["GOARCH"] != "arm64" {
		t.Fatalf("calls %+v", f.Calls)
	}
}

// TestPrebuildForAnotherArchIsNotTaken: a guest whose architecture is not
// the assumed one, or another spec, gets no result (the caller builds).
func TestPrebuildForAnotherArchIsNotTaken(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	spec := scionbin.Spec{Source: t.TempDir()}
	p := scionbin.StartPrebuild(context.Background(), f, spec, "arm64", "m")
	defer p.Stop()
	if _, _, ok := p.Result(context.Background(), spec, "amd64", "m"); ok {
		t.Fatal("an arm64 build was taken for an amd64 guest")
	}
	if _, _, ok := p.Result(context.Background(), scionbin.Spec{Source: t.TempDir()}, "arm64", "m"); ok {
		t.Fatal("a build of another source was taken")
	}
}

// TestPrebuildErrorReachesTheScionStep: a failed early build is the scion
// step's error.
func TestPrebuildErrorReachesTheScionStep(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner() // go build is unscripted: it fails
	spec := scionbin.Spec{Source: t.TempDir()}
	p := scionbin.StartPrebuild(context.Background(), f, spec, "arm64", "m")
	defer p.Stop()
	_, err, ok := p.Result(context.Background(), spec, "arm64", "m")
	if !ok || !errors.Is(err, proc.ErrUnscripted) {
		t.Fatalf("Result: %v %v", err, ok)
	}
}

type blockingRunner struct{ started chan struct{} }

func (b blockingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return b.RunIn(ctx, "", env, name, args...)
}

func (b blockingRunner) RunIn(ctx context.Context, _ string, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	close(b.started)
	<-ctx.Done()
	return proc.Result{}, ctx.Err()
}

func (b blockingRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return b.RunIn(ctx, "", env, name, args...)
}

// TestPrebuildStopCancelsAnUntakenBuild: a bring-up that fails before its
// scion step cancels the build and waits for it.
func TestPrebuildStopCancelsAnUntakenBuild(t *testing.T) {
	backendtest.IsolateCache(t)
	r := blockingRunner{started: make(chan struct{})}
	p := scionbin.StartPrebuild(context.Background(), r, scionbin.Spec{Source: t.TempDir()}, "arm64", "m")
	<-r.started
	p.Stop() // returns only once the build has ended
}

// TestPrebuildForAnotherArchIsCancelled: a guest of another architecture
// does not wait out the early build; it is cancelled and ended before
// Result returns.
func TestPrebuildForAnotherArchIsCancelled(t *testing.T) {
	backendtest.IsolateCache(t)
	r := blockingRunner{started: make(chan struct{})}
	spec := scionbin.Spec{Source: t.TempDir()}
	p := scionbin.StartPrebuild(context.Background(), r, spec, "amd64", "m")
	<-r.started
	if _, _, ok := p.Result(context.Background(), spec, "arm64", "m"); ok {
		t.Fatal("an amd64 build was taken for an arm64 guest")
	}
}
