package broker

// e2e_test.go exercises the full provision→enrol→request→exercise path over
// real mTLS: a live httptest.Server with the broker CA's ServerTLSConfigSource, real
// TLS clients that pin the broker CA, and a fake upstream MCP backend.

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

const e2eServerName = "broker.test"

// jailServer starts an httptest server using the broker's JailHandler() over
// real mTLS (broker CA ServerTLSConfigSource). Tools must be registered before this
// call because JailHandler() binds gateway routes at call time.
func jailServer(t *testing.T, b *Broker) *httptest.Server {
	t.Helper()
	src, err := b.ca.NewServerCertSource(e2eServerName, []string{e2eServerName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := b.ca.ServerTLSConfigSource(src, nil)
	srv := httptest.NewUnstartedServer(b.JailHandler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	return srv
}

// agentClient builds an HTTP client that pins the broker CA and presents the
// supplied TLS certificate as its client cert. Pass a zero tls.Certificate for
// a certless client (e.g. the worker before it has enrolled).
func agentClient(t *testing.T, b *Broker, cert tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)
	cfg := &tls.Config{
		RootCAs:    pool,
		ServerName: e2eServerName,
	}
	if len(cert.Certificate) > 0 {
		cfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

// TestE2EProvisionEnrolRequestExercise exercises the full four-step flow over
// real mTLS and a fake upstream MCP backend.
func TestE2EProvisionEnrolRequestExercise(t *testing.T) {
	// ── Setup: fake upstream backend ──────────────────────────────────────────
	var backendReached bool
	var backendBody string
	upstream := upstreamMCP(t, &backendReached, &backendBody)
	defer upstream.Close()

	// ── Setup: broker + rules ─────────────────────────────────────────────────
	cfg := testConfig(t) // includes AllowDelegate("manager","db","read","worker")
	b := New(cfg)

	// Register db tool BEFORE JailHandler() so the gateway route is wired.
	if err := b.reg.Register(regTool("db", upstream.URL, "read")); err != nil {
		t.Fatal(err)
	}

	// ── Setup: live mTLS server ───────────────────────────────────────────────
	srv := jailServer(t, b)
	defer srv.Close()

	// ── Manager cert (real TLS cert for the manager identity) ─────────────────
	managerCert := signedCert(t, b, "manager")
	managerClient := agentClient(t, b, managerCert)

	// ─────────────────────────────────────────────────────────────────────────
	// Step 1: provision — manager POSTs /provision {worker:"worker"} → ticket.
	// ─────────────────────────────────────────────────────────────────────────
	provBody, _ := json.Marshal(wire.ProvisionRequest{Worker: "worker"})
	provResp, err := managerClient.Post(srv.URL+"/provision", "application/json", bytes.NewReader(provBody))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer provResp.Body.Close()
	if provResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(provResp.Body)
		t.Fatalf("provision: status=%d body=%s", provResp.StatusCode, body)
	}
	var provResult wire.ProvisionResponse
	if err := json.NewDecoder(provResp.Body).Decode(&provResult); err != nil {
		t.Fatalf("provision: decode: %v", err)
	}
	if provResult.Ticket == "" {
		t.Fatal("provision: empty ticket")
	}
	ticket := provResult.Ticket

	// ─────────────────────────────────────────────────────────────────────────
	// Step 2: enrol — worker self-generates a keypair, POSTs /enrol {ticket,csr}
	// with NO client cert → gets a signed cert.
	// ─────────────────────────────────────────────────────────────────────────
	workerCSRPEM, workerKeyPEM := csrWithKey(t, "worker")
	enrolReqBody, _ := json.Marshal(wire.EnrolRequest{
		Ticket: ticket,
		CSR:    string(workerCSRPEM),
	})
	// certless client: pins broker CA, no client cert.
	certlessClient := agentClient(t, b, tls.Certificate{})
	enrolResp, err := certlessClient.Post(srv.URL+"/enrol", "application/json", bytes.NewReader(enrolReqBody))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	defer enrolResp.Body.Close()
	if enrolResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(enrolResp.Body)
		t.Fatalf("enrol: status=%d body=%s", enrolResp.StatusCode, body)
	}
	var enrolResult wire.EnrolResponse
	if err := json.NewDecoder(enrolResp.Body).Decode(&enrolResult); err != nil {
		t.Fatalf("enrol: decode: %v", err)
	}
	if enrolResult.Cert == "" {
		t.Fatal("enrol: empty cert")
	}

	// Build a real tls.Certificate from the enrolled cert + worker's own key.
	workerTLSCert, err := tls.X509KeyPair([]byte(enrolResult.Cert), workerKeyPEM)
	if err != nil {
		t.Fatalf("enrol: parse tls.Certificate: %v", err)
	}
	workerClient := agentClient(t, b, workerTLSCert)

	// ─────────────────────────────────────────────────────────────────────────
	// Step 3: request (delegation) — manager POSTs /request {tool:"db",
	// op:"read", bound_to:"worker"} → capability token bound to worker.
	// ─────────────────────────────────────────────────────────────────────────
	capReqBody, _ := json.Marshal(wire.CapRequest{Tool: "db", Op: "read", BoundTo: "worker"})
	capResp, err := managerClient.Post(srv.URL+"/request", "application/json", bytes.NewReader(capReqBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer capResp.Body.Close()
	if capResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(capResp.Body)
		t.Fatalf("request: status=%d body=%s", capResp.StatusCode, body)
	}
	var capResult wire.CapResponse
	if err := json.NewDecoder(capResp.Body).Decode(&capResult); err != nil {
		t.Fatalf("request: decode: %v", err)
	}
	if capResult.Token == "" {
		t.Fatal("request: empty token")
	}
	capToken := capResult.Token

	// ─────────────────────────────────────────────────────────────────────────
	// Step 4: exercise (allowed) — worker presents its enrolled cert + token
	// in _capability, POSTs tools/call to /mcp/db/ → backend reached, token
	// stripped from the upstream body, gateway returns 200.
	// ─────────────────────────────────────────────────────────────────────────
	allowedBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + capToken + `"}}}`
	mcpResp, err := workerClient.Post(srv.URL+"/mcp/db/", "application/json", strings.NewReader(allowedBody))
	if err != nil {
		t.Fatalf("exercise allowed: %v", err)
	}
	defer mcpResp.Body.Close()
	if mcpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(mcpResp.Body)
		t.Fatalf("exercise allowed: status=%d body=%s", mcpResp.StatusCode, body)
	}
	if !backendReached {
		t.Fatal("exercise allowed: backend should have been reached on a valid call")
	}
	if strings.Contains(backendBody, "_capability") {
		t.Fatalf("exercise allowed: token leaked upstream: %s", backendBody)
	}

	// ─────────────────────────────────────────────────────────────────────────
	// Step 5: exercise (denied) — second call with a constraint-violating
	// token (constrained to table=A, request asks for table=B) → 403, backend
	// NOT reached again.
	// ─────────────────────────────────────────────────────────────────────────
	// Mint a token constrained to table=A; the request sends table=B.
	constrainedToken := mintFor(t, b, "worker", map[string]string{"table": "A"})
	backendReached = false // reset the reached flag

	deniedBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"B","_capability":"` + constrainedToken + `"}}}`
	deniedResp, err := workerClient.Post(srv.URL+"/mcp/db/", "application/json", strings.NewReader(deniedBody))
	if err != nil {
		t.Fatalf("exercise denied: %v", err)
	}
	defer deniedResp.Body.Close()
	if deniedResp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(deniedResp.Body)
		t.Fatalf("exercise denied: status=%d want 403, body=%s", deniedResp.StatusCode, body)
	}
	if backendReached {
		t.Fatal("SECURITY: exercise denied: backend reached despite constraint violation")
	}
}
