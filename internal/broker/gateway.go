package broker

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// gatewayHandler returns the gated MCP reverse-proxy for one registered tool.
func (b *Broker) gatewayHandler(toolName string) (http.Handler, error) {
	t, ok := b.reg.Lookup(toolName)
	if !ok {
		return nil, fmt.Errorf("broker: gateway for unregistered tool %q", toolName)
	}
	target, err := url.Parse(t.Backend)
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
			params, err := b.reg.MapParams(toolName, op, args)
			if err != nil {
				b.audit(toolName, caller, "deny", err.Error())
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if b.isRevoked(caller) {
				b.audit(toolName, caller, "deny", "revoked")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if err := token.Verify(b.keys.Public, rawTok, token.Request{
				Caller: caller, Capability: token.Capability{Tool: toolName, Operation: op},
				Params: params, Now: time.Now(), MinEpoch: b.MinEpoch(),
			}); err != nil {
				b.audit(toolName, caller, "deny", err.Error())
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			cleaned := stripCapability(msg)
			r.Body = io.NopCloser(bytes.NewReader(cleaned))
			r.ContentLength = int64(len(cleaned))
			r.Header.Set("X-Lever-Method", "tools/call")
			b.audit(toolName, caller, "allow", op)
		default:
			// initialize, tools/list, notifications, etc. — forward unchanged.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("X-Lever-Method", method)
		}
		rp.ServeHTTP(w, r)
	}), nil
}
