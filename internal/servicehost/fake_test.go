package servicehost

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain doubles as the fake stdio MCP server the bridge tests run: the
// test binary re-executed with FAKE_MCP=1.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_MCP") == "1" {
		fakeMCP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeMCP serves initialize, tools/list and tools/call on stdio. Every
// message it receives is appended to $FAKE_LOG, so a test can prove a call
// never reached it. Before each echo it asks the client for roots and sends
// a notification, which the bridge must answer and drop.
func fakeMCP() {
	logf, _ := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<16), 1<<22)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		line := in.Bytes()
		if logf != nil {
			fmt.Fprintf(logf, "%s\n", line)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Method == "" {
			continue
		}
		reply := func(result any) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		text := func(s string) map[string]any {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
		}
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}})
		case "tools/list":
			var tools []any
			for _, n := range []string{"echo", "env", "crash", "slow", "forbidden"} {
				tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
			}
			reply(map[string]any{"tools": tools})
		case "tools/call":
			switch msg.Params.Name {
			case "echo":
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 9999, "method": "roots/list"})
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info"}})
				reply(text(fmt.Sprintf("pid=%d msg=%s", os.Getpid(), fmt.Sprint(msg.Params.Arguments["msg"]))))
			case "env":
				v, ok := os.LookupEnv(fmt.Sprint(msg.Params.Arguments["name"]))
				reply(text(fmt.Sprintf("%v:%s", ok, v)))
			case "crash":
				os.Exit(3)
			case "slow":
				time.Sleep(3 * time.Second)
				reply(text("slow"))
			default:
				reply(text("called " + msg.Params.Name))
			}
		}
	}
}
