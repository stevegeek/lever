package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// postProvision asks the broker for a ticket for worker as the given caller
// CN; an empty cn sends no client certificate at all.
func postProvision(t *testing.T, b *Broker, cn, worker string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(wire.ProvisionRequest{Worker: worker})
	r := httptest.NewRequest("POST", "/provision", bytes.NewReader(body))
	if cn != "" {
		r.TLS = leafFor(t, b, cn)
	}
	w := httptest.NewRecorder()
	b.handleProvision(w, r)
	return w
}

func TestProvisionIssuesTicketForManager(t *testing.T) {
	b := New(testConfig(t))
	w := postProvision(t, b, "manager", "worker")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp wire.ProvisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ticket == "" {
		t.Fatal("empty ticket")
	}
}

func TestProvisionRejectsNonManager(t *testing.T) {
	b := New(testConfig(t))
	w := postProvision(t, b, "analyst", "worker") // not the manager
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestProvisionRejectsUnknownWorker(t *testing.T) {
	b := New(testConfig(t))
	w := postProvision(t, b, "manager", "ghost")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for worker not in config", w.Code)
	}
}

func TestProvisionRejectsNoCert(t *testing.T) {
	b := New(testConfig(t))
	w := postProvision(t, b, "", "worker") // no client cert
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no client cert)", w.Code)
	}
}

func TestProvisionDeniesRevokedManager(t *testing.T) {
	b := New(testConfig(t))
	b.Revoke("manager")
	w := postProvision(t, b, "manager", "worker")
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked manager provision: status = %d, want 403", w.Code)
	}
}
