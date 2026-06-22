package broker

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

func provisionReq(t *testing.T, b *Broker, callerCN, grove string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(ProvisionRequest{Grove: grove})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	certPEM, keyPEM, err := b.ca.IssueAgentCert(callerCN)
	if err != nil {
		t.Fatal(err)
	}
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509Cert{{mustLeaf(t, certPEM, keyPEM)}}}
	return r
}

func TestProvisionMintsUsableBiscuit(t *testing.T) {
	b := newTestBroker(t)
	w := httptest.NewRecorder()
	b.handleProvision(w, provisionReq(t, b, "manager", "scratch"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ProvisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Cert == "" || resp.Key == "" || resp.Biscuit == "" {
		t.Fatal("empty provision fields")
	}
	// The minted biscuit must authorize scratch for a granted op.
	raw, err := base64.RawURLEncoding.DecodeString(resp.Biscuit)
	if err != nil {
		t.Fatal(err)
	}
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "scratch", Operation: "qmd", Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("minted biscuit failed to verify: %v", err)
	}
}

func TestProvisionRejectsNonManager(t *testing.T) {
	b := newTestBroker(t)
	w := httptest.NewRecorder()
	b.handleProvision(w, provisionReq(t, b, "scratch", "scratch")) // caller is a grove, not the manager
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestProvisionRejectsUnknownGrove(t *testing.T) {
	b := newTestBroker(t)
	w := httptest.NewRecorder()
	b.handleProvision(w, provisionReq(t, b, "manager", "ghost"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for grove not in policy", w.Code)
	}
}
