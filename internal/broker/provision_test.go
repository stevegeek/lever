package broker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

// leafFor builds a verified-chain TLS state whose client cert CN is `cn`.
func leafFor(t *testing.T, b *Broker, cn string) *tls.ConnectionState {
	t.Helper()
	csr := makeCSRForCN(t, cn)
	certPEM, err := b.ca.SignCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}
}

func TestProvisionIssuesTicketForManager(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ProvisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ticket == "" {
		t.Fatal("empty ticket")
	}
}

func TestProvisionRejectsNonManager(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "analyst") // not the manager
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestProvisionRejectsUnknownWorker(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(ProvisionRequest{Worker: "ghost"})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for worker not in config", w.Code)
	}
}

func TestProvisionRejectsNoCert(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body)) // no r.TLS
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no client cert)", w.Code)
	}
}

func TestProvisionDeniesRevokedManager(t *testing.T) {
	b := New(testConfig(t))
	b.Revoke("manager")
	body, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked manager provision: status = %d, want 403", w.Code)
	}
}
