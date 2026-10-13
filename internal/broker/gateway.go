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

// toolSecretHeader must equal captool.ToolSecretHeader; the captool e2e test
// pins the two (an import would cycle through brokertest).
const toolSecretHeader = "X-Lever-Tool-Secret"

// rewriteUpstream routes to target, pins the outbound Host and drops
// client-supplied forwarding/identity headers. Rewrite's Out request already
// lacks inbound X-Forwarded-*, so only Cookie is scrubbed, and X-Forwarded-For
// is set to the immediate client IP without the inbound chain. Credential
// headers are not touched here: the llm proxy injects the real key after this
// runs.
func rewriteUpstream(pr *httputil.ProxyRequest, target *url.URL) {
	pr.SetURL(target)
	pr.Out.Host = target.Host
	pr.Out.Header.Del("Cookie")
	if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
		pr.Out.Header.Set("X-Forwarded-For", ip)
	}
}

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
		// Scrub inbound X-Lever-* so an agent cannot forge broker-internal
		// context such as X-Lever-Caller.
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
		// The secret proves to a first-party tool that X-Lever-Caller is the
		// broker's word. Never sent to a third-party backend; set AFTER the
		// scrub so an inbound forgery is already gone.
		if t.FirstParty && b.toolSecret != "" {
			r.Header.Set(toolSecretHeader, b.toolSecret)
		}
		rp.ServeHTTP(w, r)
	}), nil
}

// backendURL adds http:// to a bare host:port backend, which url.Parse would
// read as scheme:opaque.
func backendURL(backend string) (*url.URL, error) {
	if !strings.Contains(backend, "://") {
		backend = "http://" + backend
	}
	return url.Parse(backend)
}

func newToolProxy(target *url.URL) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		remainder := pr.In.URL.Path
		rewriteUpstream(pr, target)
		// The default join turns "/mcp"+"/" into "/mcp/", which a strict
		// streamable-HTTP endpoint 404s: send a bare-root request to the
		// backend path exactly.
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

// answerNotFound answers an MCP discovery probe with JSON-RPC "method not
// found" so a client moves on quietly. It is never forwarded: the backend's
// resources and prompts stay out of reach. A notification gets only a 202.
func answerNotFound(w http.ResponseWriter, msg map[string]any) {
	id, ok := msg["id"]
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(mcp.Error(id, mcp.CodeMethodNotFound, "method not found"))
}

func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
}

// authorizeToolCall authorizes one MCP message on tool t. tools/call needs a
// verified capability token; the token is forwarded only to a first-party
// tool. Allowlisted methods pass unchanged, discovery probes are answered
// here, and anything else is denied. On false the response is written.
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
		// A coarse tool requires {tool, "*"} whatever MCP tool is invoked.
		// The broker CHOOSES the required op, so a "*" token can never
		// satisfy a fine tool: the wildcard cannot cross grains.
		requiredOp := op
		if t.Coarse {
			requiredOp = registry.WildcardOp
		}
		// The token id ties this use, denials included, to its /request mint
		// line. On deny paths it is the CLAIMED id: the signature is not
		// checked yet.
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
			// The first-party tool re-verifies the token independently.
			// Unconstrained args are forwarded: pinning every dangerous arg
			// is the minter's job.
			setBody(r, body)
			r.Header.Set("X-Lever-Caller", caller)
		} else {
			setBody(r, mcp.StripCapability(msg))
		}
		r.Header.Set("X-Lever-Method", "tools/call")
		// The real MCP tool name, even on the coarse path.
		b.audit(toolName, caller, "allow", op, "id", tokID)
	case "initialize", "tools/list", "notifications/initialized", "ping":
		setBody(r, body)
		r.Header.Set("X-Lever-Method", method)
	case "server/discover", "resources/list", "resources/templates/list", "prompts/list":
		// Revocation must leave a trace of every later attempt.
		if b.isRevoked(caller) {
			b.audit(toolName, caller, "deny", "revoked: "+method)
		} else {
			b.log.Debug("broker.unsupported", "op", toolName, "caller", caller, "method", method)
		}
		answerNotFound(w, msg)
		return false
	default:
		// Fail closed.
		b.audit(toolName, caller, "deny", "method not allowlisted: "+method)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}
