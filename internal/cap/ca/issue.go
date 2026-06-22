package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// certTTL bounds a leaf cert's lifetime. Short by design; rotation is a later milestone.
const certTTL = 24 * time.Hour

// IssueAgentCert mints a short-lived (24h) client cert whose CommonName is the
// agent identity. The broker compares this CN to a capability's bound agent.
func (c *CA) IssueAgentCert(agent string) (certPEM, keyPEM []byte, err error) {
	return c.issue(agent, x509.ExtKeyUsageClientAuth, nil)
}

// IssueServerCert mints a short-lived (24h) server cert for the given hostname.
func (c *CA) IssueServerCert(host string) (certPEM, keyPEM []byte, err error) {
	return c.issue(host, x509.ExtKeyUsageServerAuth, []string{host})
}

func (c *CA) issue(cn string, eku x509.ExtKeyUsage, dnsNames []string) (certPEM, keyPEM []byte, err error) {
	if cn == "" {
		return nil, nil, fmt.Errorf("ca: empty common name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: generate leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("ca: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(certTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: create leaf: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: marshal leaf key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
