package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

// cappedFake answers like proc.FakeRunner but honours the context's output
// cap the way proc.RealRunner does, and records each cap it saw.
type cappedFake struct {
	*proc.FakeRunner
	caps []int
}

func newCappedFake() *cappedFake { return &cappedFake{FakeRunner: proc.NewFakeRunner()} }

func (f *cappedFake) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	n, _ := proc.OutputLimit(ctx)
	f.caps = append(f.caps, n)
	res, err := f.FakeRunner.Run(ctx, env, name, args...)
	if n > 0 && (len(res.Stdout) > n || len(res.Stderr) > n) {
		return proc.Result{Stdout: res.Stdout[:min(n, len(res.Stdout))], Stderr: res.Stderr[:min(n, len(res.Stderr))], Code: -1},
			fmt.Errorf("%s: %w", name, proc.ErrOutputLimit)
	}
	return res, err
}

func (f *cappedFake) onlySmallCaps(t *testing.T) {
	t.Helper()
	for i, n := range f.caps {
		if n != smallGuestOutputLimit {
			t.Fatalf("call %d (%s) ran with cap %d, want %d", i, f.Calls[i].Argv(), n, smallGuestOutputLimit)
		}
	}
}

// A guest clock read past its cap signs on the host clock, even when the
// flood would still parse as an epoch.
func TestReadGuestClockIsCapped(t *testing.T) {
	logf := func(logged *[]string) func(string, ...any) {
		return func(format string, args ...any) { *logged = append(*logged, fmt.Sprintf(format, args...)) }
	}
	f := newCappedFake()
	f.Script("date +%s", proc.Result{Stdout: "1\n" + strings.Repeat(" ", smallGuestOutputLimit)})
	var logged []string
	c := readGuestClock(context.Background(), f, logf(&logged))
	if c.Offset != 0 || len(logged) != 1 || !strings.Contains(logged[0], proc.ErrOutputLimit.Error()) {
		t.Fatalf("flood: clock %+v, log %q", c, logged)
	}
	f.onlySmallCaps(t)

	f = newCappedFake()
	f.Script("date +%s", proc.Result{Stdout: fmt.Sprintf("%d\n", time.Now().Add(time.Hour).Unix())})
	logged = nil
	c = readGuestClock(context.Background(), f, logf(&logged))
	if c.Offset < 59*time.Minute || len(logged) != 0 {
		t.Fatalf("normal: clock %+v, log %q", c, logged)
	}
	f.onlySmallCaps(t)
}

// A project marker past its cap is a read failure, not a marker.
func TestReadProjectMarkerIsCapped(t *testing.T) {
	f := newCappedFake()
	f.Script("sh -c", proc.Result{Stdout: "project-id: p1\nproject-slug: s1\n" + strings.Repeat("#", smallGuestOutputLimit)})
	if _, _, err := readProjectMarker(context.Background(), f, "/w/tree"); !errors.Is(err, proc.ErrOutputLimit) {
		t.Fatalf("flood: %v", err)
	}
	f.onlySmallCaps(t)

	f = newCappedFake()
	f.Script("sh -c", proc.Result{Stdout: "project-id: p1\nproject-slug: s1\n"})
	if id, slug, err := readProjectMarker(context.Background(), f, "/w/tree"); err != nil || id != "p1" || slug != "s1" {
		t.Fatalf("normal: %q %q %v", id, slug, err)
	}
	f.onlySmallCaps(t)
}

// The marker read against a guest that really floods is killed at the cap.
func TestReadProjectMarkerStopsARealFlood(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("no yes(1)")
	}
	start := time.Now()
	if _, _, err := readProjectMarker(context.Background(), floodRunner{}, "/w/tree"); !errors.Is(err, proc.ErrOutputLimit) {
		t.Fatalf("readProjectMarker = %v, want the output limit", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("readProjectMarker took %v", d)
	}
}

// An image policy probe past its cap fails the check, and what it quotes of
// the probe's stderr is bounded and terminal-safe.
func TestAgentImagesEnforcePolicyIsCapped(t *testing.T) {
	app, _, _ := sshTestApp(t)
	app.Workers = []config.Worker{{Name: "w1", Image: "lever.local/w:1"}, {Name: "w2", Image: "lever.local/w:2"}}
	f := newCappedFake()
	f.Script("sh -c", proc.Result{Stderr: "\x1b]0;owned\a" + strings.Repeat("e", 2*smallGuestOutputLimit), Code: 5})
	err := agentImagesEnforcePolicy(context.Background(), f, app)
	if err == nil || !strings.Contains(err.Error(), "could not check") || strings.Contains(err.Error(), "\x1b") || len(err.Error()) > 2000 {
		t.Fatalf("flood: %d bytes: %.300q", len(fmt.Sprint(err)), fmt.Sprint(err))
	}
	f.onlySmallCaps(t)

	f = newCappedFake()
	f.Script("sh -c", proc.Result{})
	if err := agentImagesEnforcePolicy(context.Background(), f, app); err != nil {
		t.Fatalf("normal: %v", err)
	}
	if len(f.caps) != len(agentImages(app)) || len(f.caps) < 2 {
		t.Fatalf("%d probes, want one per agent image", len(f.caps))
	}
	f.onlySmallCaps(t)
}
