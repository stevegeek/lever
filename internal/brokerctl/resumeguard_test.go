package brokerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// The broker's re-enrol bounce of the manager runs apply's resume checks: a
// mount whose directory is gone refuses before; a tree mount from elsewhere
// after the resume suspends the manager.
func TestManagerResumeGuard(t *testing.T) {
	mounts := []jail.Mount{
		{Source: "/lever", Destination: "/workspace", RW: true},
		{Source: "/lever/kb", Destination: "/workspace/kb"},
	}
	newDeps := func(problems []jail.LiveMountProblem) (apply.Deps, *proc.FakeRunner) {
		f := proc.NewFakeRunner()
		f.Script("scion", proc.Result{})
		return apply.Deps{
			Scion:                  scion.New(f, scion.Options{}),
			InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return mounts, nil },
			CheckLiveMounts: func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				return problems, nil
			},
		}, f
	}
	suspended := func(f *proc.FakeRunner) bool {
		for _, c := range f.Calls {
			if len(c.Args) > 0 && c.Args[0] == "suspend" {
				return true
			}
		}
		return false
	}

	// The directory a record mount names is gone: refused before.
	deps, _ := newDeps(nil)
	app := &config.App{Name: "assistant", Tree: t.TempDir()}
	if _, err := managerResumeGuard(app, deps, "/lever")(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be resumed") {
		t.Fatalf("err = %v, want the gone directory refused before the bounce", err)
	}

	if err := os.Mkdir(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Held: both halves pass, nothing suspended.
	deps, f := newDeps(nil)
	after, err := managerResumeGuard(app, deps, "/lever")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := after.After(context.Background()); err != nil || suspended(f) || !after.Verified() {
		t.Fatalf("after = %v, suspended %v, verified %v", err, suspended(f), after.Verified())
	}
	// From elsewhere after the resume: refused, the manager suspended.
	deps, f = newDeps([]jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}})
	after, err = managerResumeGuard(app, deps, "/lever")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := after.After(context.Background()); err == nil || !strings.Contains(err.Error(), "is mounted from another place") || !suspended(f) {
		t.Fatalf("after = %v, suspended %v", err, suspended(f))
	}
	// A failed resume whose container runs all the same is checked too.
	deps, f = newDeps([]jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}})
	after, err = managerResumeGuard(app, deps, "/lever")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := after.AfterFailure(context.Background(), errors.New("hook failed")); err == nil || !strings.Contains(err.Error(), "hook failed") || !strings.Contains(err.Error(), "another place") || !suspended(f) {
		t.Fatalf("after failure = %v, suspended %v", err, suspended(f))
	}
	// Unwired readers: refused before anything is resumed.
	if _, err := managerResumeGuard(app, apply.Deps{}, "/lever")(context.Background()); err == nil {
		t.Fatal("a guard without the mount readers must refuse")
	}
}
