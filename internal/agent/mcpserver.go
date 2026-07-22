package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// MCPConfig assembles the capability MCP server.
type MCPConfig struct {
	BrokerURL string
	AgentCN   string
	Client    *http.Client // mTLS client (this agent's identity)
}

// MCPServer is the LLM-facing capability tool: request / delegate.
// It holds the mTLS client + the agent's CN and never exposes key material.
type MCPServer struct {
	brokerURL string
	agentCN   string
	client    *http.Client
}

func NewMCPServer(c MCPConfig) *MCPServer {
	return &MCPServer{brokerURL: c.BrokerURL, agentCN: c.AgentCN, client: c.Client}
}

func (s *MCPServer) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// mcpMaxBodyBytes caps the JSON-RPC request body to 1 MiB. The MCP handler is
// driven by a potentially-malicious in-jail LLM, so we bound reads to prevent
// memory exhaustion. (The broker uses the same http.MaxBytesReader pattern with
// per-endpoint caps; here we allow more headroom for MCP tool arguments.)
const mcpMaxBodyBytes = 1 << 20 // 1 MiB

func (s *MCPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes))
	if err != nil {
		writeRPCError(w, nil, -32700, "read error")
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	id := msg["id"]
	method, _ := msg["method"].(string)
	switch method {
	case "initialize":
		writeRPCResult(w, id, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "lever-capability", "version": "0.1.0"},
		})
	case "tools/list":
		writeRPCResult(w, id, map[string]any{"tools": capabilityToolSchemas()})
	case "tools/call":
		s.handleToolsCall(w, r, id, msg)
	default:
		writeRPCError(w, id, -32601, "method not found")
	}
}

func capabilityToolSchemas() []any {
	strProp := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return []any{
		map[string]any{"name": "request", "description": "mint a capability token bound to self (or bound_to)",
			"inputSchema": map[string]any{"type": "object",
				"required": []string{"tool", "op"},
				"properties": map[string]any{
					"tool": strProp("tool name"),
					"op":   strProp("operation"),
					// bound_to is optional: if omitted the server defaults it to the caller's own AgentCN (self).
					// Security note: whether the caller supplies bound_to or relies on the self default,
					// the broker's MayObtain policy (keyed on the mTLS caller identity) is the
					// authoritative gate. A wider bound_to is not a client-side escalation — the
					// broker will refuse to mint a binding the caller's policy does not allow.
					"bound_to": strProp("agent to bind to (default self); the broker's MayObtain policy enforces who the caller may bind to"),
				}}},
		map[string]any{"name": "delegate", "description": "mint a token bound to another agent, narrowed by constraint key=value pairs, to hand off",
			"inputSchema": map[string]any{"type": "object",
				"required": []string{"tool", "op", "to"},
				"properties": map[string]any{
					"tool": strProp("tool"),
					"op":   strProp("operation"),
					"to":   strProp("recipient agent"),
				}}},
		map[string]any{"name": "directive_consume", "description": "Consume a pending operator directive by id. Returns the verified action if and only if this agent is the directive's target and it is still active. Single use.",
			"inputSchema": map[string]any{"type": "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": strProp("the directive_id from the pointer notification, e.g. \"f81d5baa-8fe4-21e1-0408-35026a57ec47\""),
				}}},
		map[string]any{"name": "directive_check", "description": "Check the status of an operator directive addressed to this agent (read-only).",
			"inputSchema": map[string]any{"type": "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": strProp("the directive_id from the pointer notification, e.g. \"f81d5baa-8fe4-21e1-0408-35026a57ec47\""),
				}}},
	}
}

func (s *MCPServer) handleToolsCall(w http.ResponseWriter, r *http.Request, id any, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	name, _ := params["name"].(string)
	rawArgs, _ := params["arguments"].(map[string]any)
	args := map[string]string{}
	for k, v := range rawArgs {
		if str, ok := v.(string); ok {
			// Non-string args are intentionally ignored: dropping an unrecognised
			// constraint is the safe/narrowing direction since the broker re-validates
			// every surviving constraint and gates the bind target via MayObtain.
			args[k] = str
		}
	}
	// Thread the request context so a dropped HTTP connection cancels the outbound
	// broker call.
	ctx := r.Context()
	result := func(tok string) {
		writeRPCResult(w, id, map[string]any{"content": []any{map[string]any{"type": "text", "text": tok}}})
	}
	switch name {
	// request: mint a capability token.
	// The bind target (bound_to, defaulted to the server's own AgentCN when absent)
	// is authoritatively gated by the broker's MayObtain policy keyed on the mTLS
	// caller identity. An LLM-supplied bound_to that is wider than policy allows is
	// therefore not a client-side escalation — the broker refuses to mint it.
	case "request":
		boundTo := args["bound_to"]
		if boundTo == "" {
			boundTo = s.agentCN
		}
		tok, err := Request(ctx, s.brokerURL, s.client, args["tool"], args["op"], boundTo, constraintArgs(args, "tool", "op", "bound_to"))
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(tok)
	// delegate: mint a token bound to another agent, narrowed by the extra kv
	// constraints. The broker's MayObtain policy authoritatively gates whether the
	// mTLS caller may delegate this capability to `to`, and re-validates every
	// constraint; minting bound-to-other is a single online call.
	case "delegate":
		tok, err := Request(ctx, s.brokerURL, s.client, args["tool"], args["op"], args["to"], constraintArgs(args, "tool", "op", "to"))
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(tok)
	// directive_consume: atomically consume a pending operator directive addressed
	// to this agent, over its own mTLS channel. On success the broker's action JSON
	// is surfaced verbatim as the tool result. On any failure — unknown id, wrong
	// target, already consumed, expired, stale generation — the broker's opaque
	// 404 body is surfaced via a JSON-RPC error, so the model sees "not found" and
	// nothing more (no oracle for which failure occurred).
	case "directive_consume":
		did, argErr := directiveID(args)
		if argErr != nil {
			writeRPCError(w, id, -32602, argErr.Error())
			return
		}
		raw, err := DirectiveConsume(ctx, s.brokerURL, s.client, did)
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(string(raw))
	// directive_check: read-only status check for an operator directive addressed
	// to this agent. Same target-gated, opaque-failure surface as directive_consume.
	case "directive_check":
		did, argErr := directiveID(args)
		if argErr != nil {
			writeRPCError(w, id, -32602, argErr.Error())
			return
		}
		raw, err := DirectiveCheck(ctx, s.brokerURL, s.client, did)
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(string(raw))
	default:
		writeRPCError(w, id, -32601, "unknown tool")
	}
}

// directiveID resolves the directive id argument, accepting `directive_id` as
// an alias for `id`. The alias is not cosmetic: every spelling the model ever
// sees for a directive identifier — the signed statement's `directive_id`
// field, `lever directive send`'s output, the pointer notification's prose — is
// `directive_id`, so that is what it tends to send.
//
// A missing id fails HERE rather than being posted as an empty id, because the
// broker answers a bad body with its opaque 404 — byte-identical to "unknown
// id / wrong target / already consumed / expired / stale generation". That
// opacity is deliberate for directive STATE (no oracle), but a client-side
// argument mistake carries no state information, and reporting it as "not
// found" is actively harmful: the agent concludes the operator's directive does
// not exist and refuses a genuine authorization. Returning -32602 (invalid
// params) leaks nothing about any directive — this check runs before any lookup.
func directiveID(args map[string]string) (string, error) {
	did := args["id"]
	if did == "" {
		did = args["directive_id"]
	}
	if did == "" {
		return "", fmt.Errorf(`missing required argument "id" (the directive id from the pointer notification)`)
	}
	return did, nil
}

// constraintArgs returns args minus the reserved keys (the rest are constraint kv).
func constraintArgs(args map[string]string, reserved ...string) map[string]string {
	skip := map[string]bool{}
	for _, k := range reserved {
		skip[k] = true
	}
	out := map[string]string{}
	for k, v := range args {
		if !skip[k] {
			out[k] = v
		}
	}
	return out
}

func writeRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
