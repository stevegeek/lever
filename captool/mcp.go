package captool

import (
	"context"
	"net/http"

	"github.com/stevegeek/lever/internal/mcp"
)

// serveHTTP runs a single JSON-RPC request through the shared MCP skeleton
// (mcp.Dispatch): tools/call is gated in verify.go; initialize and tools/list
// are open (no credentialed action). The body read is bounded by
// mcp.MaxBodyBytes.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	caller := r.Header.Get("X-Lever-Caller")
	mcp.ServeHTTP(w, r, func(ctx context.Context, body []byte) []byte {
		return mcp.Dispatch(ctx, body, mcp.Service{
			Name:    s.name,
			Version: s.version,
			Tools:   s.toolSchemas,
			Call: func(_ context.Context, id any, msg map[string]any) []byte {
				return s.handleToolsCall(id, msg, caller)
			},
		})
	})
}

// toolSchemas advertises each operation's inputSchema, including _capability.
func (s *Server) toolSchemas() []any {
	tools := make([]any, 0, len(s.ops))
	for _, o := range s.ops {
		props := map[string]any{mcp.CapabilityArg: mcp.CapabilityProperty()}
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
