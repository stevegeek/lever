package captool

import (
	"net/http"
	"strings"
	"testing"
)

// The broker presents a per-boot shared secret on every request it proxies to
// a first-party tool (X-Lever-Tool-Secret). A request without it — an agent
// that reached the tool's loopback port directly, past the broker — is refused
// before any MCP dispatch, so X-Lever-Caller is never trusted on its own.

func TestServeHTTPDeniesMissingToolSecret(t *testing.T) {
	w := rpcNoSecret(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without the tool secret", w.Code)
	}
	if strings.Contains(w.Body.String(), "serverInfo") {
		t.Fatal("a secretless request must not be dispatched")
	}
}

func TestServeHTTPDeniesWrongToolSecret(t *testing.T) {
	w := rpcNoSecret(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		map[string]string{ToolSecretHeader: testToolSecret + "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 with a wrong tool secret", w.Code)
	}
}

func TestServeHTTPAcceptsCorrectToolSecret(t *testing.T) {
	w := rpc(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "serverInfo") {
		t.Fatalf("status = %d body = %s; want a dispatched initialize", w.Code, w.Body.String())
	}
}

// With no secret configured at all (Config.Secret empty and LEVER_TOOL_SECRET
// unset) the server fails closed: nothing is trusted, so every request is 401.
func TestServeHTTPDeniesEverythingWhenNoSecretConfigured(t *testing.T) {
	t.Setenv(ToolSecretEnv, "")
	s := newTestServerWithSecret(t, "")
	w := rpcNoSecret(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no secret is configured", w.Code)
	}
	// An empty header must not match an empty secret.
	w = rpcNoSecret(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		map[string]string{ToolSecretHeader: ""})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for empty-vs-empty", w.Code)
	}
}

// The supervisor hands the secret to the tool through the environment
// (LEVER_TOOL_SECRET); New reads it when Config.Secret is empty.
func TestNewReadsToolSecretFromEnv(t *testing.T) {
	t.Setenv(ToolSecretEnv, "from-env")
	s := newTestServerWithSecret(t, "")
	w := rpcNoSecret(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		map[string]string{ToolSecretHeader: "from-env"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the env-provided secret", w.Code)
	}
}

// An explicit Config.Secret wins over the environment.
func TestNewPrefersConfigSecretOverEnv(t *testing.T) {
	t.Setenv(ToolSecretEnv, "from-env")
	s := newTestServerWithSecret(t, "from-config")
	w := rpcNoSecret(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		map[string]string{ToolSecretHeader: "from-env"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: the env secret must not match when Config.Secret is set", w.Code)
	}
}
