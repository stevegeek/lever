package brokerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// The broker's bounce of the manager runs the manager's lifecycle: a check
// that refuses (here a mount whose directory is gone) reads as the broker's
// refusal, before any ticket is staged; an unknown change is refused.
func TestManagerLifecycleMapsRefusals(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list", proc.Result{Stdout: `[{"slug":"assistant","phase":"suspended","containerStatus":"stopped","image":"lever.local/lever-claude:1"}]`})
	app := &config.App{Name: "assistant", Tree: t.TempDir()}
	staged := false
	d := apply.Deps{
		Scion: scion.New(f, scion.Options{}), Log: func(string, ...any) {}, JailMount: "/lever",
		ManagerLock:     apply.HeldManagerLock,
		VerifyAgentRole: func(context.Context, string, string) error { return nil },
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) {
			return []jail.Mount{{Source: "/lever/kb", Destination: "/workspace/kb"}}, nil
		},
		CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil },
	}
	ensure := managerLifecycle(app, d)
	_, err := ensure(context.Background(), broker.ManagerChange{Kind: broker.ManagerReenrol,
		StageTicket: func(context.Context) error { staged = true; return nil }})
	if !errors.Is(err, broker.ErrManagerRefused) || !strings.Contains(err.Error(), "cannot be resumed") || staged {
		t.Fatalf("err = %v staged %v, want the gone directory refused before the stage", err, staged)
	}
	if _, err := ensure(context.Background(), broker.ManagerChange{Kind: 99}); err == nil {
		t.Fatal("an unknown change was accepted")
	}
}

// The broker takes the lifecycle lock without waiting: a held lock reads as
// busy and names the holder; with the state directory inside the tree there
// is no lock file.
func TestManagerLockIsTheLifecycleLock(t *testing.T) {
	root := t.TempDir()
	app := &config.App{Name: "assistant", Tree: filepath.Join(root, "tree")}
	st := state.State{Dir: filepath.Join(root, "state")}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := managerLock(app, st)
	release, err := lock("the broker's test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock("a second taker"); !errors.Is(err, broker.ErrManagerBusy) {
		t.Fatalf("held: %v", err)
	}
	release()
	again, err := lock("after release")
	if err != nil {
		t.Fatal(err)
	}
	again()
	inTree := &config.App{Name: "assistant", Tree: root}
	free := managerLock(inTree, st)
	r1, err1 := free("x")
	r2, err2 := free("y")
	if err1 != nil || err2 != nil {
		t.Fatalf("in the tree: %v %v", err1, err2)
	}
	r1()
	r2()
}
