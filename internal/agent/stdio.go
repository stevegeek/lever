package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
)

// ServeStdio runs srv over line-delimited JSON-RPC 2.0: each non-blank line read
// from r is one JSON-RPC message, answered by one line written to w. This is the
// MCP command-mode transport Claude Code uses for the capability server that
// boot registers as `lever-agent serve-capability`: it needs no TCP port inside
// the jail and no cross-container TLS for the MCP channel. The bridge is not
// streaming — one message in, one synchronous reply out — which is all the
// capability tool (request/delegate/directive_*) needs; revisit if the MCP
// session ever needs notifications. Returns when r reaches EOF or fails.
func ServeStdio(ctx context.Context, r io.Reader, w io.Writer, srv *MCPServer) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out := bytes.TrimSpace(srv.Handle(ctx, line))
		if len(out) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\n", out); err != nil {
			return fmt.Errorf("serve-capability: write reply: %w", err)
		}
	}
	return scanner.Err()
}
