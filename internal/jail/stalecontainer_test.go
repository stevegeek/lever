package jail

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

const testID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const stateFormat = "podman inspect --type container --format {{.Id}} {{.Name}} {{.State.Status}} {{.State.Running}} {{.State.Paused}} "

func TestInspectContainerState(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script(stateFormat+"proj--m", proc.Result{Stdout: testID + " proj--m Created false false\n"})
	st, err := InspectContainerState(context.Background(), f, "proj--m")
	if err != nil || st.ID != testID || st.Status != "created" || st.Running || st.Paused || !st.Removable() {
		t.Fatalf("%+v %v", st, err)
	}
	for _, out := range []string{
		testID + " other false false false",                 // another container's name
		"abc proj--m created false false",                   // not a full id
		testID + " proj--m created maybe false",             // not a bool
		testID + " proj--m created false false extra words", // more fields
	} {
		f := proc.NewFakeRunner()
		f.Script(stateFormat+"proj--m", proc.Result{Stdout: out})
		if _, err := InspectContainerState(context.Background(), f, "proj--m"); err == nil {
			t.Errorf("answer %q must be refused", out)
		}
	}
	if _, err := InspectContainerState(context.Background(), failRunner{"Error: no such container proj--m"}, "proj--m"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("want ErrNoContainer, got %v", err)
	}
	anyRun := proc.NewFakeRunner()
	if _, err := InspectContainerState(context.Background(), anyRun, "--all"); err == nil || len(anyRun.Calls) != 0 {
		t.Fatal("a flag-like reference must not reach podman")
	}
}

func TestContainerStateRemovable(t *testing.T) {
	for _, s := range []string{"created", "configured", "initialized", "exited", "stopped"} {
		if !(ContainerState{Status: s}).Removable() {
			t.Errorf("%s must be removable", s)
		}
		if (ContainerState{Status: s, Running: true}).Removable() || (ContainerState{Status: s, Paused: true}).Removable() {
			t.Errorf("%s with a process must not be removable", s)
		}
	}
	for _, s := range []string{"running", "paused", "stopping", "removing", "unknown", ""} {
		if (ContainerState{Status: s}).Removable() {
			t.Errorf("%s must not be removable", s)
		}
	}
}

func TestRemoveArgs(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("podman", proc.Result{})
	if err := RemoveStoppedContainer(context.Background(), f, testID); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[0].Argv(); got != "podman rm "+testID {
		t.Fatalf("rm argv %q (never --force)", got)
	}
	for _, id := range []string{"proj--m", "-f", testID[:12]} {
		if RemoveStoppedContainer(context.Background(), f, id) == nil {
			t.Errorf("id %q must be refused", id)
		}
	}
	if len(f.Calls) != 1 {
		t.Fatalf("refused calls reached podman: %v", f.Calls)
	}
	if err := RemoveStoppedContainer(context.Background(), failRunner{"Error: no such container"}, testID); err != nil {
		t.Fatalf("a container already gone: %v", err)
	}
	if err := RemoveStoppedContainer(context.Background(), failRunner{"Error: cannot remove container as it is running"}, testID); err == nil {
		t.Fatal("podman's refusal must come back")
	}
}

// bigErrRunner fails every call with stderr of the given size and records the
// output cap each call was given.
type bigErrRunner struct {
	stderr string
	limits []int
}

func (b *bigErrRunner) Run(ctx context.Context, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	n, _ := proc.OutputLimit(ctx)
	b.limits = append(b.limits, n)
	return proc.Result{Code: 125, Stderr: b.stderr}, errors.New("exit status 125")
}
func (b *bigErrRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return b.Run(ctx, env, name, args...)
}
func (b *bigErrRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return b.Run(ctx, env, name, args...)
}

func TestContainerStateErrorsAreBounded(t *testing.T) {
	b := &bigErrRunner{stderr: strings.Repeat("Error: \x1b[31mboom\x1b[0m "+strings.Repeat("x", 1000)+"\n", 200)}
	_, ierr := InspectContainerState(context.Background(), b, "proj--m")
	rerr := RemoveStoppedContainer(context.Background(), b, testID)
	for _, err := range []error{ierr, rerr} {
		if err == nil {
			t.Fatal("podman's failure must come back")
		}
		if len(err.Error()) > podmanErrTailMax+200 {
			t.Errorf("error is %d bytes, want it bounded", len(err.Error()))
		}
		if strings.ContainsRune(err.Error(), '\x1b') {
			t.Errorf("error keeps an escape sequence: %q", err.Error()[:80])
		}
	}
	if len(b.limits) != 2 || b.limits[0] != containerStateOutputLimit || b.limits[1] != containerStateOutputLimit {
		t.Fatalf("output caps %v, want %d on each call", b.limits, containerStateOutputLimit)
	}
}
