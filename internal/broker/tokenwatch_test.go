package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// fakeHubTokens is a HubTokenHealer: expired names the agents whose token
// reads expired; resets records every reset in order.
type fakeHubTokens struct {
	expired  map[string]bool
	readErr  error
	resetErr error
	reads    []string
	resets   []string
}

func (f *fakeHubTokens) TokenExpired(_ context.Context, agent string) (bool, error) {
	f.reads = append(f.reads, agent)
	return f.expired[agent], f.readErr
}

func (f *fakeHubTokens) ResetAuth(_ context.Context, agent string) error {
	f.resets = append(f.resets, agent)
	return f.resetErr
}

// tokenWatchBroker is a reenrol fixture (manager slug "appname", worker
// "scratch") with a hub-token healer and a captured audit log.
func tokenWatchBroker(t *testing.T, mode string, agents []scion.Agent, h *fakeHubTokens) (*Broker, *bytes.Buffer) {
	t.Helper()
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{testInstanceProject: agents}}
	b, _, _ := reenrolBroker(t, rt, mode)
	b.hubTokens = h
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	return b, &buf
}

var liveFleet = []scion.Agent{
	{Slug: "appname", Phase: "running", ContainerStatus: "Up 9 hours"},
	{Slug: "scratch", Phase: "running", ContainerStatus: "Up 2 hours"},
}

// An expired token of a running agent is reset, by its slug, and audited
// under its identity; a valid one is only read.
func TestHealHubTokensResetsExpired(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"appname": true}}
	b, log := tokenWatchBroker(t, "all", liveFleet, h)
	b.healHubTokens(context.Background())
	if strings.Join(h.reads, ",") != "appname,scratch" {
		t.Fatalf("reads = %v, want the manager then the worker", h.reads)
	}
	if strings.Join(h.resets, ",") != "appname" {
		t.Fatalf("resets = %v, want [appname]", h.resets)
	}
	if !strings.Contains(log.String(), "op=hub-token caller=test-manager decision=allow") {
		t.Fatalf("audit = %q", log.String())
	}
}

// Only a running record over a live container is read at all.
func TestHealHubTokensSkipsAgentsNotRunning(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"appname": true, "scratch": true}}
	b, _ := tokenWatchBroker(t, "all", []scion.Agent{
		{Slug: "appname", Phase: "suspended", ContainerStatus: "stopped"},
		{Slug: "scratch", Phase: "running", ContainerStatus: "Exited (1) 3 minutes ago"},
	}, h)
	b.healHubTokens(context.Background())
	if len(h.reads)+len(h.resets) != 0 {
		t.Fatalf("reads %v resets %v, want none", h.reads, h.resets)
	}
}

// The auto_reenrol mode governs the watch like the certificate healer, and a
// revoked identity is never healed.
func TestHealHubTokensModeAndRevocation(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"appname": true, "scratch": true}}
	b, _ := tokenWatchBroker(t, "manager", liveFleet, h)
	b.healHubTokens(context.Background())
	if strings.Join(h.resets, ",") != "appname" {
		t.Fatalf("mode=manager resets = %v, want [appname]", h.resets)
	}

	h = &fakeHubTokens{expired: map[string]bool{"appname": true, "scratch": true}}
	b, _ = tokenWatchBroker(t, "off", liveFleet, h)
	b.healHubTokens(context.Background())
	if len(h.reads) != 0 {
		t.Fatalf("mode=off read %v", h.reads)
	}

	h = &fakeHubTokens{expired: map[string]bool{"appname": true, "scratch": true}}
	b, _ = tokenWatchBroker(t, "all", liveFleet, h)
	b.Revoke("scratch")
	b.Revoke("test-manager")
	b.healHubTokens(context.Background())
	if len(h.resets) != 0 {
		t.Fatalf("revoked identities reset: %v", h.resets)
	}
}

// One reset per agent per cooldown, whatever the token file keeps claiming;
// after the cooldown the next pass may try again.
func TestHealHubTokensCooldown(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"scratch": true}, resetErr: errors.New("scion reset-auth scratch: hub said no")}
	b, log := tokenWatchBroker(t, "all", liveFleet, h)
	now := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)
	b.reenrolNow = func() time.Time { return now }
	b.healHubTokens(context.Background())
	b.healHubTokens(context.Background())
	if len(h.resets) != 1 {
		t.Fatalf("resets inside the cooldown = %v, want one", h.resets)
	}
	if !strings.Contains(log.String(), "decision=error") || !strings.Contains(log.String(), "reset-auth failed") {
		t.Fatalf("a failed reset must be audited: %q", log.String())
	}
	now = now.Add(tokenHealCooldown)
	b.healHubTokens(context.Background())
	if len(h.resets) != 2 {
		t.Fatalf("resets after the cooldown = %v, want two", h.resets)
	}
}

// A token that cannot be read, or a listing that fails, resets nothing.
func TestHealHubTokensReadAndListFailures(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"appname": true}, readErr: errors.New("exit status 1")}
	b, _ := tokenWatchBroker(t, "all", liveFleet, h)
	b.healHubTokens(context.Background())
	if len(h.resets) != 0 {
		t.Fatalf("an unreadable token was reset: %v", h.resets)
	}
	rt := &fakeRuntime{listErr: errors.New("hub down")}
	b2, _, _ := reenrolBroker(t, rt, "all")
	h2 := &fakeHubTokens{expired: map[string]bool{"appname": true}}
	b2.hubTokens = h2
	b2.healHubTokens(context.Background())
	if len(h2.reads) != 0 {
		t.Fatalf("a failed listing still read tokens: %v", h2.reads)
	}
}

// A record the pre-role guard refuses is never reset: the hub would mint the
// new token from its empty stored role, which a later scion reads as full.
func TestHealHubTokensRefusesAPreRoleRecord(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"appname": true, "scratch": true}}
	b, log := tokenWatchBroker(t, "all", liveFleet, h)
	b.verifyRole = func(_ context.Context, agent string) error {
		if agent == "scratch" {
			return errors.New("record stores no role \x1b[31m")
		}
		return nil
	}
	b.healHubTokens(context.Background())
	if strings.Join(h.resets, ",") != "appname" {
		t.Fatalf("resets = %v, want only the manager", h.resets)
	}
	if !strings.Contains(log.String(), "decision=deny") || strings.Contains(log.String(), "\x1b") {
		t.Fatalf("audit = %q", log.String())
	}
}

// A worker whose lifecycle lock is held is skipped without spending its
// cooldown; the next pass resets it once the lock is free.
func TestHealHubTokensSkipsABusyWorker(t *testing.T) {
	h := &fakeHubTokens{expired: map[string]bool{"scratch": true}}
	b, _ := tokenWatchBroker(t, "all", liveFleet, h)
	b.reenrolLockWait = 10 * time.Millisecond
	unlock, err := b.lockWorker(context.Background(), "scratch")
	if err != nil {
		t.Fatal(err)
	}
	b.healHubTokens(context.Background())
	if len(h.resets) != 0 {
		t.Fatalf("a busy worker was reset: %v", h.resets)
	}
	unlock()
	b.healHubTokens(context.Background())
	if strings.Join(h.resets, ",") != "scratch" {
		t.Fatalf("resets after the lock freed = %v, want [scratch]", h.resets)
	}
}
