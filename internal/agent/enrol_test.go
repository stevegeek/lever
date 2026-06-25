package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/broker"
	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// testBroker builds a broker that permits provisioning grove "worker" and a CA
// server cert, returns the broker + a TLS server over its JailHandler + the CA
// instance (for cert signing in provisionAs) + the token keypair (for Tasks 2–4).
func testBroker(t *testing.T) (*broker.Broker, *httptest.Server, *ca.CA, token.KeyPair) {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	pol := rules.NewPolicy()
	b := broker.New(broker.Config{
		Keys:            kp,
		CA:              caInst,
		Tickets:         ca.NewTicketStore(),
		Rules:           pol,
		Registry:        reg,
		ManagerIdentity: "manager",
		Agents:          []string{"worker"},
		ServerName:      "127.0.0.1",
	})
	certPEM, keyPEM, err := caInst.IssueServerCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := caInst.ServerTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(b.JailHandler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return b, srv, caInst, kp
}

// csrWithKey returns a CSR PEM and private key PEM for cn.
func csrWithKey(t *testing.T, cn string) (csrPEM, keyPEM []byte) {
	t.Helper()
	csr, key, err := GenerateCSR(cn)
	if err != nil {
		t.Fatal(err)
	}
	return csr, key
}

// provisionAs signs a manager client cert with the CA, builds an mTLS client,
// POSTs /provision {grove}, and returns the ticket string.
func provisionAs(t *testing.T, b *broker.Broker, srv *httptest.Server, caInst *ca.CA, grove string) string {
	t.Helper()

	// Build a manager cert signed by the broker CA (mirrors signedCert in broker e2e_test).
	csrPEM, keyPEM := csrWithKey(t, "manager")
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatalf("provisionAs: sign manager CSR: %v", err)
	}
	managerCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("provisionAs: build manager tls.Certificate: %v", err)
	}

	// Build an mTLS client that trusts the broker CA and presents the manager cert.
	pool := x509.NewCertPool()
	pool.AddCert(caInst.Cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{managerCert},
	}}}

	body, _ := json.Marshal(map[string]string{"grove": grove})
	resp, err := client.Post(srv.URL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("provisionAs: POST /provision: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provisionAs: status %d", resp.StatusCode)
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("provisionAs: decode: %v", err)
	}
	if result.Ticket == "" {
		t.Fatal("provisionAs: empty ticket")
	}
	return result.Ticket
}

// parseLeaf decodes the first certificate from certPEM.
func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("parseLeaf: invalid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parseLeaf: %v", err)
	}
	return cert
}

// assertMode checks that the file at path has the given permission bits.
func assertMode(t *testing.T, path string, want uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("assertMode: stat %s: %v", path, err)
	}
	got := uint32(info.Mode().Perm())
	if got != want {
		t.Fatalf("assertMode: %s mode = %04o, want %04o", path, got, want)
	}
}

func TestEnrolReturnsSignedIdentity(t *testing.T) {
	b, srv, caInst, _ := testBroker(t)
	ticket := provisionAs(t, b, srv, caInst, "worker")
	id, err := Enrol(context.Background(), srv.URL, caInst.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatal(err)
	}
	leaf := parseLeaf(t, id.CertPEM)
	if leaf.Subject.CommonName != "worker" {
		t.Fatalf("enrolled CN = %q, want worker", leaf.Subject.CommonName)
	}
	if !ValidCert(id.CertPEM, time.Now()) {
		t.Fatal("freshly enrolled cert must be valid now")
	}
}

func TestEnrolRejectsCNMismatch(t *testing.T) {
	b, srv, caInst, _ := testBroker(t)
	ticket := provisionAs(t, b, srv, caInst, "worker")
	// A CSR CN that doesn't match the ticket's grove must be rejected by the broker.
	if _, err := Enrol(context.Background(), srv.URL, caInst.CertPEM(), ticket, "evil"); err == nil {
		t.Fatal("enrol with CN != ticket grove must fail")
	}
}

func TestWriteIdentityPermissions(t *testing.T) {
	id := Identity{CertPEM: []byte("c"), KeyPEM: []byte("k"), CAPEM: []byte("a")}
	dir := t.TempDir() + "/id"
	if err := id.Write(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0o700)
	assertMode(t, dir+"/agent.crt", 0o644)
	assertMode(t, dir+"/agent.key", 0o600)
	assertMode(t, dir+"/ca.crt", 0o644)
	got, ok := LoadIdentity(dir)
	if !ok || string(got.KeyPEM) != "k" {
		t.Fatalf("LoadIdentity round-trip failed: ok=%v", ok)
	}
}
