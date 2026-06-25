// Package agent is the in-jail lever-agent core: it owns the agent's keypair
// (generated in-container, never leaves), enrols + renews its mTLS identity with
// the broker, mints/attenuates capability tokens on the LLM's behalf, and serves
// the capability MCP tool.
package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
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
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agent: bad CA PEM")
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
	body, _ := json.Marshal(map[string]string{"ticket": ticket, "csr": string(csrPEM)})
	req, err := http.NewRequestWithContext(ctx, "POST", brokerURL+"/enrol", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: enrol: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("agent: enrol status %d", resp.StatusCode)
	}
	var er struct {
		Cert string `json:"cert"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return Identity{}, fmt.Errorf("agent: enrol decode: %w", err)
	}
	return Identity{CertPEM: []byte(er.Cert), KeyPEM: keyPEM, CAPEM: caPEM}, nil
}

// Write persists the identity: agent.crt/ca.crt 0644, agent.key 0600, dir 0700.
func (id Identity) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
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

// ValidCert reports whether certPEM's leaf is currently within its validity.
func ValidCert(certPEM []byte, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return now.After(leaf.NotBefore) && now.Before(leaf.NotAfter)
}
