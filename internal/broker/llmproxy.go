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

// llmProxyHandler verifies an llm capability token, strips it, injects the real
// Console key, and reverse-proxies (streaming) to the FIXED upstream
// (b.llmUpstream — never client-controlled, so no SSRF) — for the Messages-API
// paths in llmPathAllowed only. Fail closed on any auth/verify failure; never
// log key or token bytes.
func (b *Broker) llmProxyHandler() http.Handler {
	rp := &httputil.ReverseProxy{}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		// Route + scrub forwarding/identity headers (shared with the MCP
		// gateway; forwarding headers only — it never touches credentials).
		rewriteUpstream(pr, b.llmUpstream)
		// Strip the inbound capability token — NEVER forward it upstream.
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("x-api-key")
		// Inject the real Console key + required version header.
		pr.Out.Header.Set("x-api-key", string(b.apiKey))
		if pr.Out.Header.Get("anthropic-version") == "" {
			pr.Out.Header.Set("anthropic-version", "2023-06-01")
		}
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
		// capability(llm) admits the Messages API only — never the rest of
		// what the Console key permits (files, batches, admin endpoints).
		if !llmPathAllowed(r.Method, r.URL.EscapedPath()) {
			b.audit("llm", caller, "deny", "path not allowlisted: "+r.Method+" "+r.URL.EscapedPath(), "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		b.audit("llm", caller, "allow", "", "id", tokID)
		rp.ServeHTTP(w, r)
	})
}

// llmPathAllowed reports whether method + path (post-StripPrefix, escaped
// form so a percent-encoded spelling cannot slip past) is one of the
// Messages-API endpoints Claude Code calls through ANTHROPIC_BASE_URL:
// create/count-tokens messages and list/get models. Exact matches only — no
// trailing slash, no deeper segments (batches, files, organizations, …). The
// model id of GET /v1/models/{id} is charset-checked (validModelID), not just
// slash-free: the escaped path is what is matched here, but the upstream
// decodes it, so a percent-encoded byte (`..%2F..`) would become a different
// path on the far side.
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

// validModelID reports whether id is a plausible Anthropic model id:
// non-empty, only `[A-Za-z0-9._:-]` (`claude-opus-5`,
// `claude-3-5-sonnet-20241022`), and not a bare `.`/`..` segment. No
// percent sign, so no encoded byte survives to the upstream's decoder.
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
