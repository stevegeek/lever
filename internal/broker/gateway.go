package broker

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/mcp"
)

// rewriteUpstream is the shared ProxyRequest rewrite for the broker-side
// reverse proxies (MCP gateway and llm-proxy): route to target, pin the
// outbound Host, and drop client-supplied forwarding/identity headers.
// ReverseProxy.Rewrite hands us an Out request that already lacks the inbound
// X-Forwarded-* headers, so only Cookie needs scrubbing; X-Forwarded-For is
// re-set to the immediate client IP (the pre-Rewrite behaviour) without the
// inbound chain. Forwarding/identity headers ONLY — never credential headers
// (Authorization/x-api-key): the llm proxy injects the real Console key after
// this runs. The agent-side loopback gateway (internal/agent/gateway.go) is a
// different trust context and intentionally does not scrub.
func rewriteUpstream(pr *httputil.ProxyRequest, target *url.URL) {
	pr.SetURL(target)
	pr.Out.Host = target.Host
	pr.Out.Header.Del("Cookie")
	if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
		pr.Out.Header.Set("X-Forwarded-For", ip)
	}
}

// gatewayHandler returns the gated MCP reverse-proxy for one registered tool
// (mounted at /mcp/<name>/ on the jail listener): authenticate the caller,
// scrub forged broker-internal headers, run the per-request authorization
// (authorizeToolCall), then hand the rewritten request to the tool proxy.
func (b *Broker) gatewayHandler(toolName string) (http.Handler, error) {
	t, ok := b.reg.Lookup(toolName)
	if !ok {
		return nil, fmt.Errorf("broker: gateway for unregistered tool %q", toolName)
	}
	target, err := backendURL(t.Backend)
	if err != nil {
		return nil, fmt.Errorf("broker: tool %q bad backend %q: %w", toolName, t.Backend, err)
	}
	rp := newToolProxy(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := ca.RequireAgent(r)
		if err != nil {
			b.audit(toolName, "", "deny", err.Error())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Scrub every inbound X-Lever-* header to prevent jail agents from forging
		// broker-internal context (e.g. X-Lever-Caller).
		for name := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Lever-") {
				r.Header.Del(name)
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gatewayBodyLimit))
		if err != nil {
			b.audit(toolName, caller, "deny", "bad body")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !b.authorizeToolCall(w, r, t, caller, body) {
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

// backendURL parses a registry backend into a proxy target. The config
// backend is a bare host:port listen address (config: `backend:
// 127.0.0.1:3201`), which url.Parse treats as scheme:opaque and can't proxy.
// Normalize a scheme-less authority to http:// (loopback tool traffic is
// plain HTTP); a backend that already carries a scheme (e.g. an httptest
// server URL) is left untouched.
func backendURL(backend string) (*url.URL, error) {
	if !strings.Contains(backend, "://") {
		backend = "http://" + backend
	}
	return url.Parse(backend)
}

// newToolProxy builds the reverse proxy for one tool backend: route +
// forwarding-header scrub (rewriteUpstream), the backend-path collapse for
// a bare-root request, and the tools/list schema rewrite that advertises
// _capability.
func newToolProxy(target *url.URL) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		remainder := pr.In.URL.Path // post-StripPrefix, before the backend-path join
		rewriteUpstream(pr, target)
		// A backend that carries its own path (e.g. qmd's "[::1]:3101/mcp") must
		// receive that path EXACTLY when the MCP client hits the tool root: the
		// default join turns "/mcp"+"/" into "/mcp/", which a strict streamable-
		// HTTP endpoint 404s. Collapse a bare-root remainder to the backend path.
		// Path-less backends (the common case) are untouched.
		if (remainder == "" || remainder == "/") && target.Path != "" && target.Path != "/" {
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = ""
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.Request == nil || resp.Request.Header.Get("X-Lever-Method") != "tools/list" {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		out := mcp.AugmentToolsListSchema(body)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
		return nil
	}
	return rp
}

// setBody replaces r's body with b for the proxy hop.
func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
}

// authorizeToolCall is the per-request authorization state machine for one
// MCP message on tool t. It parses body, and for tools/call it requires a
// capability token, maps the params under the tool's grain, denies a revoked
// caller, verifies the token, and rewrites r's body for the backend (the
// verified token is forwarded to a first-party tool, stripped otherwise).
// The allowlisted non-capability methods are forwarded unchanged; anything
// else is denied. On every deny it audits, writes the response and returns
// false; on true the caller forwards r.
func (b *Broker) authorizeToolCall(w http.ResponseWriter, r *http.Request, t registry.Tool, caller string, body []byte) bool {
	toolName := t.Name
	method, msg, ok := mcp.Parse(body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	switch method {
	case "tools/call":
		op, args, capB64, ok := mcp.ToolsCall(msg)
		if !ok || capB64 == "" {
			b.audit(toolName, caller, "deny", "missing capability")
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		rawTok, err := base64.RawURLEncoding.DecodeString(capB64)
		if err != nil {
			b.audit(toolName, caller, "deny", "bad capability encoding")
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		// A coarse tool's whole surface rides one wildcard capability:
		// require {tool, "*"} regardless of which MCP tool is invoked. The
		// broker CHOOSES the required op, so a "*" token can never satisfy
		// a fine tool (whose required op is the real params.name) — the
		// wildcard cannot cross grains.
		requiredOp := op
		if t.Coarse {
			requiredOp = registry.WildcardOp
		}
		// The token id (best-effort parse; shape-checked) correlates this
		// use with the /request mint line — logged on every post-decode
		// deny too, so a denied attempt (revoked replay included) still
		// ties back to its mint. On deny paths it is the token's CLAIMED
		// id: the signature has not been checked yet.
		tokID := token.ID(rawTok)
		params, err := b.reg.MapParams(toolName, requiredOp, args)
		if err != nil {
			b.audit(toolName, caller, "deny", err.Error(), "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		if b.isRevoked(caller) {
			b.audit(toolName, caller, "deny", "revoked", "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		if err := token.Verify(b.keys.Public, rawTok, token.Request{
			Caller: caller, Capability: token.Capability{Tool: toolName, Operation: requiredOp},
			Params: params, Now: time.Now(), MinEpoch: b.MinEpoch(),
		}); err != nil {
			b.audit(toolName, caller, "deny", err.Error(), "id", tokID)
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		if t.FirstParty {
			// Forward the verified token to the host-side first-party tool so it
			// can re-verify independently; assert the caller it must trust.
			// unconstrained args are intentionally forwarded;
			// pinning every dangerous arg is the minter's job.
			setBody(r, body)
			r.Header.Set("X-Lever-Caller", caller)
		} else {
			setBody(r, mcp.StripCapability(msg))
		}
		r.Header.Set("X-Lever-Method", "tools/call")
		// audit the real MCP tool name, even on the coarse path
		b.audit(toolName, caller, "allow", op, "id", tokID)
	case "initialize", "tools/list", "notifications/initialized", "ping":
		// Allowlisted non-capability methods — forward unchanged.
		setBody(r, body)
		r.Header.Set("X-Lever-Method", method)
	default:
		// Any method not in the explicit allowlist is denied. Fail closed.
		b.audit(toolName, caller, "deny", "method not allowlisted: "+method)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}
