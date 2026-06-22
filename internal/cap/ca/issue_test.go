package ca

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestIssueAgentCertHasAgentCNAndVerifies(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := c.IssueAgentCert("scratch")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("empty key")
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
	if leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("agent cert EKU = %v, want ClientAuth", leaf.ExtKeyUsage)
	}

	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("leaf does not verify against CA: %v", err)
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
