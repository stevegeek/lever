package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/httpjson"
)

// TestHarnessEnvOverlayPinsItsKeys pins the exact key set Boot and the renew
// sidecar both write. Update it deliberately when the contract changes.
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

// TestHarnessEnvOverlayForcesClassicRenderer: see HarnessEnvOverlay for why
// the flag must be in the overlay.
func TestHarnessEnvOverlayForcesClassicRenderer(t *testing.T) {
	if got := HarnessEnvOverlay()["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"]; got != "1" {
		t.Fatalf("CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN = %q, want \"1\" — a lever agent is only ever reached through a PTY, where fullscreen rendering has no scrollback", got)
	}
}

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
	// The token must come back byte-for-byte, not re-encoded.
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
	// A 200 with an empty token must fail closed.
	body, _ := json.Marshal(map[string]string{"token": ""})
	url := llmServer(t, http.StatusOK, string(body))
	_, err := RequestLLMToken(context.Background(), url, http.DefaultClient, "worker")
	if err == nil {
		t.Fatal("empty token must error")
	}
	if !errors.Is(err, errEmptyLLMToken) {
		t.Fatalf("want errEmptyLLMToken, got: %v", err)
	}
}
