// Package ca is a tiny internal CA: it issues per-agent mTLS certs whose
// CommonName is the agent identity, and proves the caller identity of a TLS
// connection. The broker uses it for non-transferable, caller-bound capabilities.
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
	"os"
	"time"
)

// osStat is a package indirection so tests can assert file permissions.
var osStat = os.Stat

// CA is a self-signed certificate authority.
type CA struct {
	Cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey
}

// Generate creates a fresh self-signed CA (ECDSA P-256, 10-year lifetime).
func Generate() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generate key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lever-broker-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca: create cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: parse cert: %w", err)
	}
	return &CA{Cert: cert, certDER: der, key: key}, nil
}

// CertPEM returns the CA certificate in PEM form.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.certDER})
}

// SaveCert writes the CA cert PEM (0644 — public).
func (c *CA) SaveCert(path string) error {
	if err := os.WriteFile(path, c.CertPEM(), 0o644); err != nil {
		return fmt.Errorf("ca: save cert: %w", err)
	}
	return nil
}

// SaveKey writes the CA private key PEM (0600 — secret).
func (c *CA) SaveKey(path string) error {
	der, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return fmt.Errorf("ca: marshal key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("ca: save key: %w", err)
	}
	return nil
}

// Load reads a CA cert + key from PEM files.
func Load(certPath, keyPath string) (*CA, error) {
	certRaw, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("ca: read cert: %w", err)
	}
	keyRaw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ca: read key: %w", err)
	}
	certBlock, _ := pem.Decode(certRaw)
	if certBlock == nil {
		return nil, fmt.Errorf("ca: cert PEM is invalid")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse cert: %w", err)
	}
	keyBlock, _ := pem.Decode(keyRaw)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca: key PEM is invalid")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse key: %w", err)
	}
	return &CA{Cert: cert, certDER: certBlock.Bytes, key: key}, nil
}
