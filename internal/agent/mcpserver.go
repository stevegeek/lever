package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
			"inputSchema": directiveInputSchema(strProp)},
		map[string]any{"name": "directive_check", "description": "Check the status of an operator directive addressed to this agent (read-only).",
			"inputSchema": directiveInputSchema(strProp)},
	}
}

// directiveInputSchema is the shared schema for both directive tools. It
// declares BOTH accepted spellings of the identifier: `id` is canonical, and
// `directive_id` is the name used everywhere else the model looks (the signed
// statement's field, `lever directive send`'s output, the pointer
// notification). The server accepts either, so the advertised contract must
// say so rather than declare one name and quietly tolerate the other — a
// schema-validating client would otherwise reject the alias before it ever
// reached us. anyOf keeps "neither supplied" invalid.
//
// Returns a fresh map per call: the two tool entries must not share one
// mutable schema value.
func directiveInputSchema(strProp func(string) map[string]any) map[string]any {
	return map[string]any{"type": "object",
		"anyOf": []any{
			map[string]any{"required": []string{"id"}},
			map[string]any{"required": []string{"directive_id"}},
		},
		"properties": map[string]any{
			"id":           strProp(`the directive_id from the pointer notification, e.g. "f81d5baa-8fe4-21e1-0408-35026a57ec47"`),
			"directive_id": strProp("alias for `id` — supply either one, not both"),
		}}
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
		tool, op, boundTo, argErr := capabilityArgs(args, capBindRequest)
		if argErr != nil {
			writeRPCError(w, id, -32602, argErr.Error())
			return
		}
		if boundTo == "" {
			boundTo = s.agentCN
		}
		tok, err := Request(ctx, s.brokerURL, s.client, tool, op, boundTo, constraintArgs(args, capReservedKeys...))
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
		tool, op, to, argErr := capabilityArgs(args, capBindDelegate)
		if argErr != nil {
			writeRPCError(w, id, -32602, argErr.Error())
			return
		}
		tok, err := Request(ctx, s.brokerURL, s.client, tool, op, to, constraintArgs(args, capReservedKeys...))
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
// A directive id is a 36-char UUID; the cap only has to be loose enough to
// never reject a real one while keeping junk from reaching the broker.
const maxDirectiveIDLen = 128

func directiveID(args map[string]string) (string, error) {
	id, alias := strings.TrimSpace(args["id"]), strings.TrimSpace(args["directive_id"])
	// Both supplied and disagreeing is ambiguous — pick nothing and say so,
	// rather than silently preferring one and acting on an id the caller may
	// not have meant.
	if id != "" && alias != "" && id != alias {
		return "", errors.New(`supply either "id" or "directive_id", not both`)
	}
	did := id
	if did == "" {
		did = alias
	}
	if did == "" {
		// "or non-string": handleToolsCall drops non-string argument values, so
		// {"id": 12345} arrives here indistinguishable from an absent one.
		return "", errors.New(`missing or non-string required argument "id" (the directive_id from the pointer notification)`)
	}
	if len(did) > maxDirectiveIDLen {
		return "", errors.New("directive id is too long to be valid")
	}
	return did, nil
}

// The bind-target argument of each capability tool, and the reserved keys that
// are never constraints. Both spellings are reserved for BOTH tools: after
// capabilityArgs rejects a populated sibling, only a blank one can still be
// present, and baking `bound_to=""` into a delegated token as a constraint
// would be nonsense.
const (
	capBindRequest  = "bound_to"
	capBindDelegate = "to"
)

var capReservedKeys = []string{"tool", "op", capBindRequest, capBindDelegate}

// capabilityArgs validates the caller's own arguments to `request`/`delegate`
// before any broker call, returning them trimmed. bindKey selects which tool's
// bind-target spelling applies.
//
// This mirrors directiveID's rationale: the broker answers a caller-side
// argument mistake with a policy/registry verdict, which misattributes the
// error — but here the quieter failure is worse than a wrong error string.
// handleToolsCall keeps every unrecognised string argument, and constraintArgs
// strips only reserved keys, so a misspelt bind target was not dropped, it was
// promoted to a narrowing constraint while the bind target itself went out
// empty. The broker defaults an empty bind target to the caller (request.go,
// "default: self-obtain"), so `delegate` with a misspelt `to` minted a
// SELF-bound token and returned it as an ordinary success. Nothing failed; the
// agent simply believed it had handed a capability to someone else.
//
// Never a privilege question — MayObtain still authoritatively gates what the
// caller may mint, and these checks read only the caller's own arguments, so
// they leak nothing and cannot become an oracle.
func capabilityArgs(args map[string]string, bindKey string) (tool, op, boundTo string, err error) {
	// "or non-string": handleToolsCall drops non-string argument values, so
	// {"to": 123} arrives here indistinguishable from an absent one.
	missing := func(name string) error {
		return errors.New(`missing or non-string required argument "` + name + `"`)
	}
	if tool = strings.TrimSpace(args["tool"]); tool == "" {
		return "", "", "", missing("tool")
	}
	if op = strings.TrimSpace(args["op"]); op == "" {
		return "", "", "", missing("op")
	}
	// The two tools sit side by side and spell the bind target differently, so
	// the other one's spelling is the likeliest near-miss — and the one that
	// silently becomes a constraint. Name the argument this tool wants.
	sibling := capBindRequest
	if bindKey == capBindRequest {
		sibling = capBindDelegate
	}
	if strings.TrimSpace(args[sibling]) != "" {
		return "", "", "", errors.New(`unknown argument "` + sibling + `" — this tool binds with "` + bindKey + `"`)
	}
	boundTo = strings.TrimSpace(args[bindKey])
	// request's bound_to is optional and defaults to self by design. delegate's
	// whole purpose is binding to another agent, so falling back to self-obtain
	// is never what the caller meant.
	if boundTo == "" && bindKey == capBindDelegate {
		return "", "", "", missing(capBindDelegate)
	}
	return tool, op, boundTo, nil
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
