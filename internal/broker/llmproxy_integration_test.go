package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLLMProxyEndToEnd is the end-to-end test for the LLM proxy.
// It wires a real Broker (with registered reserved "llm" tool, apiKey, and a
// fake Anthropic httptest upstream) and proves all five security properties in
// one consolidated integration test.
func TestLLMProxyEndToEnd(t *testing.T) {
	const realKey = "sk-REAL-KEY-doneGate-9x7z"

	// ── Assertion 1: Inject + Strip ──────────────────────────────────────────
	// The real key arrives at the fake upstream (x-api-key == configured key)
	// AND the inbound capability token does NOT (no Authorization/x-api-key
	// carrying the biscuit upstream).
	t.Run("InjectAndStrip", func(t *testing.T) {
		var gotKey, gotAuth string
		up := fakeAnthropic(t, &gotKey, &gotAuth)
		defer up.Close()

		b, caller := newTestBrokerForLLM(t, []byte(realKey), up.URL)
		tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

		rec := httptest.NewRecorder()
		req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		b.JailHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		// Real key must arrive at upstream.
		if gotKey != realKey {
			t.Errorf("upstream x-api-key = %q, want %q", gotKey, realKey)
		}
		// Capability token must NOT arrive upstream as Authorization.
		if gotAuth != "" {
			t.Errorf("capability token leaked upstream as Authorization=%q", gotAuth)
		}
		// gotKey already asserts only the real key reached upstream (not the biscuit
		// token), so a non-equal gotKey would also catch token-as-key forwarding.
	})

	// ── Assertion 2: Key never leaks back — incl. error path ────────────────
	// Point the fake upstream at a 401 handler that echoes the injected key
	// back as a response header and sets WWW-Authenticate. Assert the proxy
	// stripped both (R5) AND the real key appears in NONE of the bytes
	// (headers + body) returned to the jail.
	t.Run("KeyNeverLeaksOnError", func(t *testing.T) {
		const errorKey = "sk-REAL-KEY-errorpath-8z2q"
		var upCalls atomic.Int32

		leakyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upCalls.Add(1)
			// Simulate a leaky upstream: echo the injected x-api-key back in
			// response headers, plus a WWW-Authenticate challenge.
			if k := r.Header.Get("x-api-key"); k != "" {
				w.Header().Set("x-api-key", k) // R5: proxy must strip this
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="anthropic", charset="UTF-8"`)
			w.WriteHeader(http.StatusUnauthorized)
			// Response body intentionally does NOT embed the key, so the
			// combined-bytes check is a genuine guard on header stripping.
			io.WriteString(w, `{"error":"unauthorized","type":"authentication_error"}`)
		}))
		defer leakyUp.Close()

		b, caller := newTestBrokerForLLM(t, []byte(errorKey), leakyUp.URL)
		tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

		rec := httptest.NewRecorder()
		req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		b.JailHandler().ServeHTTP(rec, req)

		// Upstream must have been reached (a valid request should get through).
		if upCalls.Load() == 0 {
			t.Fatal("upstream was not called; the valid request should reach it")
		}

		// Collect ALL bytes the jail sees: every header value + body.
		result := rec.Result()
		var combined strings.Builder
		for name, vals := range result.Header {
			combined.WriteString(name)
			combined.WriteString(": ")
			combined.WriteString(strings.Join(vals, ", "))
			combined.WriteString("\n")
		}
		combined.WriteString(rec.Body.String())
		all := combined.String()

		if strings.Contains(all, errorKey) {
			t.Errorf("real API key leaked back to jail in response bytes:\n%s", all)
		}
		// Belt-and-suspenders: name the specific headers that must be gone.
		if result.Header.Get("x-api-key") != "" {
			t.Errorf("x-api-key response header not stripped (R5): %q", result.Header.Get("x-api-key"))
		}
		if result.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("WWW-Authenticate not stripped (R5): %q", result.Header.Get("WWW-Authenticate"))
		}
	})

	// ── Assertion 3: Revoked denied, no upstream call ────────────────────────
	// After Revoke(caller), the call must be denied AND the upstream counter
	// must remain zero — a regression that calls upstream before checking
	// revocation would increment the counter and be caught.
	t.Run("RevokedDeniedNoUpstreamCall", func(t *testing.T) {
		var upCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()

		b, caller := newTestBrokerForLLM(t, []byte(realKey), up.URL)
		tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
		b.Revoke(caller)

		rec := httptest.NewRecorder()
		req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		b.JailHandler().ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Fatal("revoked agent got 200; want denied (403/401)")
		}
		if n := upCalls.Load(); n != 0 {
			t.Fatalf("upstream called %d time(s) for revoked agent; want 0", n)
		}
	})

	// ── Assertion 4: Epoch-bump denied, no upstream call ─────────────────────
	// After BumpEpoch(), a token minted at the old epoch must be denied AND
	// the upstream must not be called.
	t.Run("EpochBumpDeniedNoUpstreamCall", func(t *testing.T) {
		var upCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()

		b, caller := newTestBrokerForLLM(t, []byte(realKey), up.URL)
		tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
		b.BumpEpoch() // invalidates all tokens at the previous epoch

		rec := httptest.NewRecorder()
		req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		b.JailHandler().ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Fatal("epoch-bumped token got 200; want denied")
		}
		if n := upCalls.Load(); n != 0 {
			t.Fatalf("upstream called %d time(s) for epoch-bumped token; want 0", n)
		}
	})

	// ── Assertion 5: SSRF / fixed upstream ──────────────────────────────────
	// The proxy must ONLY dial b.llmUpstream.Host regardless of what Host
	// header (or path) the caller supplies. Set Host: evil.example and assert
	// the fixed fake upstream still received the request.
	t.Run("SSRFFixedUpstream", func(t *testing.T) {
		var upCalls atomic.Int32
		var upSeenHost string

		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upCalls.Add(1)
			upSeenHost = r.Host // what the proxy forwarded
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		}))
		defer up.Close()

		b, caller := newTestBrokerForLLM(t, []byte(realKey), up.URL)
		tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

		rec := httptest.NewRecorder()
		req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Host = "evil.example" // attacker-controlled Host header
		b.JailHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		// The fixed upstream MUST have received the request.
		if upCalls.Load() == 0 {
			t.Fatal("fake upstream was not called; request did not reach configured upstream")
		}
		// The Host the upstream saw must be the fixed upstream, not evil.example.
		wantHost := strings.TrimPrefix(up.URL, "http://")
		if upSeenHost != wantHost {
			t.Errorf("upstream saw Host=%q, want fixed upstream %q (SSRF vector)", upSeenHost, wantHost)
		}
	})
}
