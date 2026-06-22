package broker

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

func newTestBroker(t *testing.T) *Broker {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return New(kp, c, samplePolicy(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEpochBumpRaisesMinEpoch(t *testing.T) {
	b := newTestBroker(t)
	if b.MinEpoch() != 0 {
		t.Fatalf("initial min epoch = %d, want 0", b.MinEpoch())
	}
	b.BumpEpoch()
	if b.MinEpoch() != 1 {
		t.Fatalf("after bump min epoch = %d, want 1", b.MinEpoch())
	}
}

func TestRevokeMarksAgent(t *testing.T) {
	b := newTestBroker(t)
	if b.isRevoked("scratch") {
		t.Fatal("scratch should not be revoked initially")
	}
	b.Revoke("scratch")
	if !b.isRevoked("scratch") {
		t.Fatal("scratch should be revoked after Revoke")
	}
}

func TestGrantTTLDefault(t *testing.T) {
	b := newTestBroker(t)
	if b.policy.GrantTTL <= 0 {
		t.Fatalf("GrantTTL not defaulted: %v", b.policy.GrantTTL)
	}
	_ = time.Hour
}
