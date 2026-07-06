package broker

import (
	"encoding/base64"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// llmProxyHandler verifies an llm capability token, strips it, injects the real
// Console key, and reverse-proxies (streaming) to the FIXED upstream
// (b.llmUpstream — never client-controlled, so no SSRF). Fail closed on any
// auth/verify failure; never log key or token bytes.
func (b *Broker) llmProxyHandler() http.Handler {
	rp := httputil.NewSingleHostReverseProxy(b.llmUpstream)
	base := rp.Director
	rp.Director = func(req *http.Request) {
		base(req)
		req.Host = b.llmUpstream.Host
		// Strip the inbound capability token — NEVER forward it upstream.
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		// Inject the real Console key + required version header.
		req.Header.Set("x-api-key", string(b.apiKey))
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		// Scrub forwarding/identity headers.
		req.Header.Del("Cookie")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
	}
	// R5: do not echo upstream auth/error headers back to the jail.
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
		// Token id for mint↔use correlation (best-effort parse; never the
		// token bytes). Parsed before the revoked check so a post-revocation
		// replay still correlates with its mint; on deny paths it is the
		// token's CLAIMED id — the signature has not been checked yet.
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
		b.audit("llm", caller, "allow", "", "id", tokID)
		rp.ServeHTTP(w, r)
	})
}

// bearerToken extracts and base64url-decodes the capability token from the
// Authorization: Bearer header (lever-agent sets ANTHROPIC_AUTH_TOKEN to the
// base64url-encoded raw token, which Claude Code sends as a bearer credential).
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
