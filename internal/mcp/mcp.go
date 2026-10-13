// Package mcp holds the JSON-RPC / MCP plumbing shared by the broker gateway,
// the agent's capability MCP server and the captool SDK, including the
// tools/call projection that the broker and captool must agree on
// byte-for-byte.
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// MaxBodyBytes caps a JSON-RPC request body: the caller is a possibly
// hostile LLM.
const MaxBodyBytes = 1 << 20

// ProtocolVersion is the MCP protocol version every lever server advertises.
const ProtocolVersion = "2024-11-05"

// CapabilityArg is the tools/call argument that carries the capability token.
const CapabilityArg = "_capability"

// JSON-RPC error codes.
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

// Service is what a server plugs into Dispatch. Call returns a framed reply.
type Service struct {
	Name string
	// Version is reported in serverInfo; empty means "dev".
	Version string
	Tools   func() []any
	Call    func(ctx context.Context, id any, msg map[string]any) []byte
}

// Dispatch answers initialize and tools/list, hands tools/call to svc.Call,
// and always returns a framed reply.
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

// ServeHTTP runs handle on a request body bounded by MaxBodyBytes.
func ServeHTTP(w http.ResponseWriter, r *http.Request, handle func(ctx context.Context, body []byte) []byte) {
	w.Header().Set("Content-Type", "application/json")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		_, _ = w.Write(Error(nil, CodeParseError, "read error"))
		return
	}
	_, _ = w.Write(handle(r.Context(), body))
}

// ToolsCall extracts a tools/call's tool name, string arguments (without
// _capability) and the _capability token. ok is false if the shape is wrong
// or any argument value cannot be JSON-encoded.
//
// Non-string values are JSON-encoded, never coerced to "", so the checked
// projection is faithful to the forwarded value: `{"$ne":null}` must not
// satisfy a constraint of table="". The broker gateway and captool both
// verify against this one projection so they cannot drift apart.
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
			capability, _ = v.(string) // a non-string token becomes "" and is denied
			continue
		}
		if s, ok := v.(string); ok {
			args[k] = s
		} else {
			b, err := json.Marshal(v)
			if err != nil {
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

// MapConstraintParams builds the constraint-keyed parameters the token layer
// verifies: each argument under its own name, plus caveatParam's renamed
// bindings (constraint key -> argument name). An absent renamed argument
// produces no binding, so a constraint on that key fails closed.
func MapConstraintParams(caveatParam, args map[string]string) map[string]string {
	out := make(map[string]string, len(args)+len(caveatParam))
	for k, v := range args {
		out[k] = v
	}
	for ck, argName := range caveatParam {
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

// AugmentToolsListSchema adds the _capability property to every advertised
// tool's inputSchema, so the MCP client includes the token on calls.
func AugmentToolsListSchema(respBody []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody

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
