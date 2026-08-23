package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/stevegeek/lever/internal/mcp"
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

// Handler is the HTTP adapter over Handle (body bounded by mcp.MaxBodyBytes).
func (s *MCPServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcp.ServeHTTP(w, r, s.Handle) })
}

// Handle is the transport-free core: it takes one raw JSON-RPC message and
// returns the framed reply (always non-empty). Stdio bridges (lever-agent
// serve-capability) call this directly, one line in, one line out; ctx cancels
// the outbound broker call.
func (s *MCPServer) Handle(ctx context.Context, msg []byte) []byte {
	return mcp.Dispatch(ctx, msg, mcp.Service{
		Name:    "lever-capability",
		Version: Version,
		Tools:   capabilityToolSchemas,
		Call:    s.handleToolsCall,
	})
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
// reached us.
//
// It MUST be a plain top-level object: no anyOf/oneOf/allOf combinator and no
// top-level `required`. The Anthropic API rejects a tool input schema
// carrying a top-level combinator, and Claude Code then silently DROPS the
// tool from the session ("No such tool available") — so the directive channel
// dies invisibly for exactly the client it exists for (#24). The
// exactly-one-spelling rule never depended on the schema: directiveID()
// enforces it caller-side (neither supplied, and both-supplied-and-disagreeing,
// each return JSON-RPC -32602 before any broker call), so the schema only
// advertises the two properties without constraining their combination.
//
// Returns a fresh map per call: the two tool entries must not share one
// mutable schema value.
func directiveInputSchema(strProp func(string) map[string]any) map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{
			"id":           strProp(`the directive_id from the pointer notification, e.g. "f81d5baa-8fe4-21e1-0408-35026a57ec47" — supply this OR directive_id, not both`),
			"directive_id": strProp("alias for `id` — supply either one, not both"),
		}}
}

// invalidParams marks a caller-side argument error (JSON-RPC -32602, invalid
// params) as opposed to a failed broker call (-32000).
type invalidParams struct{ err error }

func (e invalidParams) Error() string { return e.err.Error() }
func (e invalidParams) Unwrap() error { return e.err }

// capabilityTools dispatches a tools/call by name. Every tool returns the text
// of its result, or an error: invalidParams for an argument mistake, anything
// else for a failed broker call.
var capabilityTools = map[string]func(*MCPServer, context.Context, map[string]string) (string, error){
	// request: mint a capability token.
	// The bind target (bound_to, defaulted to the server's own AgentCN when absent)
	// is authoritatively gated by the broker's MayObtain policy keyed on the mTLS
	// caller identity. An LLM-supplied bound_to that is wider than policy allows is
	// therefore not a client-side escalation — the broker refuses to mint it.
	"request": func(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
		return s.mint(ctx, args, requestArgs)
	},
	// delegate: mint a token bound to another agent, narrowed by the extra kv
	// constraints. The broker's MayObtain policy authoritatively gates whether the
	// mTLS caller may delegate this capability to `to`, and re-validates every
	// constraint; minting bound-to-other is a single online call.
	"delegate": func(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
		return s.mint(ctx, args, delegateArgs)
	},
	// directive_consume: atomically consume a pending operator directive addressed
	// to this agent, over its own mTLS channel. On success the broker's action JSON
	// is surfaced verbatim as the tool result. On any failure — unknown id, wrong
	// target, already consumed, expired, stale generation — the broker's opaque
	// 404 body is surfaced via a JSON-RPC error, so the model sees "not found" and
	// nothing more (no oracle for which failure occurred).
	"directive_consume": func(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
		return s.directive(ctx, args, DirectiveConsume)
	},
	// directive_check: read-only status check for an operator directive addressed
	// to this agent. Same target-gated, opaque-failure surface as directive_consume.
	"directive_check": func(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
		return s.directive(ctx, args, DirectiveCheck)
	},
}

func (s *MCPServer) handleToolsCall(ctx context.Context, id any, msg map[string]any) []byte {
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
	tool, ok := capabilityTools[name]
	if !ok {
		return mcp.Error(id, mcp.CodeMethodNotFound, "unknown tool")
	}
	text, err := tool(s, ctx, args)
	var bad invalidParams
	switch {
	case errors.As(err, &bad):
		return mcp.Error(id, mcp.CodeInvalidParams, err.Error())
	case err != nil:
		return mcp.Error(id, mcp.CodeServerError, err.Error())
	}
	return mcp.TextResult(id, text)
}

// mint validates a capability tool's arguments with parse (requestArgs or
// delegateArgs) and asks the broker for the token.
func (s *MCPServer) mint(ctx context.Context, args map[string]string, parse func(map[string]string, string) (capMint, error)) (string, error) {
	m, err := parse(args, s.agentCN)
	if err != nil {
		return "", invalidParams{err}
	}
	return Request(ctx, s.brokerURL, s.client, m.tool, m.op, m.boundTo, constraintArgs(args, m.reserved...))
}

// directive resolves the directive id and runs call (DirectiveConsume or
// DirectiveCheck) against the broker, surfacing its raw JSON verbatim.
func (s *MCPServer) directive(ctx context.Context, args map[string]string, call func(context.Context, string, *http.Client, string) (json.RawMessage, error)) (string, error) {
	did, err := directiveID(args)
	if err != nil {
		return "", invalidParams{err}
	}
	raw, err := call(ctx, s.brokerURL, s.client, did)
	if err != nil {
		return "", err
	}
	return string(raw), nil
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

// The bind-target argument of each capability tool. They differ, and the
// difference matters below: `bound_to` is lever's own vocabulary and is never a
// tool's argument name, whereas `to` is an ordinary argument name for a real
// tool (mail, message, transfer).
const (
	capBindRequest  = "bound_to"
	capBindDelegate = "to"
)

// capMint is a capability tool's validated arguments: the mint parameters plus
// the keys that must NOT survive into the token as constraints.
type capMint struct {
	tool, op, boundTo string
	reserved          []string
}

// missingArg names the argument the caller got wrong. "or non-string" because
// handleToolsCall drops non-string argument values, so {"to": 123} arrives
// indistinguishable from an absent one.
func missingArg(name string) error {
	return errors.New(`missing or non-string required argument "` + name + `"`)
}

// mintToolOp validates the two arguments both capability tools share.
//
// This mirrors directiveID's rationale: the broker answers a caller-side
// argument mistake with a policy/registry verdict, which misattributes the
// error. These checks read only the caller's own arguments and run before any
// lookup, so they leak nothing and cannot become an oracle.
func mintToolOp(args map[string]string) (tool, op string, err error) {
	if tool = strings.TrimSpace(args["tool"]); tool == "" {
		return "", "", missingArg("tool")
	}
	if op = strings.TrimSpace(args["op"]); op == "" {
		return "", "", missingArg("op")
	}
	return tool, op, nil
}

// requestArgs validates `request`, whose bind target is optional: absent means
// self, which is the tool's documented contract.
//
// That optionality is exactly why a stray `to` must be rejected here, and why
// this is the load-bearing sibling check rather than delegate's. Nothing else
// would catch it: it passes tool/op validation, `bound_to` resolves empty and
// defaults to self, and `to` falls through into the constraints — yielding a
// self-bound token carrying to=worker, returned as a success. That is the exact
// mirror of the bug this file's guards exist to close.
//
// The argument is genuinely ambiguous, and rejecting it has a real cost.
// Constraint keys ARE tool argument names (registry.MapParams identity-maps
// them; ValidateConstraints leaves keys without an allowed_values entry
// unrestricted), and `to` is an ordinary argument name for a mail, message or
// transfer tool — so this forecloses minting a `to`-narrowed token through the
// MCP tool. Rejecting is still right, on this file's standing trade: a loud
// refusal is recoverable, a confident wrong mint is not. And the refusal has an
// escape hatch the silent path does not — an operator can rename the caveat in
// lever.yaml (`caveat_param: {recipient: to}`), which is plumbed through to the
// registry. The message must therefore not assert the argument is "unknown"
// (false for such a tool, and the repair it invites — dropping the argument —
// mints a WIDER token); it names both readings instead.
func requestArgs(args map[string]string, self string) (capMint, error) {
	tool, op, err := mintToolOp(args)
	if err != nil {
		return capMint{}, err
	}
	if strings.TrimSpace(args[capBindDelegate]) != "" {
		return capMint{}, errors.New(`"` + capBindDelegate + `" is ambiguous on this tool: to bind the token to an agent use "` + capBindRequest + `", or use the "delegate" tool to hand it to another agent`)
	}
	boundTo := strings.TrimSpace(args[capBindRequest])
	if boundTo == "" {
		boundTo = self
	}
	return capMint{tool, op, boundTo, []string{"tool", "op", capBindRequest, capBindDelegate}}, nil
}

// delegateArgs validates `delegate`, whose entire purpose is binding to ANOTHER
// agent — so unlike `request` it has no meaningful default, and every way of
// failing to name a recipient used to succeed silently. handleToolsCall keeps
// every unrecognised string argument and constraintArgs strips only reserved
// keys, so a misspelt `to` was not dropped: it was promoted to a narrowing
// constraint while the bind target went out empty. The broker defaults an empty
// bind target to the caller (request.go, "default: self-obtain"), so a
// SELF-bound token was minted and returned as an ordinary success — the agent
// believed it had handed a capability to someone else. Never a privilege
// question (MayObtain remains the authoritative gate), but a silent one.
func delegateArgs(args map[string]string, self string) (capMint, error) {
	tool, op, err := mintToolOp(args)
	if err != nil {
		return capMint{}, err
	}
	// `bound_to` is the sibling tool's spelling. Both present and disagreeing is
	// ambiguous — pick nothing and say so, exactly as directiveID does for
	// id/directive_id; both present and agreeing states one intent twice and is
	// accepted. Either way it is reserved, never a constraint.
	to, alias := strings.TrimSpace(args[capBindDelegate]), strings.TrimSpace(args[capBindRequest])
	if to != "" && alias != "" && to != alias {
		return capMint{}, errors.New(`"` + capBindDelegate + `" and "` + capBindRequest + `" disagree — name the recipient once, as "` + capBindDelegate + `"`)
	}
	if to == "" {
		if alias != "" {
			return capMint{}, errors.New(`this tool names the recipient with "` + capBindDelegate + `", not "` + capBindRequest + `"`)
		}
		return capMint{}, missingArg(capBindDelegate)
	}
	// Delegating to yourself hands nothing off, and it does not even go through
	// the delegate policy: MayObtainRule branches on requester == boundTo and
	// consults the OBTAIN set, so this succeeds for an agent holding no delegate
	// grant at all and lands in the audit log looking like a self-obtain. Same
	// silent no-op as the empty `to` above; `request` is the tool that mints for
	// self, and it takes the same constraints.
	if to == self {
		return capMint{}, errors.New(`"` + capBindDelegate + `" names this agent — delegate hands a capability to another agent; use "request" to mint one for yourself`)
	}
	return capMint{tool, op, to, []string{"tool", "op", capBindDelegate, capBindRequest}}, nil
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
