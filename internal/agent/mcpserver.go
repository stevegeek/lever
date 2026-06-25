package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// MCPConfig assembles the capability MCP server.
type MCPConfig struct {
	BrokerURL string
	AgentCN   string
	Client    *http.Client // mTLS client (this agent's identity)
}

// MCPServer is the LLM-facing capability tool: request / attenuate / delegate.
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

func (s *MCPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
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
		s.handleToolsCall(w, id, msg)
	default:
		writeRPCError(w, id, -32601, "method not found")
	}
}

func capabilityToolSchemas() []any {
	strProp := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return []any{
		map[string]any{"name": "request", "description": "mint a capability token bound to self (or bound_to)",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"tool": strProp("tool name"), "op": strProp("operation"), "bound_to": strProp("agent to bind to (default self)")}}},
		map[string]any{"name": "attenuate", "description": "narrow a token by adding constraint key=value pairs",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"token": strProp("base64url token")}}},
		map[string]any{"name": "delegate", "description": "mint a token bound to another agent, narrowed by constraints, to hand off",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"tool": strProp("tool"), "op": strProp("operation"), "to": strProp("recipient agent")}}},
	}
}

func (s *MCPServer) handleToolsCall(w http.ResponseWriter, id any, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	name, _ := params["name"].(string)
	rawArgs, _ := params["arguments"].(map[string]any)
	args := map[string]string{}
	for k, v := range rawArgs {
		if str, ok := v.(string); ok {
			args[k] = str
		}
	}
	ctx := context.Background()
	result := func(tok string) {
		writeRPCResult(w, id, map[string]any{"content": []any{map[string]any{"type": "text", "text": tok}}})
	}
	switch name {
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
	case "attenuate":
		tok, err := Attenuate(args["token"], constraintArgs(args, "token"))
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(tok)
	case "delegate":
		tok, err := Request(ctx, s.brokerURL, s.client, args["tool"], args["op"], args["to"], nil)
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		narrowed, err := Attenuate(tok, constraintArgs(args, "tool", "op", "to"))
		if err != nil {
			writeRPCError(w, id, -32000, err.Error())
			return
		}
		result(narrowed)
	default:
		writeRPCError(w, id, -32601, "unknown tool")
	}
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
