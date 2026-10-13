package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/stevegeek/lever/internal/mcp"
)

// ServeStdio runs srv over line-delimited JSON-RPC 2.0: each non-blank line
// read from r is one message, answered by one line written to w. It does not
// stream and sends no notifications. Returns when r reaches EOF or fails, or
// nil when ctx is cancelled, leaving the reader goroutine parked in its read.
// A line longer than mcp.MaxBodyBytes fails the session, the same cap the
// HTTP transport applies per request.
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
