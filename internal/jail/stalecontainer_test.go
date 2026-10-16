package jail

import (
	"context"
	"errors"
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
