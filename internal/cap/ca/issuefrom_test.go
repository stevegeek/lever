package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func signFrom(t *testing.T, c *CA, earliest time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := c.SignPublicKeyFrom(key.Public(), "worker", earliest)
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

// A leaf is stamped no earlier than earliest, stays valid at once, and a past
// or zero earliest is the plain issue time.
func TestSignPublicKeyFrom(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	ahead := time.Now().Truncate(time.Second).Add(2 * time.Second)
	leaf := signFrom(t, c, ahead)
	if !LeafIssuedAt(leaf).Equal(ahead) {
		t.Fatalf("issued %v, want %v", LeafIssuedAt(leaf), ahead)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: poolOf(c), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("not valid now: %v", err)
	}
	for _, earliest := range []time.Time{{}, time.Now().Add(-time.Hour)} {
		before := time.Now().Truncate(time.Second)
		got := LeafIssuedAt(signFrom(t, c, earliest))
		if got.Before(before) || got.After(time.Now()) {
			t.Fatalf("earliest %v: issued %v, want now", earliest, got)
		}
	}
}

func poolOf(c *CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.Cert)
	return p
}
