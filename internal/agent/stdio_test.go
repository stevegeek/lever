package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestServeStdioOneLineInOneLineOut pins the command-mode MCP transport: every
// non-blank input line is one JSON-RPC message answered by exactly one output
// line, blank lines are skipped, and a malformed line still gets a framed
// parse-error reply rather than killing the session.
func TestServeStdioOneLineInOneLineOut(t *testing.T) {
	srv := NewMCPServer(MCPConfig{BrokerURL: "https://broker.invalid", AgentCN: "worker", Client: http.DefaultClient})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		"",
		"   ",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`not json`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := ServeStdio(context.Background(), strings.NewReader(in), &out, srv); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 reply lines (blank input lines skipped), got %d: %q", len(lines), out.String())
	}
	var init struct {
		ID     any `json:"id"`
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatalf("initialize reply is not JSON: %v: %s", err, lines[0])
	}
	if init.ID != float64(1) || init.Result.ServerInfo.Name != "lever-capability" {
		t.Errorf("initialize reply = %s, want id 1 from lever-capability", lines[0])
	}
	if !strings.Contains(lines[1], `"request"`) || !strings.Contains(lines[1], `"delegate"`) {
		t.Errorf("tools/list reply = %s, want the capability tools", lines[1])
	}
	var perr struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &perr); err != nil || perr.Error.Code != -32700 {
		t.Errorf("malformed line reply = %s, want a framed -32700 parse error", lines[2])
	}
}

func TestServeStdioEmptyInput(t *testing.T) {
	srv := NewMCPServer(MCPConfig{})
	var out bytes.Buffer
	if err := ServeStdio(context.Background(), strings.NewReader(""), &out, srv); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("no input must produce no output, got %q", out.String())
	}
}
