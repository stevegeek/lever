package broker

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

type x509Cert = x509.Certificate

func mustLeaf(t *testing.T, certPEM, _ []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
