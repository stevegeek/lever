package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/stevegeek/lever/internal/mcp"
)

// ServeStdio runs srv over line-delimited JSON-RPC 2.0: each non-blank line read
// from r is one JSON-RPC message, answered by one line written to w. This is the
// MCP command-mode transport Claude Code uses for the capability server that
// boot registers as `lever-agent serve-capability`: it needs no TCP port inside
// the jail and no cross-container TLS for the MCP channel. The bridge is not
// streaming — one message in, one synchronous reply out — which is all the
// capability tool (request/delegate/directive_*) needs; revisit if the MCP
// session ever needs notifications. Returns when r reaches EOF or fails, or
// (nil) when ctx is cancelled — the verb's ctx is SIGINT/SIGTERM-bound, and a
// blocking read on a stdin that never closes must not outlive the signal. The
// reader goroutine is left parked in its read on cancel; the process exits.
// A line longer than mcp.MaxBodyBytes fails the session (bufio.ErrTooLong) —
// the same 1 MiB cap the HTTP transport applies per request.
func ServeStdio(ctx context.Context, r io.Reader, w io.Writer, srv *MCPServer) error {
	lines := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		// +1: the scanner's limit must also hold the line's delimiter.
		scanner.Buffer(make([]byte, 0, 64*1024), mcp.MaxBodyBytes+1)
		defer func() {
			errc <- scanner.Err()
			close(lines)
		}()
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			select {
			case lines <- bytes.Clone(line): // Scan reuses its buffer
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				return <-errc
			}
			out := bytes.TrimSpace(srv.Handle(ctx, line))
			if len(out) == 0 {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s\n", out); err != nil {
				return fmt.Errorf("serve-capability: write reply: %w", err)
			}
		}
	}
}
