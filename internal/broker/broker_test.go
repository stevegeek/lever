package broker

import (
	"testing"
	"time"
)

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
	c.Identity.GrantTTL = 0
	c.Identity.TicketTTL = 0
	b := New(c)
	if b.grantTTL <= 0 || b.ticketTTL <= 0 {
		t.Fatalf("TTLs not defaulted: grant=%v ticket=%v", b.grantTTL, b.ticketTTL)
	}
	// Coupling: the in-container lever-renew sidecar refreshes the LLM
	// capability token every 12h (cmd/lever-agent renew --loop default interval).
	// A long-running claude reads ANTHROPIC_AUTH_TOKEN once at startup and holds
	// it for the session, so the default grant TTL must outlive a renew cycle (and
	// a session) or the token strands mid-session. The live epoch/revoke gate is
	// the security cut, not this TTL — so a session-scale default is safe.
	if b.grantTTL < 12*time.Hour {
		t.Fatalf("default grantTTL = %v, must be >= the 12h renew interval to avoid mid-session token expiry", b.grantTTL)
	}
}
