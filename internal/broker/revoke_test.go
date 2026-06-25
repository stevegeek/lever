package broker

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewSeedsRevocationState(t *testing.T) {
	c := testConfig(t)
	c.RevocationState = RevocationState{MinEpoch: 4, Revoked: []string{"worker"}}
	b := New(c)
	if b.MinEpoch() != 4 {
		t.Fatalf("MinEpoch = %d, want 4 (seeded)", b.MinEpoch())
	}
	if !b.isRevoked("worker") {
		t.Fatal("seeded revoked agent must read as revoked")
	}
}

func TestBumpEpochPersistsAndRaises(t *testing.T) {
	c := testConfig(t)
	var saved RevocationState
	c.PersistRevocation = func(rs RevocationState) error { saved = rs; return nil }
	b := New(c)
	r := httptest.NewRequest("POST", "/bump-epoch", nil)
	w := httptest.NewRecorder()
	b.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || b.MinEpoch() != 1 || saved.MinEpoch != 1 {
		t.Fatalf("code=%d epoch=%d saved=%+v", w.Code, b.MinEpoch(), saved)
	}
}

func TestRevokePersistsAgent(t *testing.T) {
	c := testConfig(t)
	var saved RevocationState
	c.PersistRevocation = func(rs RevocationState) error { saved = rs; return nil }
	b := New(c)
	r := httptest.NewRequest("POST", "/revoke", bytes.NewReader([]byte(`{"agent":"worker"}`)))
	w := httptest.NewRecorder()
	b.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !b.isRevoked("worker") || len(saved.Revoked) != 1 {
		t.Fatalf("code=%d revoked=%v saved=%+v", w.Code, b.isRevoked("worker"), saved)
	}
}
