package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
)

// makeCSR builds a PEM-encoded CSR for cn, returning the CSR (the private key
// stays with the caller, mirroring an agent generating its own key).
func makeCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignCSRHasAgentCNClientAuthAndVerifies(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := c.SignCSR(makeCSR(t, "scratch"))
	if err != nil {
		t.Fatalf("sign csr: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert PEM invalid")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "scratch" {
		t.Errorf("CN = %q, want scratch", leaf.Subject.CommonName)
	}
	if len(leaf.ExtKeyUsage) == 0 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("EKU = %v, want ClientAuth", leaf.ExtKeyUsage)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("leaf does not verify against CA: %v", err)
	}
}

func TestSignCSRRejectsGarbage(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignCSR([]byte("not a csr")); err == nil {
		t.Fatal("expected error for non-CSR input")
	}
}

func TestSignCSRRejectsEmptyCN(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignCSR(makeCSR(t, "")); err == nil {
		t.Fatal("expected error for empty CN CSR")
	}
}

func TestIssueServerCertHasServerAuthAndDNS(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := c.IssueServerCert("host.orb.internal")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("server cert EKU = %v, want ServerAuth", leaf.ExtKeyUsage)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "host.orb.internal" {
		t.Errorf("DNSNames = %v, want [host.orb.internal]", leaf.DNSNames)
	}
}
