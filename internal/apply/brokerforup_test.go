package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/agent"
)

// brokerForUpDeps records the broker controller calls in order.
func brokerForUpDeps(calls *[]string, mint func() (BootstrapMaterial, error)) Deps {
	return Deps{
		StartBroker:    func(context.Context) error { *calls = append(*calls, "start"); return nil },
		BrokerHealthy:  func(context.Context) error { *calls = append(*calls, "healthy"); return nil },
		RearmBootstrap: func(context.Context) error { *calls = append(*calls, "rearm"); return nil },
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			*calls = append(*calls, "mint")
			return mint()
		},
	}
}

// TestEnsureBrokerForUpStagesAnOpenLatch: a broker the call started has an
// open latch, which is minted and staged at once, as apply's mint step does.
func TestEnsureBrokerForUpStagesAnOpenLatch(t *testing.T) {
	tree := t.TempDir()
	var calls []string
	d := brokerForUpDeps(&calls, func() (BootstrapMaterial, error) {
		return BootstrapMaterial{Ticket: "tkt-up", AgentCN: "mgr"}, nil
	})
	staged, err := EnsureBrokerForUp(context.Background(), d, tree)
	if err != nil || !staged {
		t.Fatalf("staged %v err %v", staged, err)
	}
	if got := strings.Join(calls, ","); got != "start,healthy,mint" {
		t.Fatalf("calls = %s", got)
	}
	b, err := agent.LoadBootstrap(filepath.Join(tree, ".lever", "bootstrap.json"))
	if err != nil || b.Ticket != "tkt-up" {
		t.Fatalf("staged %+v, %v", b, err)
	}
}

// TestEnsureBrokerForUpReusedBroker: a reused broker's spent latch is not an
// error, stages nothing and is not re-armed here: EnsureManager re-arms it
// only before a resume or create.
func TestEnsureBrokerForUpReusedBroker(t *testing.T) {
	tree := t.TempDir()
	var calls []string
	d := brokerForUpDeps(&calls, func() (BootstrapMaterial, error) { return BootstrapMaterial{}, ErrBootstrapLatched })
	if staged, err := EnsureBrokerForUp(context.Background(), d, tree); err != nil || staged {
		t.Fatalf("staged %v err %v", staged, err)
	}
	if got := strings.Join(calls, ","); got != "start,healthy,mint" {
		t.Fatalf("calls = %s", got)
	}
	if _, err := os.Stat(filepath.Join(tree, ".lever", "bootstrap.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a ticket was staged: %v", err)
	}
}

// TestEnsureBrokerForUpFailures: a broker that does not start or is not
// healthy fails the up before any mint; an unwired controller is refused.
func TestEnsureBrokerForUpFailures(t *testing.T) {
	boom := errors.New("bind: address already in use")
	var calls []string
	d := brokerForUpDeps(&calls, func() (BootstrapMaterial, error) { return BootstrapMaterial{}, nil })
	d.BrokerHealthy = func(context.Context) error { calls = append(calls, "healthy"); return boom }
	if _, err := EnsureBrokerForUp(context.Background(), d, t.TempDir()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(calls, ","); got != "start,healthy" {
		t.Fatalf("calls = %s", got)
	}
	calls = nil
	d = brokerForUpDeps(&calls, func() (BootstrapMaterial, error) { return BootstrapMaterial{}, boom })
	if _, err := EnsureBrokerForUp(context.Background(), d, t.TempDir()); !errors.Is(err, boom) {
		t.Fatalf("mint err = %v", err)
	}
	if _, err := EnsureBrokerForUp(context.Background(), Deps{}, t.TempDir()); err == nil {
		t.Fatal("an unwired broker controller was accepted")
	}
}
