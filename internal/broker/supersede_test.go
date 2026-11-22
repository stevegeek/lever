package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// issueLeaf signs a fresh client leaf for cn as enrol does.
func issueLeaf(t *testing.T, b *Broker, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := b.ca.SignPublicKeyFrom(key.Public(), cn, b.leafFrom(cn))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// gated sends one request with leaf as its client certificate through the
// gate in front of a handler that answers 200.
func gated(b *Broker, leaf *x509.Certificate) int {
	h := b.supersededGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	r := httptest.NewRequest(http.MethodGet, "/tools", nil)
	if leaf != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// A leaf issued before the purge is refused; one issued after it, even in
// the same second, passes; other names and certless requests are untouched.
func TestSupersedeRefusesOlderLeavesOnly(t *testing.T) {
	var saved []RevocationState
	b := New(testConfig(t, func(c *Config) {
		c.Persistence.PersistRevocation = func(rs RevocationState) error { saved = append(saved, rs); return nil }
	}))
	old := issueLeaf(t, b, "worker")
	other := issueLeaf(t, b, "scratch")
	if gated(b, old) != http.StatusOK {
		t.Fatal("refused before any purge")
	}
	if err := b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	if got := gated(b, old); got != http.StatusForbidden {
		t.Fatalf("old leaf after the purge: %d, want 403", got)
	}
	fresh := issueLeaf(t, b, "worker") // within the cutoff's second: stamped at it
	if got := gated(b, fresh); got != http.StatusOK {
		t.Fatalf("leaf issued after the purge: %d, want 200 (issued %v)", got, ca.LeafIssuedAt(fresh))
	}
	if fresh.NotBefore.After(time.Now()) {
		t.Fatalf("fresh leaf is not valid yet: NotBefore %v", fresh.NotBefore)
	}
	if gated(b, other) != http.StatusOK || gated(b, nil) != http.StatusOK {
		t.Fatal("the gate touched another name or a certless request")
	}
	if len(saved) != 1 || saved[0].Superseded["worker"].IsZero() {
		t.Fatalf("saved %+v", saved)
	}
}

// The cutoff survives a restart, and Revoked is kept beside it.
func TestSupersedeIsPersisted(t *testing.T) {
	var saved RevocationState
	b := New(testConfig(t, func(c *Config) {
		c.Persistence.Revocation = RevocationState{Revoked: []string{"gone"}}
		c.Persistence.PersistRevocation = func(rs RevocationState) error { saved = rs; return nil }
	}))
	old := issueLeaf(t, b, "worker")
	if err := b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	if len(saved.Revoked) != 1 || saved.Revoked[0] != "gone" {
		t.Fatalf("revoked list lost: %+v", saved)
	}
	b2 := New(testConfig(t, func(c *Config) { c.Persistence.Revocation = saved }))
	b2.ca = b.ca
	if got := gated(b2, old); got != http.StatusForbidden {
		t.Fatalf("after a restart: %d, want 403", got)
	}
}

// The jail handler carries the gate on every route.
func TestJailHandlerIsGated(t *testing.T) {
	b := New(testConfig(t))
	old := issueLeaf(t, b, "worker")
	if err := b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/tools", "/request", "/renew", "/mcp/db/"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{old}}
		rec := httptest.NewRecorder()
		b.JailHandler().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, rec.Code)
		}
	}
}

// A cutoff in the past leaves issuing alone.
func TestLeafFromPastCutoff(t *testing.T) {
	b := New(testConfig(t))
	b.clock = func() time.Time { return time.Now().Add(-time.Hour) }
	if err := b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	b.clock = nil
	leaf := issueLeaf(t, b, "worker")
	if d := time.Since(ca.LeafIssuedAt(leaf)); d < 0 || d > 5*time.Second {
		t.Fatalf("issued %v ago, want now", d)
	}
}

// On the jail server the cutoff runs before the lapse gate: a superseded
// leaf that has also expired is refused without asking the healer to bounce
// the name's new agent.
func TestSupersededExpiredLeafIsNotReportedAsALapse(t *testing.T) {
	b := New(testConfig(t))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The handler reads only the leaf's times and name: the handshake, not
	// run here, checks its chain.
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "worker"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	if err := b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	var lapsed []string
	srv := b.jailServer(nil, func(cn string) { lapsed = append(lapsed, cn) })
	r := httptest.NewRequest(http.MethodGet, "/tools", nil)
	r = r.WithContext(ca.ConnContext(r.Context(), nil))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || len(lapsed) != 0 {
		t.Fatalf("%d, lapses reported %v", rec.Code, lapsed)
	}
}
