package jail

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestContainerNetworkMode(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("podman inspect --type container --format {{.HostConfig.NetworkMode}} lever--a", proc.Result{Stdout: "slirp4netns\n"})
	mode, err := ContainerNetworkMode(context.Background(), f, "lever--a")
	if err != nil || mode != "slirp4netns" {
		t.Fatalf("mode %q err %v; want slirp4netns", mode, err)
	}
	// A runner that answers anything: the refusal must come from the guard.
	anyRun := proc.NewFakeRunner()
	anyRun.Script("podman", proc.Result{Stdout: "pasta\n"})
	for _, ref := range []string{"-rm", "--format=x", " "} {
		if _, err := ContainerNetworkMode(context.Background(), anyRun, ref); err == nil {
			t.Fatalf("reference %q must be refused", ref)
		}
	}
	if len(anyRun.Calls) != 0 {
		t.Fatalf("a refused reference must not reach podman: %v", anyRun.Calls)
	}
}

func TestContainerNetworkModeNoContainer(t *testing.T) {
	// podman 4.9 and 5 word it differently, and in either case.
	for _, stderr := range []string{"Error: no such container lever--gone", "Error: No such object: lever--gone"} {
		if _, err := ContainerNetworkMode(context.Background(), failRunner{stderr}, "lever--gone"); !errors.Is(err, ErrNoContainer) {
			t.Fatalf("stderr %q: want ErrNoContainer, got %v", stderr, err)
		}
	}
	if _, err := ContainerNetworkMode(context.Background(), failRunner{"Error: cannot connect to podman"}, "lever--a"); err == nil || errors.Is(err, ErrNoContainer) {
		t.Fatalf("an inspect failure is not a missing container: %v", err)
	}
}

type failRunner struct{ stderr string }

func (f failRunner) Run(context.Context, map[string]string, string, ...string) (proc.Result, error) {
	return proc.Result{Code: 125, Stderr: f.stderr}, errors.New("exit status 125")
}

func (f failRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return f.Run(ctx, env, name, args...)
}

func (f failRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return f.Run(ctx, env, name, args...)
}
