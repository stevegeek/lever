package broker

import (
	"encoding/base64"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// llmProxyHandler verifies an llm capability token, swaps it for the real
// key, and proxies the allowlisted Messages-API paths to the FIXED upstream
// (never client-controlled, so no SSRF). Fails closed; never logs key or
// token bytes.
func (b *Broker) llmProxyHandler() http.Handler {
	rp := &httputil.ReverseProxy{}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		rewriteUpstream(pr, b.llmUpstream)
		// NEVER forward the capability token upstream.
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("x-api-key")
		pr.Out.Header.Set("x-api-key", string(b.apiKey))
		if pr.Out.Header.Get("anthropic-version") == "" {
			pr.Out.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	// Do not echo upstream auth headers back to the jail.
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("WWW-Authenticate")
		resp.Header.Del("x-api-key")
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		b.audit("llm", "", "error", "upstream: "+err.Error())
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := ca.RequireAgent(r)
		if err != nil {
			b.audit("llm", "", "deny", "no client cert")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		raw, ok := bearerToken(r)
		if !ok {
			b.audit("llm", caller, "deny", "missing capability")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Parsed before the revoked check so a replay after revocation still
		// ties to its mint. On deny paths it is the CLAIMED id.
		tokID := token.ID(raw)
		if b.isRevoked(caller) {
			b.audit("llm", caller, "deny", "revoked", "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := token.Verify(b.keys.Public, raw, token.Request{
			Caller:     caller,
			Capability: token.Capability{Tool: ReservedLLMTool, Operation: ReservedLLMOp},
			Now:        time.Now(),
			MinEpoch:   b.MinEpoch(),
		}); err != nil {
			b.audit("llm", caller, "deny", "token: "+err.Error(), "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The Messages API only, never the rest of what the key permits.
		if !llmPathAllowed(r.Method, r.URL.EscapedPath()) {
			b.audit("llm", caller, "deny", "path not allowlisted: "+r.Method+" "+r.URL.EscapedPath(), "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		b.audit("llm", caller, "allow", "", "id", tokID)
		rp.ServeHTTP(w, r)
	})
}

// llmPathAllowed matches the escaped path exactly against the endpoints
// Claude Code calls through ANTHROPIC_BASE_URL. The model id is
// charset-checked because the upstream decodes the path, so `..%2F..` would
// become a different path there.
func llmPathAllowed(method, path string) bool {
	switch method {
	case http.MethodPost:
		return path == "/v1/messages" || path == "/v1/messages/count_tokens"
	case http.MethodGet:
		if path == "/v1/models" {
			return true
		}
		id, ok := strings.CutPrefix(path, "/v1/models/")
		return ok && validModelID(id)
	}
	return false
}

// validModelID allows no percent sign, so no encoded byte reaches the
// upstream's decoder, and no bare `.`/`..` segment.
func validModelID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// bearerToken decodes the token Claude Code sends as a bearer credential
// (ANTHROPIC_AUTH_TOKEN, base64url).
func bearerToken(r *http.Request) ([]byte, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(h[len(p):]))
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}
