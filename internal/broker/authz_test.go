package broker

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

// requestWith builds a request carrying a biscuit bearer and a fake verified
// client cert chain for caller CN.
func requestWith(t *testing.T, b *Broker, caller string, biscuit []byte) *http.Request {
	t.Helper()
	r, _ := http.NewRequest("POST", "/mcp/qmd/x", nil)
	if biscuit != nil {
		r.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(biscuit))
	}
	// Provide a verified-chain TLS state with the caller as the leaf CN.
	certPEM, keyPEM, err := b.ca.IssueAgentCert(caller)
	if err != nil {
		t.Fatal(err)
	}
	leaf := mustLeaf(t, certPEM, keyPEM)
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509Cert{{leaf}}}
	return r
}

func TestAuthorizeAllowsValid(t *testing.T) {
	b := newTestBroker(t)
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent: "scratch", Tools: []string{"qmd"}, Expiry: time.Now().Add(time.Hour), Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := requestWith(t, b, "scratch", tok)
	caller, err := b.authorize(r, "qmd")
	if err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if caller != "scratch" {
		t.Errorf("caller = %q", caller)
	}
}

func TestAuthorizeDeniesRevoked(t *testing.T) {
	b := newTestBroker(t)
	b.Revoke("scratch")
	tok, _ := token.Mint(b.keys.Private, token.Grant{Agent: "scratch", Tools: []string{"qmd"}, Expiry: time.Now().Add(time.Hour)})
	r := requestWith(t, b, "scratch", tok)
	if _, err := b.authorize(r, "qmd"); err == nil {
		t.Fatal("expected denial for revoked agent")
	}
}

func TestAuthorizeDeniesMissingBiscuit(t *testing.T) {
	b := newTestBroker(t)
	r := requestWith(t, b, "scratch", nil)
	if _, err := b.authorize(r, "qmd"); err == nil {
		t.Fatal("expected denial for missing biscuit")
	}
}
