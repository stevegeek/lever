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

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
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

// newTestBrokerForLLM builds a broker whose registry contains the reserved "llm"
// pseudo-tool and whose APIKey + LLMUpstream are wired for proxy tests.
// Returns the broker and the caller identity ("worker").
func newTestBrokerForLLM(t *testing.T, apiKey []byte, upstreamURL string) (*Broker, string) {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Register(registry.Tool{
		Name:       ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{ReservedLLMOp: {Name: ReservedLLMOp}},
		FirstParty: true,
	}); err != nil {
		t.Fatal(err)
	}
	b := New(Config{
		Keys:        kp,
		CA:          c,
		Tickets:     ca.NewTicketStore(),
		Registry:    reg,
		Agents:      []string{"worker"},
		APIKey:      apiKey,
		LLMUpstream: upstreamURL,
	})
	return b, "worker"
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
