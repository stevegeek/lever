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

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// brokerEnv holds all test-side handles for a broker under test.
type brokerEnv struct {
	Broker   *broker.Broker
	Server   *httptest.Server
	CA       *ca.CA
	Keys     token.KeyPair
	Rules    *rules.Policy
	Registry *registry.Registry
}

// testBroker builds a broker that permits provisioning grove "worker" and a CA
// server cert, and returns a brokerEnv with all relevant handles for test setup
// and assertion (including the policy and registry instances the broker was built
// from, so callers can drive them directly without any production accessor).
func testBroker(t *testing.T) *brokerEnv {
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
	return &brokerEnv{Broker: b, Server: srv, CA: caInst, Keys: kp, Rules: pol, Registry: reg}
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
func provisionAs(t *testing.T, b *broker.Broker, srv *httptest.Server, caInst *ca.CA, worker string) string {
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

	body, _ := json.Marshal(map[string]string{"worker": worker})
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
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
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
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	// A CSR CN that doesn't match the ticket's grove must be rejected by the broker.
	if _, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "evil"); err == nil {
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

func TestLoadIdentityMissingReturnsFalse(t *testing.T) {
	// Boot's idempotency hinges on this: an empty dir must report ok=false so Boot
	// re-enrols rather than proceeding with a non-existent identity (a wrong true
	// here would skip enrolment and then fail building the mTLS client).
	if _, ok := LoadIdentity(t.TempDir()); ok {
		t.Fatal("LoadIdentity on an empty dir must return ok=false")
	}
}
