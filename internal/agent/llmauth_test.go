package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/httpjson"
)

// TestHarnessEnvOverlayPinsItsKeys pins the exact key set. Boot and the renew
// sidecar both call this builder and MUST write identical keys — a key added at
// one call site instead of here would be written at first enrol and then
// dropped by the next cert rotation (the settings writer merges, so the loss is
// silent). Update this test deliberately when the contract changes.
func TestHarnessEnvOverlayPinsItsKeys(t *testing.T) {
	got := HarnessEnvOverlay()
	want := map[string]string{
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1",
	}
	if len(got) != len(want) {
		t.Fatalf("key set changed: got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestHarnessEnvOverlayForcesClassicRenderer states WHY the renderer flag is in
// the overlay at all, so a later reader does not drop it as unrelated to
// identity. Every route to a lever agent is a PTY onto the container's tmux
// (`lever attach`, or scion's web terminal over `lever remote`), and Claude
// Code's default fullscreen renderer draws on the alternate screen, which has
// no scrollback. The operator cannot set this from inside the session either:
// `/tui default` needs a relaunch, and Claude Code refuses to relaunch a
// session started with a --system-prompt replacement — which is exactly what
// scion's harness passes.
func TestHarnessEnvOverlayForcesClassicRenderer(t *testing.T) {
	if got := HarnessEnvOverlay()["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"]; got != "1" {
		t.Fatalf("CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN = %q, want \"1\" — a lever agent is only ever reached through a PTY, where fullscreen rendering has no scrollback", got)
	}
}

// The RequestLLMToken tests pin its stable contract: verbatim token on success,
// a surfaced-body StatusError on non-200, trailing-slash tolerance, and the
// empty-token guard (which Request does NOT provide, so the wrapper must).

// llmServer answers POST /request with status and body and checks the request
// shape; any other path is a test failure.
func llmServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/request" {
			t.Errorf("got %s %s, want POST /request", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRequestLLMTokenReturnsTokenVerbatim(t *testing.T) {
	// The broker returns an already-base64url token; RequestLLMToken must return
	// it byte-for-byte (no re-encoding), so the proxy's bearerToken can decode it.
	tok := "abc123_base64url-verbatim"
	body, _ := json.Marshal(map[string]string{"token": tok})
	url := llmServer(t, http.StatusOK, string(body))
	got, err := RequestLLMToken(context.Background(), url, http.DefaultClient, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Fatalf("token = %q, want %q (must be verbatim)", got, tok)
	}
}

func TestRequestLLMTokenNormalizesTrailingSlash(t *testing.T) {
	// A brokerURL with a trailing slash must still hit /request (not //request).
	body, _ := json.Marshal(map[string]string{"token": "t"})
	url := llmServer(t, http.StatusOK, string(body))
	if _, err := RequestLLMToken(context.Background(), url+"/", http.DefaultClient, "worker"); err != nil {
		t.Fatalf("trailing-slash brokerURL must normalize to /request: %v", err)
	}
}

func TestRequestLLMTokenNon200SurfacesBody(t *testing.T) {
	url := llmServer(t, http.StatusForbidden, "policy: may not obtain (tool=llm op=generate)")
	_, err := RequestLLMToken(context.Background(), url, http.DefaultClient, "worker")
	if err == nil {
		t.Fatal("non-200 must error")
	}
	if !strings.Contains(err.Error(), "policy: may not obtain (tool=llm op=generate)") {
		t.Fatalf("error must surface the response body, got: %v", err)
	}
	if httpjson.Status(err) != http.StatusForbidden {
		t.Fatalf("error must carry the status, got: %v", err)
	}
}

func TestRequestLLMTokenEmptyTokenErrors(t *testing.T) {
	// A 200 with an empty token field must fail closed — a blank ANTHROPIC_AUTH_TOKEN
	// would silently break LLM auth.
	body, _ := json.Marshal(map[string]string{"token": ""})
	url := llmServer(t, http.StatusOK, string(body))
	_, err := RequestLLMToken(context.Background(), url, http.DefaultClient, "worker")
	if err == nil {
		t.Fatal("empty token must error")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("want an empty-token error, got: %v", err)
	}
}
