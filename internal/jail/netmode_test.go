package jail

import (
	"context"
	"errors"
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
	if _, err := ContainerNetworkMode(context.Background(), f, "-rm"); err == nil {
		t.Fatal("a reference that looks like a flag must be refused")
	}
}

func TestContainerNetworkModeNoContainer(t *testing.T) {
	r := noContainerRunner{}
	if _, err := ContainerNetworkMode(context.Background(), r, "lever--gone"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("want ErrNoContainer, got %v", err)
	}
}

type noContainerRunner struct{ proc.Runner }

func (noContainerRunner) Run(context.Context, map[string]string, string, ...string) (proc.Result, error) {
	return proc.Result{Code: 125, Stderr: "Error: no such container lever--gone"}, errors.New("exit status 125")
}
