package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// requestLLMToken has zero tests today; the D1 refactor rewrites it as a thin
// wrapper over agent.Request. These pin the STABLE contract that must survive the
// rewrite: verbatim token on success, a surfaced-body error on non-200, and the
// empty-token guard (which agent.Request does NOT provide, so the wrapper must
// keep it). The exact non-200 message prefix is deliberately NOT pinned — the
// plan sanctions it changing to agent.Request's style (it is log-only, re-wrapped
// at renew.go).

// llmServer returns a plain-HTTP server that answers POST /request with the given
// status and raw body; it also verifies the request path/verb/Content-Type.
func llmServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/request" {
			t.Errorf("path = %s, want /request", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequestLLMTokenReturnsTokenVerbatim(t *testing.T) {
	// The broker returns an already-base64url token; requestLLMToken must return it
	// byte-for-byte (no re-encoding), so the proxy's bearerToken can decode it.
	tok := "abc123_base64url-verbatim"
	body, _ := json.Marshal(map[string]string{"token": tok})
	srv := llmServer(t, http.StatusOK, string(body))

	got, err := requestLLMToken(context.Background(), srv.URL, srv.Client(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Fatalf("token = %q, want %q (must be verbatim)", got, tok)
	}
}

func TestRequestLLMTokenNormalizesTrailingSlash(t *testing.T) {
	// The empty-token/TrimRight guards must survive the rewrite: a brokerURL with a
	// trailing slash must still hit /request (not //request).
	body, _ := json.Marshal(map[string]string{"token": "t"})
	srv := llmServer(t, http.StatusOK, string(body))

	if _, err := requestLLMToken(context.Background(), srv.URL+"/", srv.Client(), "worker"); err != nil {
		t.Fatalf("trailing-slash brokerURL must normalize to /request: %v", err)
	}
}

func TestRequestLLMTokenNon200SurfacesBody(t *testing.T) {
	srv := llmServer(t, http.StatusForbidden, "policy: may not obtain (tool=llm op=generate)")
	_, err := requestLLMToken(context.Background(), srv.URL, srv.Client(), "worker")
	if err == nil {
		t.Fatal("non-200 must error")
	}
	if !strings.Contains(err.Error(), "policy: may not obtain (tool=llm op=generate)") {
		t.Fatalf("error must surface the response body, got: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error must surface the status, got: %v", err)
	}
}

func TestRequestLLMTokenEmptyTokenErrors(t *testing.T) {
	// A 200 with an empty token field must fail closed — a blank ANTHROPIC_AUTH_TOKEN
	// would silently break LLM auth. agent.Request does not guard this; the wrapper must.
	body, _ := json.Marshal(map[string]string{"token": ""})
	srv := llmServer(t, http.StatusOK, string(body))

	_, err := requestLLMToken(context.Background(), srv.URL, srv.Client(), "worker")
	if err == nil {
		t.Fatal("empty token must error")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("want an empty-token error, got: %v", err)
	}
}
