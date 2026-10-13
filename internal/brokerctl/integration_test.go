//go:build integration

package brokerctl

// The host-side acceptance test (build tag integration: it builds the real
// cmd/lever-tool-db). It drives the whole host-side capability stack:
// config.Load, EnsureKeys, BuildBroker, broker.New, ServeListeners on real
// loopback listeners, a supervised lever-tool-db subprocess, and a simulated
// worker (CA-issued cert, delegated token) calling /mcp/db/ over mTLS.
//
// It asserts: a table=A/filter=alice-constrained db.read returns only
// alice's rows; table=C and a dropped filter are denied with no rows;
// /bootstrap is a single-shot latch; and revocation denies the next call and
// survives a broker restart from the same state.

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/stevegeek/lever/internal/state"
)

// freeLoopbackPort grabs a loopback port and releases it for the real tool to
// bind. The close/re-bind race is acceptable in a test; the alternative
// would change the reference tool.
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

// buildToolDB skips the test when the binary cannot be built here.
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

// mintWorkerToken mints a delegated db.read token bound to "worker",
// constrained to table=A and filter=alice, at epoch.
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

func TestBrokerctlHostIntegration(t *testing.T) {
	toolBin := buildToolDB(t) // skips if the env can't build it

	// The manager delegates db.read to the worker; the worker obtains
	// nothing.
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

	// Assembled like brokerctl.Serve, but on :0 listeners.
	st := state.ForConfig(work)
	kp, caInst, err := EnsureKeys(st)
	if err != nil {
		t.Fatalf("EnsureKeys: %v", err)
	}
	rev, err := LoadRevocation(st)
	if err != nil {
		t.Fatalf("LoadRevocation: %v", err)
	}
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	cfg.Persistence.Revocation = rev
	cfg.Persistence.PersistRevocation = func(rs broker.RevocationState) error { return SaveRevocation(st, rs) }
	toolSecret, err := NewToolSecret()
	if err != nil {
		t.Fatalf("NewToolSecret: %v", err)
	}
	cfg.ToolSecret = toolSecret
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	specs, err := ToolSpecs(app)
	if err != nil {
		t.Fatal(err)
	}
	sup := NewSupervisor(specs, adminURL, st.ToolLogDir(), toolSecret)
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("supervisor start: %v", err)
	}
	defer sup.Stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- b.ServeListeners(ctx, jailLn, adminLn, nil, nil, certSrc) }()

	// The /mcp/db/ route exists from serve time (BuildBroker pre-loads the
	// envelope); only the tool process must be listening.
	waitBackend(t, backendAddr)

	wClient := workerClient(t, caInst, workerCert(t, caInst, "worker"))
	tok := mintWorkerToken(t, kp, 0)

	// (1) Allowed: only alice's rows.
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

	// (2) Forbidden table C: denied, no rows.
	respC := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "C", "filter": "secret"})
	bodyC := readBody(t, respC)
	if respC.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: table=C returned 200 with data: %s", bodyC)
	}
	if strings.Contains(bodyC, "secret") {
		t.Fatalf("SECURITY: table=C leaked a row: %s", bodyC)
	}

	// (3) Dropped filter: denied, no rows.
	respNF := mcpRead(t, wClient, jailURL, tok, map[string]any{"table": "A"})
	bodyNF := readBody(t, respNF)
	if respNF.StatusCode == http.StatusOK {
		t.Fatalf("SECURITY: dropped-filter returned 200 with data: %s", bodyNF)
	}
	if strings.Contains(bodyNF, "alice") {
		t.Fatalf("SECURITY: dropped-filter leaked alice: %s", bodyNF)
	}

	// (4) /bootstrap is single-shot.
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

	// (5) Revocation: the same token works, then is denied after bump-epoch.
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

	// (6) Persistence: a second broker from the same state denies the
	// epoch-0 token with no fresh bump.
	rev2, err := LoadRevocation(st)
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
	cfg2.Persistence.Revocation = rev2
	cfg2.Persistence.PersistRevocation = func(rs broker.RevocationState) error { return SaveRevocation(st, rs) }
	b2 := broker.New(cfg2)

	// No second tool: the epoch floor denies at the gateway's token check,
	// before any proxy to a backend.
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
	go func() { _ = b2.ServeListeners(ctx2, jailLn2, adminLn2, nil, nil, certSrc2) }()
	waitEpoch(t, "http://"+adminLn2.Addr().String()) // broker #2 up

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

func waitBackend(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// Brief settle so /register has also landed.
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for lever-tool-db backend %s to listen", addr)
}

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

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}
