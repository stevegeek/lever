package jail

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// The guest deadline is shorter than the host's by the SIGKILL grace and
// a second for the transport, so the guest ends a hung command first.
func TestGuestBounded(t *testing.T) {
	name, args := guestBounded(context.Background(), "podman", "exec", "c", "tmux")
	if name != "timeout" || !reflect.DeepEqual(args, []string{"--kill-after=1s", "8s", "podman", "exec", "c", "tmux"}) {
		t.Fatalf("argv = %s %q", name, args)
	}
	if kill := guestTimeoutSecs(context.Background()) + int(guestKillAfter/time.Second); time.Duration(kill)*time.Second >= agentExecTimeout {
		t.Fatalf("guest deadline %ds + kill grace must end before the host's %s", guestTimeoutSecs(context.Background()), agentExecTimeout)
	}
	// Several commands under one host deadline: each gets what is left.
	ctx, cancel := context.WithTimeout(context.Background(), 5500*time.Millisecond)
	defer cancel()
	if got := guestTimeoutSecs(ctx); got != 4 {
		t.Fatalf("5.5 s left: guest deadline %ds, want 4", got)
	}
	old := agentExecTimeout
	agentExecTimeout = 50 * time.Millisecond
	t.Cleanup(func() { agentExecTimeout = old })
	if guestTimeoutSecs(context.Background()) != 1 {
		t.Fatalf("a short host deadline still gives the guest 1 s, got %d", guestTimeoutSecs(context.Background()))
	}
}

// timeout(1)'s exit statuses for a command it ended are a timed-out probe;
// any other result passes through unchanged.
func TestRunGuestBoundedTimeout(t *testing.T) {
	for _, code := range []int{guestTimedOut, guestKilled} {
		var calls []string
		r := codeRunner{res: proc.Result{Code: code}, calls: &calls}
		if _, err := runGuestBounded(context.Background(), r, "podman", "exec", "c", "true"); !errors.Is(err, ErrProbeTimedOut) {
			t.Fatalf("code %d: err = %v, want ErrProbeTimedOut", code, err)
		}
		if ProbeErrorClass(func() error { _, err := (AgentProbe{R: r}).HubToken(context.Background(), "c"); return err }()) != "timed out" {
			t.Fatalf("code %d: a guest timeout must read as timed out", code)
		}
	}
	var calls []string
	r := codeRunner{res: proc.Result{Code: 2}, calls: &calls}
	if _, err := runGuestBounded(context.Background(), r, "podman", "exec", "c", "true"); err == nil || errors.Is(err, ErrProbeTimedOut) {
		t.Fatalf("exit 2: err = %v", err)
	}
	if calls[0] != "timeout --kill-after=1s 8s podman exec c true" {
		t.Fatalf("argv %q", calls[0])
	}
	if _, err := (AgentProbe{R: codeRunner{res: proc.Result{Code: guestTimedOut}, calls: &calls}}).HarnessAlive(context.Background(), "c"); !errors.Is(err, ErrProbeTimedOut) {
		t.Fatalf("harness probe: err = %v, want ErrProbeTimedOut", err)
	}
}

// Every exec into an agent container runs under the guest's timeout, so a
// probe the agent made hang does not outlive its deadline in the guest.
func TestAgentExecsRunUnderGuestTimeout(t *testing.T) {
	var calls []string
	r := codeRunner{res: proc.Result{Code: 1}, calls: &calls}
	p := AgentProbe{R: r}
	ctx := context.Background()
	_, _ = p.HubToken(ctx, "c")
	_, _ = p.HarnessAlive(ctx, "c")
	_ = p.ReportSessionRunning(ctx, "c")
	_, _ = p.ClaudeSettings(ctx, "c")
	_, _ = p.BackgroundSessions(ctx, "c")
	_, _ = p.LogTail(ctx, "c")
	_, _ = ContainerHasCharDevice(ctx, r, "c", "/dev/kvm")
	if len(calls) != 7 {
		t.Fatalf("calls = %q", calls)
	}
	for _, c := range calls {
		if len(c) < len("timeout --kill-after=1s 8s podman ") || c[:len("timeout --kill-after=1s 8s podman ")] != "timeout --kill-after=1s 8s podman " {
			t.Fatalf("not under the guest timeout: %q", c)
		}
	}
}
