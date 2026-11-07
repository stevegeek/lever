// Package agent is the in-jail lever-agent core: it owns the agent's keypair
// (generated in-container; it never leaves), enrols and renews its mTLS
// identity with the broker, mints capability tokens on the LLM's behalf, and
// serves the capability MCP tool.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
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

// caClient has no client cert: at enrol the agent has none yet.
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
// dir's parent must exist. The work is confined to that parent and refuses a
// symlink at dir or at any of the three files (see errRefusedPath).
func (id Identity) Write(dir string) error {
	root, base := splitAbove(dir, 1)
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("agent: open %s: %w", root, err)
	}
	defer r.Close()
	if err := refuseLink(r, base, true); err != nil {
		return err
	}
	if err := r.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if err := r.Chmod(base, 0o700); err != nil {
		return fmt.Errorf("agent: chmod dir: %w", err)
	}
	// Each file is replaced by rename, never rewritten in place: the renew
	// sidecar rewrites the identity while CLI verbs and the MCP server read it,
	// and a crash mid-write must not leave a truncated file. The key goes
	// first, so the files left by a crash between two renames are a new key
	// beside an old cert, which LoadIdentity reports as no identity (the
	// pair does not match) rather than as a valid one.
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"agent.key", id.KeyPEM, 0o600},
		{"agent.crt", id.CertPEM, 0o644},
		{"ca.crt", id.CAPEM, 0o644},
	}
	// Refuse a link at any of the three names before replacing the first.
	for _, f := range files {
		if err := refuseLink(r, filepath.Join(base, f.name), false); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := replaceFile(r, filepath.Join(base, f.name), f.data, f.mode); err != nil {
			return fmt.Errorf("agent: write %s: %w", f.name, err)
		}
	}
	// Make the renames durable, so a power loss cannot keep the new cert
	// and lose the new key. Best effort.
	if d, err := r.Open(base); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// replaceFile writes data to a fresh temporary file beside name, syncs it and
// renames it over name, all inside r.
func replaceFile(r *os.Root, name string, data []byte, mode os.FileMode) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+".tmp-"+hex.EncodeToString(b[:]))
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode) // O_CREATE's mode is filtered by the umask
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, name)
	}
	if err != nil {
		_ = r.Remove(tmp)
	}
	return err
}

// LoadIdentity reads a previously-written identity from dir.
func LoadIdentity(dir string) (Identity, bool) {
	// A read that falls between Write's renames sees a new key beside the
	// old cert: read once more before reporting no identity.
	if id, ok := loadIdentityOnce(dir); ok {
		return id, true
	}
	time.Sleep(50 * time.Millisecond)
	return loadIdentityOnce(dir)
}

func loadIdentityOnce(dir string) (Identity, bool) {
	cert, err1 := os.ReadFile(filepath.Join(dir, "agent.crt"))
	key, err2 := os.ReadFile(filepath.Join(dir, "agent.key"))
	caPEM, err3 := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err1 != nil || err2 != nil || err3 != nil {
		return Identity{}, false
	}
	// A key that does not match the cert (a crash between Write's renames)
	// is no identity: Boot then enrols again instead of keeping a pair no
	// TLS client can use.
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return Identity{}, false
	}
	return Identity{CertPEM: cert, KeyPEM: key, CAPEM: caPEM}, true
}

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

// CN returns the common name of the identity's leaf certificate, the name the
// broker authenticated this agent under.
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
