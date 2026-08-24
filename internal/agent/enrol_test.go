package agent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/brokertest"
	"github.com/stevegeek/lever/internal/broker/registry"
)

// testBroker builds a broker that permits provisioning worker "worker" and a CA
// server cert, and returns a brokertest.Env with all relevant handles for test setup
// and assertion (including the policy and registry instances the broker was built
// from, so callers can drive them directly without any production accessor).
func testBroker(t *testing.T) *brokertest.Env {
	t.Helper()
	return brokertest.NewTestBroker(t, brokertest.Config{})
}

// allowLLM registers the broker's built-in llm pseudo-tool and grants agent
// permission to self-obtain it, so RequestLLMToken / RefreshLLMToken succeed
// against env.
func allowLLM(t *testing.T, env *brokertest.Env, agent string) {
	t.Helper()
	env.Rules.AllowObtain(agent, broker.ReservedLLMTool, broker.ReservedLLMOp)
	if err := env.Registry.Register(registry.Tool{
		Name:       broker.ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{broker.ReservedLLMOp: {Name: broker.ReservedLLMOp}},
		FirstParty: true,
	}); err != nil {
		t.Fatalf("allowLLM: register llm tool: %v", err)
	}
}

// enrolWorker provisions and enrols "worker" against env, returning its identity.
func enrolWorker(t *testing.T, env *brokertest.Env) Identity {
	t.Helper()
	ticket := env.ProvisionWorker(t, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatalf("enrolWorker: %v", err)
	}
	return id
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
	ticket := env.ProvisionWorker(t, "worker")
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
	ticket := env.ProvisionWorker(t, "worker")
	// A CSR CN that doesn't match the ticket's worker must be rejected by the broker.
	if _, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "evil"); err == nil {
		t.Fatal("enrol with CN != ticket worker must fail")
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

func TestIdentityCN(t *testing.T) {
	env := testBroker(t)
	id := enrolWorker(t, env)
	cn, err := id.CN()
	if err != nil {
		t.Fatal(err)
	}
	if cn != "worker" {
		t.Fatalf("CN = %q, want worker", cn)
	}
	if _, err := (Identity{CertPEM: []byte("not pem")}).CN(); err == nil {
		t.Fatal("CN on invalid PEM must error")
	}
}
