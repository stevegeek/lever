// Package agent is the in-jail lever-agent core: it owns the agent's keypair
// (generated in-container, never leaves), enrols + renews its mTLS identity with
// the broker, mints/attenuates capability tokens on the LLM's behalf, and serves
// the capability MCP tool.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Identity is the agent's enrolled mTLS material (PEM).
type Identity struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

// GenerateCSR creates an EC P-256 keypair in-process and a CSR with the given CN.
func GenerateCSR(cn string) (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: generate key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: create CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: marshal key: %w", err)
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return csrPEM, keyPEM, nil
}

// caClient builds an HTTPS client that trusts caPEM (server-authenticated; the
// agent has no client cert yet at enrol — /enrol uses VerifyClientCertIfGiven).
func caClient(caPEM []byte) (*http.Client, error) {
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, nil
}

// Enrol generates a keypair + CSR (CN=cn) and redeems the ticket at /enrol,
// returning the signed identity. The private key never leaves this process.
func Enrol(ctx context.Context, brokerURL string, caPEM []byte, ticket, cn string) (Identity, error) {
	csrPEM, keyPEM, err := GenerateCSR(cn)
	if err != nil {
		return Identity{}, err
	}
	client, err := caClient(caPEM)
	if err != nil {
		return Identity{}, err
	}
	var er wire.EnrolResponse
	if err := httpjson.Post(ctx, client, brokerURL+wire.PathEnrol, wire.EnrolRequest{Ticket: ticket, CSR: string(csrPEM)}, &er); err != nil {
		return Identity{}, fmt.Errorf("agent: enrol: %w", err)
	}
	return Identity{CertPEM: []byte(er.Cert), KeyPEM: keyPEM, CAPEM: caPEM}, nil
}

// Write persists the identity: agent.crt/ca.crt 0644, agent.key 0600, dir 0700.
func (id Identity) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("agent: chmod dir: %w", err)
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"agent.crt", id.CertPEM, 0o644},
		{"agent.key", id.KeyPEM, 0o600},
		{"ca.crt", id.CAPEM, 0o644},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return fmt.Errorf("agent: write %s: %w", f.name, err)
		}
	}
	return nil
}

// LoadIdentity reads a previously-written identity from dir.
func LoadIdentity(dir string) (Identity, bool) {
	cert, err1 := os.ReadFile(filepath.Join(dir, "agent.crt"))
	key, err2 := os.ReadFile(filepath.Join(dir, "agent.key"))
	caPEM, err3 := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err1 != nil || err2 != nil || err3 != nil {
		return Identity{}, false
	}
	return Identity{CertPEM: cert, KeyPEM: key, CAPEM: caPEM}, true
}

// parseLeafPEM parses the first certificate in certPEM.
func parseLeafPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("agent: invalid cert PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("agent: parse cert: %w", err)
	}
	return leaf, nil
}

// CN returns the common name of the identity's leaf certificate — the name the
// broker authenticated this agent under, and so the bound_to of a self-obtain.
func (id Identity) CN() (string, error) {
	leaf, err := parseLeafPEM(id.CertPEM)
	if err != nil {
		return "", err
	}
	return leaf.Subject.CommonName, nil
}

// ValidCert reports whether certPEM's leaf is currently within its validity.
func ValidCert(certPEM []byte, now time.Time) bool {
	leaf, err := parseLeafPEM(certPEM)
	if err != nil {
		return false
	}
	return now.After(leaf.NotBefore) && now.Before(leaf.NotAfter)
}
