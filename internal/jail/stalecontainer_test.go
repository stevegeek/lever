package jail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestCommitAndRemoveArgs(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("podman", proc.Result{})
	if err := CommitContainer(context.Background(), f, testID, "lever.local/proj--m-backup:20261015T063000Z"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStoppedContainer(context.Background(), f, testID); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[0].Argv(); got != "podman commit --quiet "+testID+" lever.local/proj--m-backup:20261015T063000Z" {
		t.Fatalf("commit argv %q", got)
	}
	if got := f.Calls[1].Argv(); got != "podman rm "+testID {
		t.Fatalf("rm argv %q (never --force)", got)
	}
	for _, img := range []string{"docker.io/library/alpine:latest", "lever.local/x-backup", "lever.local/X-backup:1", "--rm:1"} {
		if err := CommitContainer(context.Background(), f, testID, img); err == nil {
			t.Errorf("image %q must be refused", img)
		}
	}
	for _, id := range []string{"proj--m", "-f", testID[:12]} {
		if CommitContainer(context.Background(), f, id, "lever.local/proj--m-backup:1") == nil || RemoveStoppedContainer(context.Background(), f, id) == nil {
			t.Errorf("id %q must be refused", id)
		}
	}
	if len(f.Calls) != 2 {
		t.Fatalf("refused calls reached podman: %v", f.Calls)
	}
	if err := RemoveStoppedContainer(context.Background(), failRunner{"Error: no such container"}, testID); err != nil {
		t.Fatalf("a container already gone: %v", err)
	}
	if err := RemoveStoppedContainer(context.Background(), failRunner{"Error: cannot remove container as it is running"}, testID); err == nil {
		t.Fatal("podman's refusal must come back")
	}
}

func TestBackupImageRepo(t *testing.T) {
	if r, err := BackupImageRepo("proj--m"); err != nil || r != "lever.local/proj--m-backup" {
		t.Fatalf("%q %v", r, err)
	}
	for _, n := range []string{"Proj--m", "a/b", "", "-x", "a..b", "a b"} {
		if _, err := BackupImageRepo(n); err == nil {
			t.Errorf("%q must be refused", n)
		}
	}
	if got := BackupTag(time.Date(2026, 10, 15, 6, 30, 1, 0, time.FixedZone("x", 3600))); got != "20261015T053001Z" {
		t.Fatal(got)
	}
}

func TestPruneBackupImagesKeepsNewest(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("podman images", proc.Result{Stdout: strings.Join([]string{
		"lever.local/proj--m-backup:20261013T000000Z",
		"lever.local/proj--m-backup:20261015T000000Z",
		"lever.local/proj--m-backup:manual",
		"lever.local/proj--m-backup:20261014T000000Z",
		"lever.local/proj--m-backup:20261012T000000Z",
	}, "\n")})
	f.Script("podman rmi", proc.Result{})
	removed, err := PruneBackupImages(context.Background(), f, "lever.local/proj--m-backup", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "lever.local/proj--m-backup:20261013T000000Z lever.local/proj--m-backup:20261012T000000Z"
	if strings.Join(removed, " ") != want {
		t.Fatalf("removed %v; want %s (a tag lever did not write is left alone)", removed, want)
	}
	if _, err := PruneBackupImages(context.Background(), f, "docker.io/library/alpine", 2); err == nil {
		t.Fatal("a repository lever does not own must be refused")
	}
}

func TestContainerWritableSize(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("podman container inspect --size --format {{json .SizeRw}} "+testID, proc.Result{Stdout: "18253611008\n"})
	if n, err := ContainerWritableSize(context.Background(), f, testID); err != nil || n != 18253611008 {
		t.Fatalf("%d %v", n, err)
	}
	g := proc.NewFakeRunner()
	g.Script("podman container inspect", proc.Result{Stdout: "null\n"})
	if _, err := ContainerWritableSize(context.Background(), g, testID); err == nil {
		t.Fatal("an unknown size must be an error")
	}
}
