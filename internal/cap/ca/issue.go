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
	"net"
	"time"
)

// certTTL bounds a leaf cert's lifetime. Rotation keeps it fresh: the
// broker's serving cert self-rotates (ServerCertSource) and agent leafs renew
// via the lever-renew sidecar hitting /renew.
const certTTL = 24 * time.Hour

// leafTemplate is the single place the leaf-cert security policy lives:
// random 128-bit serial, 1-minute clock-skew backdate, certTTL lifetime,
// digital-signature key usage, exactly one EKU.
func leafTemplate(cn string, eku x509.ExtKeyUsage, dnsNames []string, ipAddrs []net.IP) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca: serial: %w", err)
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(certTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}, nil
}

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
	return c.SignPublicKey(csr.PublicKey, csr.Subject.CommonName)
}

// SignPublicKey signs pub into a short-lived ClientAuth cert with the given
// CommonName. /renew uses it to stamp the authenticated identity, never a
// CSR-supplied CN.
func (c *CA) SignPublicKey(pub crypto.PublicKey, cn string) ([]byte, error) {
	if cn == "" {
		return nil, fmt.Errorf("ca: empty common name")
	}
	tmpl, err := leafTemplate(cn, x509.ExtKeyUsageClientAuth, nil, nil)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, pub, c.key)
	if err != nil {
		return nil, fmt.Errorf("ca: sign public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// IssueServerCertSANs mints a server cert with the given DNS and IP SANs and
// cn as CommonName. The IP SAN is needed under closed-internet egress, where
// DNS is dropped and the agent dials the allowlisted IP directly. ""
// entries are ignored.
func (c *CA) IssueServerCertSANs(cn string, dnsNames []string, ips []string) (certPEM, keyPEM []byte, err error) {
	var dns []string
	for _, d := range dnsNames {
		if d != "" {
			dns = append(dns, d)
		}
	}
	var parsed []net.IP
	for _, s := range ips {
		if s == "" {
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, nil, fmt.Errorf("ca: invalid IP SAN %q", s)
		}
		parsed = append(parsed, ip)
	}
	return c.issue(cn, x509.ExtKeyUsageServerAuth, dns, parsed)
}

func (c *CA) issue(cn string, eku x509.ExtKeyUsage, dnsNames []string, ipAddrs []net.IP) (certPEM, keyPEM []byte, err error) {
	if cn == "" {
		return nil, nil, fmt.Errorf("ca: empty common name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: generate leaf key: %w", err)
	}
	tmpl, err := leafTemplate(cn, eku, dnsNames, ipAddrs)
	if err != nil {
		return nil, nil, err
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
