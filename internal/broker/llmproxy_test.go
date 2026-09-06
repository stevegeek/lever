package broker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// fakeAnthropic records what the proxy forwarded and replies with an SSE body.
func fakeAnthropic(t *testing.T, gotKey *string, gotAuth *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotKey = r.Header.Get("x-api-key")
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
}

// encodeToken base64url-encodes a raw token for use as an Authorization: Bearer value.
// Reuses base64urlNoPad from testhelpers_test.go (same package).
func encodeToken(raw []byte) string { return base64urlNoPad(raw) }

func mintLLM(t *testing.T, priv ed25519.PrivateKey, agent string, epoch int) string {
	t.Helper()
	raw, err := token.Mint(priv, token.Grant{
		Agent:      agent,
		Capability: token.Capability{Tool: ReservedLLMTool, Operation: ReservedLLMOp},
		Expiry:     time.Now().Add(time.Hour),
		Epoch:      epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encodeToken(raw) // base64.RawURLEncoding — matches the proxy's decode
}

// newMTLSRequest creates an *http.Request with a verified TLS client cert whose
// CN is cn, using the broker's CA to sign — matching how gateway/request tests
// fake ca.RequireAgent via leafFor.
func newMTLSRequest(t *testing.T, b *Broker, cn, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.TLS = leafFor(t, b, cn)
	return req
}

func TestLLMProxyInjectsKeyAndStripsToken(t *testing.T) {
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()

	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if gotKey != "sk-REAL-KEY" {
		t.Errorf("upstream x-api-key = %q, want injected real key", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("inbound capability token leaked upstream as Authorization=%q", gotAuth)
	}
	if strings.Contains(rec.Body.String(), "sk-REAL-KEY") {
		t.Errorf("real key leaked back to the jail in the response body")
	}
}

func TestLLMProxyDeniesRevoked(t *testing.T) {
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()
	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
	b.Revoke(caller)

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("revoked agent got 200; want denied")
	}
	if gotKey != "" {
		t.Fatal("revoked agent caused an upstream call (key forwarded)")
	}
}

func TestLLMProxyDeniesEpochBump(t *testing.T) {
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()
	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
	b.BumpEpoch()

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || gotKey != "" {
		t.Fatal("epoch-bumped token authorized; want denied + no upstream call")
	}
}

func TestLLMProxyDeniesMissingToken(t *testing.T) {
	b, caller := newTestBrokerForLLM(t, []byte("sk"), "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("no token got 200; want 401/403")
	}
}

func TestLLMProxyDeniesNoClientCert(t *testing.T) {
	// RequireAgent (llmproxy.go:50) is the first gate: with no mTLS client cert at
	// all, the proxy must 403 and never reach the upstream — the token is always
	// CN-bound (R4 closed by failing closed).
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()
	b, _ := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)

	rec := httptest.NewRecorder()
	// No req.TLS → no verified client cert.
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+mintLLM(t, b.keys.Private, "worker", b.MinEpoch()))
	b.JailHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no client cert)", rec.Code)
	}
	if gotKey != "" {
		t.Fatal("no-cert request reached the upstream (key forwarded)")
	}
}

func TestLLMProxyDeniesMalformedBearer(t *testing.T) {
	// bearerToken (llmproxy.go:91) base64url-decodes the credential; junk after
	// "Bearer " must yield 401 with no upstream call (no real key injected).
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()
	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer !!!not-base64url!!!")
	b.JailHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (malformed bearer)", rec.Code)
	}
	if gotKey != "" {
		t.Fatal("malformed-bearer request reached the upstream (key forwarded)")
	}
}

func TestLLMProxyAuditCorrelatesTokenID(t *testing.T) {
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()

	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	id := token.ID(raw)
	if id == "" {
		t.Fatal("minted llm token must carry an id")
	}

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "id="+id) {
		t.Fatalf("llm allow audit must carry the token id %q, got: %s", id, buf.String())
	}
}

func TestLLMProxyRevokedDenyAuditCarriesTokenID(t *testing.T) {
	var gotKey, gotAuth string
	up := fakeAnthropic(t, &gotKey, &gotAuth)
	defer up.Close()
	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	id := token.ID(raw)
	b.Revoke(caller)

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (revoked)", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "detail=revoked") || !strings.Contains(out, "id="+id) {
		t.Fatalf("revoked llm deny must carry the token id %q, got: %s", id, out)
	}
}

// capability(llm) is a Messages-API capability, not a blanket pass to the
// upstream: only the endpoints Claude Code uses through ANTHROPIC_BASE_URL
// are proxied. Anything else the Console key would permit (files, batches,
// admin, or a wrong method on a permitted path) is refused with an audit line
// and never reaches the upstream.
func TestLLMProxyAllowlistsMessagesAPIPaths(t *testing.T) {
	cases := []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, "/llm/v1/messages", true},
		{http.MethodPost, "/llm/v1/messages?beta=true", true},
		{http.MethodPost, "/llm/v1/messages/count_tokens", true},
		{http.MethodGet, "/llm/v1/models", true},
		{http.MethodGet, "/llm/v1/models/claude-opus-5", true},
		{http.MethodGet, "/llm/v1/models/claude-3-5-sonnet-20241022", true},
		{http.MethodGet, "/llm/v1/models/org:custom_model.v2", true},

		{http.MethodGet, "/llm/v1/messages", false},
		{http.MethodDelete, "/llm/v1/messages", false},
		{http.MethodPost, "/llm/v1/models", false},
		{http.MethodPost, "/llm/v1/messages/batches", false},
		{http.MethodGet, "/llm/v1/messages/batches/mb_1", false},
		{http.MethodPost, "/llm/v1/files", false},
		{http.MethodGet, "/llm/v1/files/file_1/content", false},
		{http.MethodGet, "/llm/v1/organizations/me", false},
		{http.MethodGet, "/llm/v1/models/", false},
		{http.MethodGet, "/llm/v1/models/a/b", false},
		// The model id is charset-restricted, not merely slash-free: a
		// percent-encoded traversal (or any encoded byte) must not reach the
		// upstream, which would decode it into a different path.
		{http.MethodGet, "/llm/v1/models/..%2F..%2Ffiles", false},
		{http.MethodGet, "/llm/v1/models/%2E%2E", false},
		{http.MethodGet, "/llm/v1/models/claude%2Dopus-5", false},
		{http.MethodGet, "/llm/v1/models/claude%20opus", false},
		{http.MethodGet, "/llm/v1/models/claude-opus-5?x=1", true},
		{http.MethodGet, "/llm/v1/models/claude-opus-5;v=2", false},
		{http.MethodGet, "/llm/v1/models/model@2", false},
		{http.MethodPost, "/llm/v1/messages/", false},
		{http.MethodPost, "/llm/v1/%6Dessages", false}, // percent-encoded: EscapedPath is matched, not the decoded Path
		{http.MethodPost, "/llm/", false},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var gotKey, gotAuth string
			up := fakeAnthropic(t, &gotKey, &gotAuth)
			defer up.Close()
			b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
			var buf bytes.Buffer
			b.log = slog.New(slog.NewTextHandler(&buf, nil))
			tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

			rec := httptest.NewRecorder()
			req := newMTLSRequest(t, b, caller, c.method, c.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			b.JailHandler().ServeHTTP(rec, req)

			if c.allowed {
				if rec.Code != http.StatusOK || gotKey != "sk-REAL-KEY" {
					t.Fatalf("status=%d upstream key=%q; want proxied 200", rec.Code, gotKey)
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403 for a non-allowlisted path", rec.Code)
			}
			if gotKey != "" {
				t.Fatal("SECURITY: non-allowlisted path reached the upstream with the real key")
			}
			if !strings.Contains(buf.String(), "path not allowlisted") {
				t.Fatalf("deny must be audited; log=%s", buf.String())
			}
		})
	}
}
