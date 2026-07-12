package brokerctl

// integration_test.go is the host-side host-side acceptance test. It
// drives the WHOLE host-side capability stack end to end:
//
//	config.Load(lever.yaml)  →  EnsureKeys + LoadRevocation + BuildBroker + broker.New
//	→  ServeListeners on real loopback listeners  →  a REAL lever-tool-db SUBPROCESS
//	(supervised, self-registering over the admin listener)  →  a simulated worker
//	agent (CA-issued client cert + a directly-minted, broker-key-signed delegated
//	token) exercising the gated /mcp/db/ gateway over mTLS.
//
// Unlike internal/broker/e2e_captool_test.go (which runs an INLINE captool server
// behind jailServer), this test runs the broker exactly as `brokerctl.Serve`
// assembles it, but inline so the test can bind 127.0.0.1:0 for BOTH the jail and
// admin listeners and learn the OS-assigned ports. The "db" backend is the real
// cmd/lever-tool-db binary, built to a temp path and launched by the Supervisor.
//
// It asserts the host-side scenario: a delegated, table=A/filter=alice-constrained
// db.read returns ONLY alice's rows; table=C and a dropped filter are denied with
// no rows reaching the agent; /bootstrap is a single-shot latch; and revocation
// (bump-epoch) denies the next call AND survives a broker restart from the same
// persisted state.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
)

// csrWithKey returns a PEM CSR for cn plus the matching EC private key PEM, so the
// test can present the CA-signed cert as a client (the simulated worker).
func csrWithKey(t *testing.T, cn string) (csrPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// freeLoopbackPort grabs an OS-assigned loopback port, then releases it so a
// child process (the real lever-tool-db) can bind it as its own MCP backend. A
// brief race window exists between close and re-bind, but on loopback in a test
// it is reliable; the alternative (binding :0 in the tool and reporting back)
// would require changing the reference tool, which this integration test must
// NOT do.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// buildToolDB compiles cmd/lever-tool-db to a temp path and returns it. If the
// build is impossible in this environment the test is skipped (per the brief).
func buildToolDB(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "lever-tool-db")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/lever-tool-db")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build lever-tool-db in this environment (skipping integration): %v\n%s", err, out)
	}
	return bin
}

// repoRoot walks up from the package dir to the module root (where go.mod lives)
// so `go build ./cmd/lever-tool-db` resolves regardless of the test's cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repoRoot: go.mod not found above test dir")
		}
		dir = parent
	}
}

// workerClient builds an mTLS client that pins the broker CA and presents the
// worker's CA-issued cert, dialing 127.0.0.1 but verifying the server cert
// against the broker's serverName (host.orb.internal) — the OrbStack hostname the
// real server cert is issued for.
func workerClient(t *testing.T, caInst *ca.CA, cert tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(caInst.Cert)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		ServerName:   serverName, // "host.orb.internal"
		Certificates: []tls.Certificate{cert},
	}}}
}

// workerCert issues a CA-signed client cert for cn (the simulated-agent technique
// from the broker e2e: skip provision/enrol, mint the leaf directly from the CA).
func workerCert(t *testing.T, caInst *ca.CA, cn string) tls.Certificate {
	t.Helper()
	csrPEM, keyPEM := csrWithKey(t, cn)
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// mintWorkerToken mints a delegated db.read token bound to "worker" constrained to
// table=A + filter=alice, signed with the broker's capability private key at the
// given epoch, and base64url-encodes it for the _capability arg.
func mintWorkerToken(t *testing.T, kp token.KeyPair, epoch int) string {
	t.Helper()
	tok, err := token.Mint(kp.Private, token.Grant{
		Agent:      "worker",
		Capability: token.Capability{Tool: "db", Operation: "read"},
		Constraints: []token.Constraint{
			{Key: "table", Value: "A"},
			{Key: "filter", Value: "alice"},
		},
		Expiry: time.Now().Add(time.Hour),
		Epoch:  epoch,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(tok)
}

// mcpRead POSTs a tools/call read{table,filter,_capability} to /mcp/db/ on the
// jail base URL and returns the HTTP response.
func mcpRead(t *testing.T, client *http.Client, baseURL, tok string, args map[string]any) *http.Response {
	t.Helper()
	a := map[string]any{"_capability": tok}
	for k, v := range args {
		a[k] = v
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "read", "arguments": a},
	})
	resp, err := client.Post(baseURL+"/mcp/db/", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("mcp read: %v", err)
	}
	return resp
}

// TestBrokerctlHostIntegration proves the host-side scenario through config →
// brokerctl → a real lever-tool-db subprocess with a simulated worker, plus
// revocation persistence across a broker restart.
func TestBrokerctlHostIntegration(t *testing.T) {
	toolBin := buildToolDB(t) // skips if the env can't build it

	// ── Fixture config ────────────────────────────────────────────────────────
	// manager delegates db.read → worker; worker has no obtain (pure executor).
	// The db tool's backend is a fixed free loopback port the real tool binds.
	work := t.TempDir()
	treeDir := filepath.Join(work, "tree")
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	refDB := filepath.Join(work, "ref.db")
	backendAddr := freeLoopbackPort(t) // e.g. 127.0.0.1:54321 — tool's -backend + envelope backend

	cfgYAML := fmt.Sprintf(`name: integ
backend: orbstack
tree: tree
manager:
  delegate:
    - tool: db
      op: read
      to: [worker]
workers:
  - name: worker
    dir: w
broker:
  llm_auth: subscription
  jail_port: 0
  admin_port: 0
  tools:
    - name: db
      command: [%q, -dsn, %q]
      backend: %q
      operations:
        - name: read
          caveat_param:
            table: table
            filter: filter
`, toolBin, "file:"+refDB, backendAddr)

	cfgPath := filepath.Join(work, config.CanonicalName)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// ── Assemble the broker exactly like brokerctl.Serve, but on :0 listeners ──
	state := StateDir(work)
	kp, caInst, err := state.EnsureKeys()
	if err != nil {
		t.Fatalf("EnsureKeys: %v", err)
	}
	rev, err := state.LoadRevocation()
	if err != nil {
		t.Fatalf("LoadRevocation: %v", err)
	}
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	cfg.RevocationState = rev
	cfg.PersistRevocation = state.SaveRevocation
	b := broker.New(cfg)

	jailLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	jailURL := "https://" + jailLn.Addr().String()
	adminURL := "http://" + adminLn.Addr().String()

	certSrc, err := caInst.NewServerCertSource(serverName, []string{serverName}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// ── Supervise the REAL lever-tool-db subprocess ────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logf, err := os.OpenFile(filepath.Join(work, "tool.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	sup := NewSupervisor(app.Broker.Tools, adminURL, logf)
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("supervisor start: %v", err)
	}
	defer sup.Stop()

	// ── Run the broker on the pre-bound listeners ──────────────────────────────
	serveErr := make(chan error, 1)
	go func() { serveErr <- b.ServeListeners(ctx, jailLn, adminLn, certSrc) }()

	// Wait until the real tool's MCP backend is accepting connections. The "db"
	// gateway route already exists (BuildBroker pre-loads the config-authoritative
	// envelope, so JailHandler bound /mcp/db/ at serve time); we only need the
	// tool process up + listening before the gateway proxies to it. The tool's
	// /register (loopback admin) lands at roughly the same moment.
	waitBackend(t, backendAddr)

	// ── Simulated worker: CA-issued cert + directly-minted delegated token ─────
	wClient := workerClient(t, caInst, workerCert(t, caInst, "worker"))
	tok := mintWorkerToken(t, kp, 0)

	// ── (1) ALLOWED: read{table:A, filter:alice} → only alice's rows ───────────
	resp := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "A", "filter": "alice"})
	allowedText := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed read: status=%d want 200, body=%s", resp.StatusCode, allowedText)
	}
	if !strings.Contains(allowedText, "alice") {
		t.Fatalf("allowed read: must contain alice; got %s", allowedText)
	}
	if strings.Contains(allowedText, "bob") || strings.Contains(allowedText, "secret") || strings.Contains(allowedText, "carol") {
		t.Fatalf("SECURITY: allowed read leaked a forbidden row: %s", allowedText)
	}

	// ── (2) DENIED forbidden table C: read{table:C} → denied, no rows ──────────
	respC := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "C", "filter": "secret"})
	bodyC := readBody(t, respC)
	if respC.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: table=C returned 200 with data: %s", bodyC)
	}
	if strings.Contains(bodyC, "secret") {
		t.Fatalf("SECURITY: table=C leaked a row: %s", bodyC)
	}

	// ── (3) DENIED dropped filter: read{table:A} (no filter) → denied, no rows ─
	respNF := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "A"})
	bodyNF := readBody(t, respNF)
	if respNF.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: dropped-filter returned 200 with data: %s", bodyNF)
	}
	if strings.Contains(bodyNF, "alice") {
		t.Fatalf("SECURITY: dropped-filter leaked alice: %s", bodyNF)
	}

	// ── (4) /bootstrap single-shot latch: first → ticket, second → refused ─────
	first, err := http.Post(adminURL+"/bootstrap", "application/json", nil)
	if err != nil {
		t.Fatalf("bootstrap #1: %v", err)
	}
	firstBody := readBody(t, first)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap #1: status=%d want 200, body=%s", first.StatusCode, firstBody)
	}
	var bt struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal([]byte(firstBody), &bt); err != nil || bt.Ticket == "" {
		t.Fatalf("bootstrap #1: expected a ticket, got err=%v body=%s", err, firstBody)
	}
	second, err := http.Post(adminURL+"/bootstrap", "application/json", nil)
	if err != nil {
		t.Fatalf("bootstrap #2: %v", err)
	}
	secondBody := readBody(t, second)
	if second.StatusCode != http.StatusForbidden {
		t.Fatalf("bootstrap #2: status=%d want 403 (already bootstrapped), body=%s", second.StatusCode, secondBody)
	}

	// ── (5) REVOCATION: bump-epoch → the worker's next call is denied ──────────
	// Re-confirm the token works right now, then bump and re-exercise the SAME token.
	okAgain := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "A", "filter": "alice"})
	_ = readBody(t, okAgain)
	if okAgain.StatusCode != http.StatusOK {
		t.Fatalf("pre-bump sanity read: status=%d want 200", okAgain.StatusCode)
	}
	bumpResp, err := http.Post(adminURL+"/bump-epoch", "application/json", nil)
	if err != nil {
		t.Fatalf("bump-epoch: %v", err)
	}
	_ = readBody(t, bumpResp)
	if bumpResp.StatusCode != http.StatusOK {
		t.Fatalf("bump-epoch: status=%d want 200", bumpResp.StatusCode)
	}
	denied := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "A", "filter": "alice"})
	deniedBody := readBody(t, denied)
	if denied.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: epoch-revoked token still returned 200: %s", deniedBody)
	}
	if strings.Contains(deniedBody, "alice") {
		t.Fatalf("SECURITY: epoch-revoked call leaked alice: %s", deniedBody)
	}

	// ── (6) PERSISTENCE: a SECOND broker from the SAME state still denies ──────
	// bump-epoch persisted minEpoch=1 via SaveRevocation. Rebuild broker.New from
	// the same state dir and prove the same epoch-0 token is denied with NO fresh
	// bump — the floor survived the "restart".
	rev2, err := state.LoadRevocation()
	if err != nil {
		t.Fatalf("reload revocation: %v", err)
	}
	if rev2.MinEpoch < 1 {
		t.Fatalf("persistence: reloaded minEpoch=%d want >=1 (bump-epoch did not persist)", rev2.MinEpoch)
	}
	cfg2, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker #2: %v", err)
	}
	cfg2.RevocationState = rev2
	cfg2.PersistRevocation = state.SaveRevocation
	b2 := broker.New(cfg2)

	// Stand up the second broker on its own listeners. We do NOT start a second
	// tool: the persisted epoch floor denies the revoked token at the gateway's
	// token check, BEFORE any proxy to a backend — so no backend is needed to
	// prove the floor survived the restart. (The /mcp/db/ route still exists,
	// because BuildBroker pre-loaded the config-authoritative "db" envelope into
	// b2's registry.)
	jailLn2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	jailURL2 := "https://" + jailLn2.Addr().String()
	certSrc2, err := caInst.NewServerCertSource(serverName, []string{serverName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = b2.ServeListeners(ctx2, jailLn2, adminLn2, certSrc2) }()
	waitEpoch(t, "http://"+adminLn2.Addr().String()) // broker #2 up

	// The same epoch-0 token, against the freshly-restarted broker, is denied by
	// the persisted minEpoch floor — no bump-epoch was issued to b2.
	wClient2 := workerClient(t, caInst, workerCert(t, caInst, "worker"))
	persisted := mcpRead(t, wClient2, jailURL2, tok, map[string]any{"table": "A", "filter": "alice"})
	persistedBody := readBody(t, persisted)
	if persisted.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: persisted-floor broker accepted the revoked token: %s", persistedBody)
	}
	if persisted.StatusCode == http.StatusForbidden && b2.MinEpoch() < 1 {
		t.Fatalf("persistence: second broker minEpoch=%d want >=1", b2.MinEpoch())
	}
	if strings.Contains(persistedBody, "alice") {
		t.Fatalf("SECURITY: persisted-floor broker leaked alice: %s", persistedBody)
	}
}

// waitBackend blocks until addr accepts a TCP connection (the real lever-tool-db
// MCP backend is up), or fails after a generous timeout.
func waitBackend(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// Brief settle so /register (loopback admin) has also landed.
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for lever-tool-db backend %s to listen", addr)
}

// waitEpoch blocks until the broker's admin /epoch endpoint responds 200 (the
// admin server is up), or fails after a timeout.
func waitEpoch(t *testing.T, adminURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminURL + "/epoch")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for broker admin %s/epoch", adminURL)
}

// readBody fully reads + closes a response body and returns it as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}
