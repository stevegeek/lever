package brokerctl

// external_integration_test.go drives the EXTERNAL-tool path end to end:
//
//	config.Load(lever.yaml with two external tools — fine "devonthink" and
//	coarse "things3") → BuildBroker (registers them from config,
//	FirstParty=false) → broker.New → ServeListeners → agents (CA-issued client
//	certs) mint via /request and call through the gated /mcp/<name>/ gateway
//	over mTLS to plain httptest MCP servers, which the broker did NOT spawn.
//
// Scenarios (spec §7): coarse token → any MCP call allowed + token stripped;
// fine token → declared op allowed; /request denies an agent with no grant;
// the wildcard cannot be minted for — nor satisfy — a fine tool.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/broker"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
	"github.com/lever-to/lever/internal/config"
)

// fakeMCP is a plain (non-captool) MCP server: it records the last body it
// received and answers OK. It stands in for a user-session host server the
// broker fronts but does not spawn.
func fakeMCP(t *testing.T, lastBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*lastBody = string(b)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
}

// requestToken POSTs /request as the given client and returns (statusCode, token).
func requestToken(t *testing.T, client *http.Client, jailURL, tool, op string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"tool": tool, "op": op})
	resp, err := client.Post(jailURL+"/request", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("/request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	var cr struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode /request: %v", err)
	}
	return resp.StatusCode, cr.Token
}

// mcpCall POSTs a tools/call for mcpTool with the given capability to
// /mcp/<gatewayTool>/ and returns the HTTP status.
func mcpCall(t *testing.T, client *http.Client, jailURL, gatewayTool, mcpTool, tok string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": mcpTool, "arguments": map[string]any{"q": "x", "_capability": tok}},
	})
	resp, err := client.Post(jailURL+"/mcp/"+gatewayTool+"/", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestExternalToolsIntegration(t *testing.T) {
	var dtBody, thBody string
	dtSrv := fakeMCP(t, &dtBody) // fine: devonthink
	defer dtSrv.Close()
	thSrv := fakeMCP(t, &thBody) // coarse: things3
	defer thSrv.Close()
	dtBackend := strings.TrimPrefix(dtSrv.URL, "http://")
	thBackend := strings.TrimPrefix(thSrv.URL, "http://")

	// manager: coarse things3; worker: fine devonthink/search ONLY.
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := fmt.Sprintf(`name: extinteg
backend: orbstack
tree: tree
manager:
  obtain:
    - {tool: things3, op: "*"}
groves:
  - name: worker
    dir: w
    obtain:
      - {tool: devonthink, op: search}
broker:
  llm_auth: subscription
  jail_port: 0
  admin_port: 0
  tools:
    - name: devonthink
      external: true
      backend: %q
      operations:
        - {name: search}
    - name: things3
      external: true
      backend: %q
      gate: coarse
`, dtBackend, thBackend)
	cfgPath := filepath.Join(work, config.CanonicalName)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	state := StateDir(work)
	kp, caInst, err := state.EnsureKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
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
	certPEM, keyPEM, err := caInst.IssueServerCert(serverName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- b.ServeListeners(ctx, jailLn, adminLn, certPEM, keyPEM) }()

	manager := workerClient(t, caInst, workerCert(t, caInst, "manager"))
	worker := workerClient(t, caInst, workerCert(t, caInst, "worker"))

	// 1. Coarse: manager mints {things3,"*"} and calls an ARBITRARY MCP tool.
	code, thTok := requestToken(t, manager, jailURL, "things3", "*")
	if code != http.StatusOK || thTok == "" {
		t.Fatalf("manager mint things3/*: status %d", code)
	}
	if got := mcpCall(t, manager, jailURL, "things3", "add-todo", thTok); got != http.StatusOK {
		t.Fatalf("coarse call = %d, want 200", got)
	}
	if thBody == "" || strings.Contains(thBody, "_capability") {
		t.Fatalf("upstream must be reached WITHOUT the token; got %q", thBody)
	}

	// 2. Fine: worker mints {devonthink,search} and calls it.
	code, dtTok := requestToken(t, worker, jailURL, "devonthink", "search")
	if code != http.StatusOK || dtTok == "" {
		t.Fatalf("worker mint devonthink/search: status %d", code)
	}
	if got := mcpCall(t, worker, jailURL, "devonthink", "search", dtTok); got != http.StatusOK {
		t.Fatalf("fine call = %d, want 200", got)
	}
	if dtBody == "" || strings.Contains(dtBody, "_capability") {
		t.Fatalf("upstream must be reached WITHOUT the token; got %q", dtBody)
	}

	// 3. Absent grant: worker has NO grant on things3 — mint is denied.
	if code, _ := requestToken(t, worker, jailURL, "things3", "*"); code != http.StatusForbidden {
		t.Fatalf("worker mint things3/* = %d, want 403 (no grant = no access)", code)
	}

	// 4a. Wildcard mint against a fine tool is denied (no grant AND no "*" op).
	if code, _ := requestToken(t, manager, jailURL, "devonthink", "*"); code != http.StatusForbidden {
		t.Fatalf("manager mint devonthink/* = %d, want 403 (wildcard only on coarse)", code)
	}
	// 4b. Even a broker-key-signed wildcard token cannot satisfy a fine tool.
	forged, err := token.Mint(kp.Private, token.Grant{
		Agent: "worker", Capability: token.Capability{Tool: "devonthink", Operation: "*"},
		Expiry: time.Now().Add(time.Hour), Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	dtBody = ""
	if got := mcpCall(t, worker, jailURL, "devonthink", "search",
		base64.RawURLEncoding.EncodeToString(forged)); got != http.StatusForbidden {
		t.Fatalf("wildcard token on fine tool = %d, want 403", got)
	}
	if dtBody != "" {
		t.Fatal("SECURITY: fine upstream reached on a wildcard token")
	}

	// 5. Discovery: both external tools appear in /tools (what lever-agent
	// boot walks to `claude mcp add` each gateway route).
	resp, err := worker.Get(jailURL + "/tools")
	if err != nil {
		t.Fatalf("/tools: %v", err)
	}
	var tl struct {
		Tools []string `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tl); err != nil {
		t.Fatalf("decode /tools: %v", err)
	}
	resp.Body.Close()
	listed := map[string]bool{}
	for _, n := range tl.Tools {
		listed[n] = true
	}
	if !listed["devonthink"] || !listed["things3"] {
		t.Fatalf("/tools = %v, want devonthink + things3 listed", tl.Tools)
	}

	cancel()
	if err := <-serveErr; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
