package broker

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	rl.AllowObtain("analyst", "db", "read")
	rl.AllowDelegate("manager", "db", "read", "worker")
	reg := registry.New()
	_ = reg.Register(registry.Tool{
		Name: "db", Backend: "http://127.0.0.1:3201",
		Operations: map[string]registry.Operation{"read": {Name: "read"}},
	})
	return Config{
		Keys: kp, CA: c, Tickets: ca.NewTicketStore(), Rules: rl, Registry: reg,
		ManagerIdentity: "manager", Agents: []string{"manager", "analyst", "worker"},
		GrantTTL: time.Hour, TicketTTL: 10 * time.Minute, ServerName: "host.orb.internal",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestEpochBumpAndRevoke(t *testing.T) {
	b := New(testConfig(t))
	if b.MinEpoch() != 0 {
		t.Fatalf("initial epoch = %d", b.MinEpoch())
	}
	b.BumpEpoch()
	if b.MinEpoch() != 1 {
		t.Fatalf("after bump = %d", b.MinEpoch())
	}
	if b.isRevoked("analyst") {
		t.Fatal("analyst not revoked initially")
	}
	b.Revoke("analyst")
	if !b.isRevoked("analyst") {
		t.Fatal("analyst should be revoked")
	}
}

func TestNewDefaultsTTLs(t *testing.T) {
	c := testConfig(t)
	c.GrantTTL = 0
	c.TicketTTL = 0
	b := New(c)
	if b.grantTTL <= 0 || b.ticketTTL <= 0 {
		t.Fatalf("TTLs not defaulted: grant=%v ticket=%v", b.grantTTL, b.ticketTTL)
	}
	// G2 coupling: the in-container lever-renew sidecar refreshes the LLM
	// capability token every 12h (cmd/lever-agent renew --loop default interval).
	// A long-running claude reads ANTHROPIC_AUTH_TOKEN once at startup and holds
	// it for the session, so the default grant TTL must outlive a renew cycle (and
	// a session) or the token strands mid-session. The live epoch/revoke gate is
	// the security cut, not this TTL — so a session-scale default is safe.
	if b.grantTTL < 12*time.Hour {
		t.Fatalf("default grantTTL = %v, must be >= the 12h renew interval to avoid mid-session token expiry", b.grantTTL)
	}
}
