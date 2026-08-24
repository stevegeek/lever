// Package mcp holds the JSON-RPC / MCP plumbing shared by the broker gateway,
// the agent's capability MCP server and the captool SDK: message parsing, the
// initialize / tools/list / tools/call dispatch skeleton, result and error
// framing, and the security-relevant tools/call projection that the broker
// and captool must agree on byte-for-byte.
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// MaxBodyBytes caps a JSON-RPC request body at 1 MiB. MCP handlers are driven
// by a potentially-malicious LLM, so reads are bounded to prevent memory
// exhaustion while leaving headroom for tool arguments.
const MaxBodyBytes = 1 << 20

// ProtocolVersion is the MCP protocol version every lever server advertises.
const ProtocolVersion = "2024-11-05"

// CapabilityArg is the tools/call argument that carries the capability token.
const CapabilityArg = "_capability"

// Standard JSON-RPC error codes used across lever's MCP servers.
const (
	CodeParseError     = -32700
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeServerError    = -32000
)

// Parse decodes a JSON-RPC message and returns its method (if any). ok is
// false when the body is not a JSON object.
func Parse(body []byte) (method string, msg map[string]any, ok bool) {
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", nil, false
	}
	method, _ = msg["method"].(string)
	return method, msg, true
}

// Result frames a JSON-RPC success reply.
func Result(id any, result any) []byte {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return out
}

// Error frames a JSON-RPC error reply.
func Error(id any, code int, message string) []byte {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	return out
}

// TextResult frames a tools/call success whose content is a single text block.
func TextResult(id any, text string) []byte {
	return Result(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
}

// CapabilityProperty is the inputSchema property every capability-gated tool
// advertises so the MCP client includes the token on calls.
func CapabilityProperty() map[string]any {
	return map[string]any{"type": "string", "description": "lever capability token authorizing this call"}
}

// Service is what a server plugs into Dispatch: its advertised name, its
// tools/list schemas, and its tools/call implementation. Call receives the
// request id and the whole decoded message and returns a framed reply.
type Service struct {
	Name string
	// Version is reported in the initialize reply's serverInfo; empty
	// falls back to "dev".
	Version string
	Tools   func() []any
	Call    func(ctx context.Context, id any, msg map[string]any) []byte
}

// Dispatch runs one JSON-RPC message through the standard MCP skeleton:
// initialize and tools/list are answered here; tools/call goes to svc.Call;
// anything else is "method not found". It always returns a framed reply.
func Dispatch(ctx context.Context, body []byte, svc Service) []byte {
	method, msg, ok := Parse(body)
	if !ok {
		return Error(nil, CodeParseError, "parse error")
	}
	id := msg["id"]
	switch method {
	case "initialize":
		return Result(id, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": svc.Name, "version": cmp.Or(svc.Version, "dev")},
		})
	case "tools/list":
		return Result(id, map[string]any{"tools": svc.Tools()})
	case "tools/call":
		return svc.Call(ctx, id, msg)
	default:
		return Error(id, CodeMethodNotFound, "method not found")
	}
}

// ServeHTTP is the HTTP adapter over a transport-free handler: it reads a body
// bounded by MaxBodyBytes, runs handle with the request context, and writes
// the framed reply as application/json.
func ServeHTTP(w http.ResponseWriter, r *http.Request, handle func(ctx context.Context, body []byte) []byte) {
	w.Header().Set("Content-Type", "application/json")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		_, _ = w.Write(Error(nil, CodeParseError, "read error"))
		return
	}
	_, _ = w.Write(handle(r.Context(), body))
}

// ToolsCall extracts a tools/call's tool name, canonical string arguments
// (excluding _capability), and the _capability token. ok is false if the shape
// is wrong or any argument value cannot be JSON-encoded.
//
// String values are passed through raw. Non-string values are canonical-JSON-
// encoded so the checked projection is faithful to the forwarded value. This
// closes the bypass where `{"$ne":null}` was silently coerced to "" and could
// satisfy a constraint of table="". The broker gateway and captool both verify
// against this one projection, so they cannot drift apart.
func ToolsCall(msg map[string]any) (name string, args map[string]string, capability string, ok bool) {
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return "", nil, "", false
	}
	name, _ = params["name"].(string)
	rawArgs, _ := params["arguments"].(map[string]any)
	args = map[string]string{}
	for k, v := range rawArgs {
		if k == CapabilityArg {
			capability, _ = v.(string) // non-string _capability -> "" -> denied downstream
			continue
		}
		if s, ok := v.(string); ok {
			args[k] = s
		} else {
			b, err := json.Marshal(v)
			if err != nil {
				// unencodable value: project to something that cannot
				// masquerade as another value; fail closed at verify.
				return "", nil, "", false
			}
			args[k] = string(b)
		}
	}
	if name == "" {
		return "", nil, "", false
	}
	return name, args, capability, true
}

// MapConstraintParams builds the constraint-keyed parameter set the token
// layer verifies against, from the request's projected arguments. Arguments
// are identity-mapped (constraint key == arg name); caveatParam entries add
// renamed bindings (constraint key -> the value of a differently-named arg). A
// renamed arg that is absent produces no binding, so a token constraint on that
// key then fails closed at verification.
func MapConstraintParams(caveatParam, args map[string]string) map[string]string {
	out := make(map[string]string, len(args)+len(caveatParam))
	for k, v := range args { // identity mapping (constraint key == arg name)
		out[k] = v
	}
	for ck, argName := range caveatParam { // renamed bindings
		if v, ok := args[argName]; ok {
			out[ck] = v
		}
	}
	return out
}

// StripCapability re-marshals the message with params.arguments._capability
// removed, so the token never reaches the upstream tool.
func StripCapability(msg map[string]any) []byte {
	if params, ok := msg["params"].(map[string]any); ok {
		if args, ok := params["arguments"].(map[string]any); ok {
			delete(args, CapabilityArg)
		}
	}
	out, _ := json.Marshal(msg)
	return out
}

// AugmentToolsListSchema injects a `_capability` string property into every
// advertised tool's inputSchema.properties, so the MCP client includes the
// token on calls.
func AugmentToolsListSchema(respBody []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody // pass through unparseable bodies unchanged
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return respBody
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return respBody
	}
	for _, ti := range tools {
		tool, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			schema = map[string]any{"type": "object"}
			tool["inputSchema"] = schema
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			props = map[string]any{}
			schema["properties"] = props
		}
		props[CapabilityArg] = CapabilityProperty()
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return respBody
	}
	return out
}
