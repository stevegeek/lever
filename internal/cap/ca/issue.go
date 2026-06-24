package ca

import (
	"crypto"
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

// SignCSR signs an agent-generated CSR into a short-lived client cert whose
// CommonName is taken from the CSR subject. The CA never sees the private key.
func (c *CA) SignCSR(csrPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, fmt.Errorf("ca: CSR PEM is invalid")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("ca: CSR signature: %w", err)
	}
	if csr.Subject.CommonName == "" {
		return nil, fmt.Errorf("ca: CSR has empty common name")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(certTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("ca: sign csr: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// SignPublicKey signs an externally-provided public key into a short-lived
// ClientAuth cert with the given CommonName. Unlike SignCSR, the CN is chosen by
// the caller — used by /renew to stamp the authenticated identity, never a
// CSR-supplied CN.
func (c *CA) SignPublicKey(pub crypto.PublicKey, cn string) ([]byte, error) {
	if cn == "" {
		return nil, fmt.Errorf("ca: empty common name")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(certTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, pub, c.key)
	if err != nil {
		return nil, fmt.Errorf("ca: sign public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
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
