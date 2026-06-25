package captool

import (
	"encoding/json"
	"io"
	"net/http"
)

// serveHTTP dispatches a single JSON-RPC request. tools/call is gated in
// verify.go; initialize and tools/list are open (no credentialed action).
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
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
			"serverInfo":      map[string]any{"name": s.name, "version": "0.1.0"},
		})
	case "tools/list":
		writeRPCResult(w, id, map[string]any{"tools": s.toolSchemas()})
	case "tools/call":
		s.handleToolsCall(w, id, msg, r.Header.Get("X-Lever-Caller"))
	default:
		writeRPCError(w, id, -32601, "method not found")
	}
}

// toolSchemas advertises each operation's inputSchema, including _capability.
func (s *Server) toolSchemas() []any {
	tools := make([]any, 0, len(s.ops))
	for _, o := range s.ops {
		props := map[string]any{
			"_capability": map[string]any{"type": "string", "description": "lever capability token authorizing this call"},
		}
		for _, p := range o.Params {
			typ := p.Type
			if typ == "" {
				typ = "string"
			}
			props[p.Name] = map[string]any{"type": typ, "description": p.Description}
		}
		tools = append(tools, map[string]any{
			"name": o.Name, "description": o.Description,
			"inputSchema": map[string]any{"type": "object", "properties": props},
		})
	}
	return tools
}

func writeRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
