package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/brokertest"
	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/httpjson"
)

// allowDelegate adds a delegation rule to the broker's policy: agent may delegate
// (tool, op) to recipient.
func allowDelegate(t *testing.T, env *brokertest.Env, agent, tool, op, recipient string) {
	t.Helper()
	env.Rules.AllowDelegate(agent, tool, op, recipient)
}

// regDB registers the "db" tool with a "read" operation in the broker's registry.
func regDB(t *testing.T, env *brokertest.Env) {
	t.Helper()
	if err := env.Registry.Register(registry.Tool{
		Name:    "db",
		Backend: "http://127.0.0.1:3201",
		Operations: map[string]registry.Operation{
			"read": {Name: "read"},
		},
	}); err != nil {
		t.Fatalf("regDB: %v", err)
	}
}

// enrolManager signs a manager CSR directly with the CA (bypassing /provision
// since the manager identity needs a cert to call /provision in the first place),
// then builds an Identity from the signed cert, key, and CA PEM.
func enrolManager(t *testing.T, caInst *ca.CA) Identity {
	t.Helper()
	csrPEM, keyPEM, err := GenerateCSR("manager")
	if err != nil {
		t.Fatalf("enrolManager: generate CSR: %v", err)
	}
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatalf("enrolManager: sign CSR: %v", err)
	}
	return Identity{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caInst.CertPEM()}
}

// decodeB64 decodes a base64url (no-padding) string; fatals on error.
func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decodeB64: %v", err)
	}
	return b
}

// TestBuildToolCallBody verifies that the JSON-RPC body produced for the gateway
// satisfies the contract expected by internal/mcp.ToolsCall:
//   - jsonrpc == "2.0", method == "tools/call"
//   - params.name == op
//   - params.arguments._capability == token
//   - extra kv args appear in params.arguments
func TestBuildToolCallBody(t *testing.T) {
	const op = "query"
	const tok = "tok_abc123"
	extra := map[string]string{"table": "users", "limit": "10"}

	body := buildToolCallBody(op, tok, extra)

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := msg["jsonrpc"]; got != "2.0" {
		t.Errorf("jsonrpc: got %v, want 2.0", got)
	}
	if got := msg["method"]; got != "tools/call" {
		t.Errorf("method: got %v, want tools/call", got)
	}

	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatal("params missing or wrong type")
	}
	if got := params["name"]; got != op {
		t.Errorf("params.name: got %v, want %q", got, op)
	}

	args, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("params.arguments missing or wrong type")
	}
	if got := args["_capability"]; got != tok {
		t.Errorf("arguments._capability: got %v, want %q", got, tok)
	}
	if got := args["table"]; got != "users" {
		t.Errorf("arguments.table: got %v, want users", got)
	}
	if got := args["limit"]; got != "10" {
		t.Errorf("arguments.limit: got %v, want 10", got)
	}
}

// TestBuildToolCallBodyEmptyArgs verifies token-only calls (no extra kv pairs).
func TestBuildToolCallBodyEmptyArgs(t *testing.T) {
	body := buildToolCallBody("op", "mytoken", nil)
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	params := msg["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if got := args["_capability"]; got != "mytoken" {
		t.Errorf("arguments._capability: got %v, want mytoken", got)
	}
	// Only _capability should be present
	if len(args) != 1 {
		t.Errorf("expected 1 argument (only _capability), got %d: %v", len(args), args)
	}
}

// TestCallWireContract pins agent.Call's HTTP contract against an httptest stub:
// it POSTs to /mcp/<tool>/ with Content-Type application/json and the token in
// arguments._capability, and — critically, unlike Request — returns the raw
// response body EVEN on a non-200 (the caller prints it before surfacing the
// error), with a non-200 mapped to a call:-wrapped *httpjson.StatusError.
func TestCallWireContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, `{"result":"ok"}`},
		{"denied", http.StatusForbidden, `{"error":"denied"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotCT string
			var gotBody bytes.Buffer
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotCT = r.Header.Get("Content-Type")
				gotBody.ReadFrom(r.Body)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer stub.Close()

			body, err := Call(context.Background(), stub.URL, stub.Client(),
				"db", "read", "tok_xyz", map[string]string{"table": "users"})

			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", gotMethod)
			}
			if gotPath != "/mcp/db/" {
				t.Errorf("path = %q, want /mcp/db/", gotPath)
			}
			if gotCT != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotCT)
			}
			if !strings.Contains(gotBody.String(), `"_capability":"tok_xyz"`) {
				t.Errorf("request body missing _capability token: %s", gotBody.String())
			}
			if !strings.Contains(gotBody.String(), `"table":"users"`) {
				t.Errorf("request body missing constraint: %s", gotBody.String())
			}
			// Body returned regardless of status; the status rides on the error.
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
			if tc.status == http.StatusOK {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if httpjson.Status(err) != tc.status {
				t.Errorf("error = %v, want a *httpjson.StatusError with code %d", err, tc.status)
			}
			if err == nil || !strings.HasPrefix(err.Error(), "call: ") {
				t.Errorf("error = %v, want a call:-prefixed wrap", err)
			}
		})
	}
}

// TestCallTransportError verifies a dial failure yields an empty body, no
// status, and a `call:`-wrapped error (no body was ever received to print).
func TestCallTransportError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := stub.URL
	stub.Close() // nothing is listening now → dial fails

	body, err := Call(context.Background(), url, http.DefaultClient, "db", "read", "tok", nil)
	if err == nil {
		t.Fatal("dial failure must error")
	}
	if !strings.HasPrefix(err.Error(), "call: ") {
		t.Errorf("error = %q, want a call:-prefixed wrap", err)
	}
	if body != "" || httpjson.Status(err) != 0 {
		t.Errorf("transport failure must yield empty body and no status, got %q/%d", body, httpjson.Status(err))
	}
}

func TestRequestMintsDelegatedToken(t *testing.T) {
	env := testBroker(t)
	// Allow manager to delegate db.read to worker; register the db tool envelope.
	allowDelegate(t, env, "manager", "db", "read", "worker")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, err := managerID.Client()
	if err != nil {
		t.Fatal(err)
	}
	tokB64, err := Request(context.Background(), env.Server.URL, client, "db", "read", "worker", map[string]string{"table": "A"})
	if err != nil {
		t.Fatal(err)
	}
	// The minted token must verify as bound to worker for db.read with table=A.
	raw := decodeB64(t, tokB64)
	if err := token.Verify(env.Keys.Public, raw, token.Request{
		Caller: "worker", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{"table": "A"}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("minted token must verify for worker/db.read/table=A: %v", err)
	}
}
