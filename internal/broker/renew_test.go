package broker

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRenewUsesAuthenticatedCNNotCSRCN(t *testing.T) {
	b := New(testConfig(t))
	// Caller is authenticated as "worker" but submits a CSR claiming "manager".
	csr := makeCSRForCN(t, "manager")
	body, _ := json.Marshal(RenewRequest{CSR: string(csr)})
	r := httptest.NewRequest("POST", "/renew", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	b.handleRenew(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp RenewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	blk, _ := pem.Decode([]byte(resp.Cert))
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "worker" {
		t.Fatalf("renewed CN = %q, want worker (authenticated CN, NOT the CSR's manager)", leaf.Subject.CommonName)
	}
}

func TestRenewRejectsNoCert(t *testing.T) {
	b := New(testConfig(t))
	csr := makeCSRForCN(t, "worker")
	body, _ := json.Marshal(RenewRequest{CSR: string(csr)})
	r := httptest.NewRequest("POST", "/renew", bytes.NewReader(body)) // no client cert
	w := httptest.NewRecorder()
	b.handleRenew(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
