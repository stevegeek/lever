package broker

// e2e_captool_test.go exercises the WHOLE first-party capability chain over real
// mTLS: manager provisions a worker, the worker enrols (mTLS cert), the manager
// delegates a constrained `db.read` capability to the worker, and the worker
// calls the gated MCP gateway. The "db" backend is a REAL captool.Server that
// independently re-verifies the forwarded token (signature + bound_agent +
// capability + caveats + expiry + epoch) and enforces a hard backstop before its
// handler ever runs over an in-memory store.
//
// It reuses the broker e2e helpers (jailServer, agentClient, signedCert) and the
// gateway/broker test config (testConfig). The captool server is built inline so
// the test stays self-contained and fast (no SQLite dependency) while still
// exercising the captool VERIFY pipeline + backstop + the broker forward seam.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
	registry "github.com/stevegeek/lever/internal/broker/registry"
)

// dbRow is one row in the in-memory store the captool handler reads from.
type dbRow struct {
	Table string
	Name  string
}

// memStore is a tiny in-test data source: rows in tables A, B, C. The captool
// read handler returns only the rows matching the (table, filter) request. The
// point of the test is to prove the capability constraints gate which rows the
// agent can ever obtain — not to test a real database.
var memStore = []dbRow{
	{Table: "A", Name: "alice"},
	{Table: "A", Name: "bob"},
	{Table: "B", Name: "secret"},
	{Table: "C", Name: "carol"},
}

// readBackstop is the captool hard invariant: read-only over the {A,B} table
// allowlist. Table C is forbidden at the backstop regardless of any token, which
// is the defence-in-depth layer behind the capability constraint.
func readBackstop(vc captool.ValidatedContext, args map[string]string) error {
	if vc.Operation != "read" {
		return fmt.Errorf("backstop: only read is permitted")
	}
	switch args["table"] {
	case "A", "B":
		return nil
	default:
		return fmt.Errorf("backstop: table %q not in allowlist", args["table"])
	}
}

// readHandler returns only the rows whose Table and Name match the request args.
// A name (filter) is required; an empty/absent filter yields no rows.
func readHandler(_ captool.ValidatedContext, args map[string]string) (any, error) {
	table := args["table"]
	filter := args["filter"]
	rows := make([]string, 0, len(memStore))
	for _, r := range memStore {
		if r.Table == table && filter != "" && r.Name == filter {
			rows = append(rows, r.Name)
		}
	}
	return map[string]any{"rows": rows}, nil
}

// newCaptoolDB stands up a real captool.Server for the "db" tool, mounted on its
// own httptest server, registered with the broker over the loopback admin
// listener. It returns the captool httptest server (so the caller can defer
// Close) — the broker now has "db" registered as a first-party tool whose
// backend is that server. Both servers are registered with t.Cleanup.
func newCaptoolDB(t *testing.T, b *Broker) {
	t.Helper()

	// Loopback admin listener: captool.Register POSTs /register here and caches
	// the broker pubkey + epoch; /epoch serves freshness.
	adminTS := httptest.NewServer(b.AdminHandler())
	t.Cleanup(adminTS.Close)

	// Forward-declared pointer: the httptest handler derefs captoolSrv per-request,
	// AFTER it is assigned below. This lets Config.Backend equal the captool
	// server's own URL (which the broker proxies /mcp/db/ to) without a
	// chicken-and-egg between New() and NewServer().
	var captoolSrv *captool.Server
	captoolTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captoolSrv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(captoolTS.Close)

	var err error
	captoolSrv, err = captool.New(captool.Config{
		Name:     "db",
		Backend:  captoolTS.URL, // broker proxies /mcp/db/ here
		AdminURL: adminTS.URL,
		Operations: []captool.Operation{{
			Name:        "read",
			Description: "read rows from a table filtered by name",
			Params: []captool.ParamSpec{
				{Name: "table", Type: "string"},
				{Name: "filter", Type: "string"},
			},
			// Identity mapping: the "table"/"filter" token caveats bind the
			// "table"/"filter" request args.
			CaveatParam: map[string]string{"table": "table", "filter": "filter"},
			Backstop:    readBackstop,
			Handler:     readHandler,
		}},
	})
	if err != nil {
		t.Fatalf("captool.New: %v", err)
	}

	// Pre-load the config envelope with the REAL captool backend URL.
	// The config-authoritative handleRegister takes backend/allowed_values/
	// FirstParty from the pre-loaded config, so this must reflect the actual
	// test server address before captoolSrv.Register() POSTs /register.
	if err := b.reg.Register(registry.Tool{
		Name: "db", Backend: captoolTS.URL, FirstParty: true,
		Operations: map[string]registry.Operation{
			"read": {Name: "read"},
		},
	}); err != nil {
		t.Fatalf("preload db config envelope: %v", err)
	}

	// Register the "db" tool (backend=captoolTS.URL, first_party=true) and cache
	// the broker pubkey + epoch. MUST happen before jailServer()/JailHandler()
	// builds the gateway routes (registration-order footgun) — otherwise /mcp/db/ 404s.
	if err := captoolSrv.Register(context.Background()); err != nil {
		t.Fatalf("captool.Register: %v", err)
	}
}

// mcpCall POSTs a tools/call to /mcp/db/ as the given client and returns the
// HTTP response. The capability token is injected into the arguments under
// _capability; extra args are merged in.
func mcpCall(t *testing.T, client *http.Client, url, token string, args map[string]any) *http.Response {
	t.Helper()
	a := map[string]any{"_capability": token}
	for k, v := range args {
		a[k] = v
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "read", "arguments": a},
	})
	resp, err := client.Post(url+"/mcp/db/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	return resp
}

// TestE2ECaptoolFirstPartyDelegatedRead drives the full acceptance scenario.
func TestE2ECaptoolFirstPartyDelegatedRead(t *testing.T) {
	// ── Setup: broker + rules (manager may delegate db.read to worker; worker is
	// a pure executor with no obtain) ─────────────────────────────────────────
	cfg := testConfig(t) // AllowDelegate("manager","db","read","worker"); no worker obtain
	b := New(cfg)

	// Stand up the REAL captool "db" tool and register it first-party with the
	// broker BEFORE the jail handler binds its gateway routes.
	newCaptoolDB(t, b)

	srv := jailServer(t, b)
	defer srv.Close()

	managerCert := signedCert(t, b, "manager")
	managerClient := agentClient(t, b, managerCert)

	// ── Step 1: provision — manager → ticket ──────────────────────────────────
	provBody, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	provResp, err := managerClient.Post(srv.URL+"/provision", "application/json", bytes.NewReader(provBody))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer provResp.Body.Close()
	if provResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(provResp.Body)
		t.Fatalf("provision: status=%d body=%s", provResp.StatusCode, body)
	}
	var provResult ProvisionResponse
	if err := json.NewDecoder(provResp.Body).Decode(&provResult); err != nil {
		t.Fatalf("provision: decode: %v", err)
	}
	if provResult.Ticket == "" {
		t.Fatal("provision: empty ticket")
	}

	// ── Step 1b: enrol — worker self-generates a key, POSTs /enrol → cert ──────
	workerCSRPEM, workerKeyPEM := csrWithKey(t, "worker")
	enrolReqBody, _ := json.Marshal(EnrolRequest{Ticket: provResult.Ticket, CSR: string(workerCSRPEM)})
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
	var enrolResult EnrolResponse
	if err := json.NewDecoder(enrolResp.Body).Decode(&enrolResult); err != nil {
		t.Fatalf("enrol: decode: %v", err)
	}
	if enrolResult.Cert == "" {
		t.Fatal("enrol: empty cert")
	}
	workerTLSCert, err := tls.X509KeyPair([]byte(enrolResult.Cert), workerKeyPEM)
	if err != nil {
		t.Fatalf("enrol: parse tls.Certificate: %v", err)
	}
	workerClient := agentClient(t, b, workerTLSCert)

	// ── Step 2: request (delegation) — manager delegates a db.read bound to the
	// worker, constrained to table=A, filter=alice → capability token ─────────
	capReqBody, _ := json.Marshal(CapRequest{
		Tool: "db", Op: "read", BoundTo: "worker",
		Constraints: map[string]string{"table": "A", "filter": "alice"},
	})
	capResp, err := managerClient.Post(srv.URL+"/request", "application/json", bytes.NewReader(capReqBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer capResp.Body.Close()
	if capResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(capResp.Body)
		t.Fatalf("request: status=%d body=%s", capResp.StatusCode, body)
	}
	var capResult CapResponse
	if err := json.NewDecoder(capResp.Body).Decode(&capResult); err != nil {
		t.Fatalf("request: decode: %v", err)
	}
	if capResult.Token == "" {
		t.Fatal("request: empty token")
	}
	capToken := capResult.Token

	// ── Step 3 (ALLOWED): worker calls read{table:A, filter:alice} → broker
	// verifies, forwards token + X-Lever-Caller to captool, captool re-verifies +
	// backstop + handler returns ONLY alice's row. ────────────────────────────
	allowedResp := mcpCall(t, workerClient, srv.URL, capToken, map[string]any{"table": "A", "filter": "alice"})
	defer allowedResp.Body.Close()
	if allowedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(allowedResp.Body)
		t.Fatalf("allowed read: status=%d want 200, body=%s", allowedResp.StatusCode, body)
	}
	allowedText := mcpResultText(t, allowedResp)
	if !strings.Contains(allowedText, "alice") {
		t.Fatalf("allowed read: result must contain alice's row; got %s", allowedText)
	}
	if strings.Contains(allowedText, "bob") || strings.Contains(allowedText, "secret") {
		t.Fatalf("SECURITY: allowed read leaked bob/secret rows: %s", allowedText)
	}

	// ── Step 4 (DENIED: forbidden table) — same token (caveat table=A) used for
	// table=C. The capability constraint denies it (and the captool backstop
	// forbids C as defence-in-depth). Assert non-200 and NO rows reach the agent.
	deniedTableResp := mcpCall(t, workerClient, srv.URL, capToken, map[string]any{"table": "C", "filter": "carol"})
	defer deniedTableResp.Body.Close()
	if deniedTableResp.StatusCode == http.StatusOK {
		text := mcpResultText(t, deniedTableResp)
		t.Fatalf("SECURITY: table=C call returned 200 with data: %s", text)
	}
	assertNoRows(t, deniedTableResp, "carol")

	// ── Step 5 (DENIED: dropped/changed filter) — same token (caveat
	// filter=alice) with filter=bob. The canonical-projected filter no longer
	// matches the token caveat → denied. Assert non-200 and NO rows. ──────────
	deniedFilterResp := mcpCall(t, workerClient, srv.URL, capToken, map[string]any{"table": "A", "filter": "bob"})
	defer deniedFilterResp.Body.Close()
	if deniedFilterResp.StatusCode == http.StatusOK {
		text := mcpResultText(t, deniedFilterResp)
		t.Fatalf("SECURITY: changed-filter call returned 200 with data: %s", text)
	}
	assertNoRows(t, deniedFilterResp, "bob")

	// ── Step 5b (DENIED: omitted filter) — token requires filter=alice; omitting
	// filter leaves the caveat unsatisfied → denied. ──────────────────────────
	droppedFilterResp := mcpCall(t, workerClient, srv.URL, capToken, map[string]any{"table": "A"})
	defer droppedFilterResp.Body.Close()
	if droppedFilterResp.StatusCode == http.StatusOK {
		text := mcpResultText(t, droppedFilterResp)
		t.Fatalf("SECURITY: dropped-filter call returned 200 with data: %s", text)
	}
	assertNoRows(t, droppedFilterResp, "alice")
}

// mcpResultText reads the MCP JSON-RPC response and returns the text content of
// the result (the captool handler marshals its row payload into a text block).
func mcpResultText(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("decode mcp result: %v (body=%s)", err, body)
	}
	var sb strings.Builder
	for _, c := range rpc.Result.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// assertNoRows fails if the response carried any result content mentioning the
// named row — a denied call must never deliver data to the agent.
func assertNoRows(t *testing.T, resp *http.Response, name string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), name) {
		t.Fatalf("SECURITY: denied call leaked row %q to the agent: %s", name, body)
	}
}
