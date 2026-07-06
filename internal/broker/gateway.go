package broker

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// gatewayHandler returns the gated MCP reverse-proxy for one registered tool.
func (b *Broker) gatewayHandler(toolName string) (http.Handler, error) {
	t, ok := b.reg.Lookup(toolName)
	if !ok {
		return nil, fmt.Errorf("broker: gateway for unregistered tool %q", toolName)
	}
	firstParty := t.FirstParty
	coarse := t.Coarse
	// The config backend is a bare host:port listen address (config:
	// `backend: 127.0.0.1:3201`), which url.Parse treats as scheme:opaque and
	// can't proxy. Normalize a scheme-less authority to http:// (loopback tool
	// traffic is plain HTTP); a backend that already carries a scheme (e.g. an
	// httptest server URL) is left untouched.
	backend := t.Backend
	if !strings.Contains(backend, "://") {
		backend = "http://" + backend
	}
	target, err := url.Parse(backend)
	if err != nil {
		return nil, fmt.Errorf("broker: tool %q bad backend %q: %w", toolName, t.Backend, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	base := rp.Director
	rp.Director = func(req *http.Request) {
		base(req)
		req.Host = target.Host
		req.Header.Del("Cookie")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
	}
	// Rewrite tools/list responses to advertise _capability.
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.Request == nil || resp.Request.Header.Get("X-Lever-Method") != "tools/list" {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		out := augmentToolsListSchema(body)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
		return nil
	}

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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, msg, ok := parseJSONRPC(body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch method {
		case "tools/call":
			op, args, capB64, ok := toolsCallFields(msg)
			if !ok || capB64 == "" {
				b.audit(toolName, caller, "deny", "missing capability")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			rawTok, err := base64.RawURLEncoding.DecodeString(capB64)
			if err != nil {
				b.audit(toolName, caller, "deny", "bad capability encoding")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			// A coarse tool's whole surface rides one wildcard capability:
			// require {tool, "*"} regardless of which MCP tool is invoked. The
			// gateway CHOOSES the required op, so a "*" token can never satisfy
			// a fine tool (whose required op is the real params.name) — the
			// wildcard cannot cross grains.
			requiredOp := op
			if coarse {
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
				return
			}
			if b.isRevoked(caller) {
				b.audit(toolName, caller, "deny", "revoked", "id", tokID)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if err := token.Verify(b.keys.Public, rawTok, token.Request{
				Caller: caller, Capability: token.Capability{Tool: toolName, Operation: requiredOp},
				Params: params, Now: time.Now(), MinEpoch: b.MinEpoch(),
			}); err != nil {
				b.audit(toolName, caller, "deny", err.Error(), "id", tokID)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if firstParty {
				// Forward the verified token to the host-side first-party tool so it
				// can re-verify independently; assert the caller it must trust.
				// unconstrained args are intentionally forwarded;
				// pinning every dangerous arg is the minter's job.
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				r.Header.Set("X-Lever-Caller", caller)
			} else {
				cleaned := stripCapability(msg)
				r.Body = io.NopCloser(bytes.NewReader(cleaned))
				r.ContentLength = int64(len(cleaned))
			}
			r.Header.Set("X-Lever-Method", "tools/call")
			// audit the real MCP tool name, even on the coarse path
			b.audit(toolName, caller, "allow", op, "id", tokID)
		case "initialize", "tools/list", "notifications/initialized", "ping":
			// Allowlisted non-capability methods — forward unchanged.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("X-Lever-Method", method)
		default:
			// Any method not in the explicit allowlist is denied. Fail closed.
			b.audit(toolName, caller, "deny", "method not allowlisted: "+method)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}
