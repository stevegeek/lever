package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

func TestContactMessageWritesTheExactTextAndTheCommand(t *testing.T) {
	var got wire.ContactMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathContactMessage {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(wire.ContactMessageResponse{OK: true, Ref: "0123456789abcdef0123456789abcdef", Kind: "initiated", To: "@c@example.com"})
	}))
	defer srv.Close()
	dir := t.TempDir()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client(), BodyDir: dir})
	text := "Workbook v3 is ready.\nTrailing newline kept:\n"
	args, _ := json.Marshal(map[string]string{"to": "c@example.com", "text": text})
	out := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contact_message","arguments":`+string(args)+`}}`)
	if got.To != "c@example.com" || got.Text != text {
		t.Fatalf("posted %+v", got)
	}
	var res struct {
		BodyFile string `json:"body_file"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result %q: %v", out, err)
	}
	b, err := os.ReadFile(res.BodyFile)
	if err != nil || string(b) != text {
		t.Fatalf("body file %q = %q, %v", res.BodyFile, b, err)
	}
	if fi, _ := os.Stat(res.BodyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if res.Command != "scion message --body-file "+res.BodyFile+" -- '@c@example.com'" {
		t.Fatalf("command %q", res.Command)
	}
}

func TestContactMessageRefusalIsAResultNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.ContactMessageResponse{Reason: "limit", NextAllowedAt: "2026-10-07T12:00:00Z"})
	}))
	defer srv.Close()
	dir := t.TempDir()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client(), BodyDir: dir})
	out := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contact_message","arguments":{"to":"c@example.com","text":"hi"}}}`)
	if !strings.Contains(out, `"reason":"limit"`) {
		t.Fatalf("%s", out)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Fatal("a refusal writes no body file")
	}
}

func TestContactMessageRefusesAnUnsafeTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.ContactMessageResponse{OK: true, Ref: "0123456789abcdef0123456789abcdef", To: "@c@example.com'; rm -rf /"})
	}))
	defer srv.Close()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client(), BodyDir: t.TempDir()})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contact_message","arguments":{"to":"c@example.com","text":"hi"}}}`)
	if _, isErr := resp["error"]; !isErr {
		t.Fatalf("an unplain target must be an error, got %v", resp)
	}
}

func TestToolsListAdvertisesContactTools(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "worker"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	raw, _ := json.Marshal(resp)
	for _, want := range []string{`"contacts"`, `"contact_message"`, `"reply_to_ref"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tools/list lacks %s", want)
		}
	}
	if strings.Contains(string(raw), "anyOf") || strings.Contains(string(raw), "oneOf") {
		t.Fatal("no combinators (#24)")
	}
}

// A body directory the command line could not carry plainly is refused
// before anything is written.
func TestContactMessageRefusesAnUnplainBodyDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.ContactMessageResponse{OK: true, Ref: "0123456789abcdef0123456789abcdef", To: "@c@example.com"})
	}))
	defer srv.Close()
	dir := t.TempDir() + "/a b"
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client(), BodyDir: dir})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contact_message","arguments":{"to":"c@example.com","text":"hi"}}}`)
	if _, isErr := resp["error"]; !isErr {
		t.Fatalf("an unplain body path must be an error, got %v", resp)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Fatal("nothing is written")
	}
}

func TestContactsReturnsTheBrokersAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathContacts {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wire.ContactsResponse{Enabled: true, Contacts: []wire.ContactInfo{{Login: "c@example.com", To: "@c@example.com", CanInitiate: true}}})
	}))
	defer srv.Close()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client()})
	out := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contacts","arguments":{}}}`)
	if !strings.Contains(out, `"login":"c@example.com"`) || !strings.Contains(out, `"can_initiate":true`) {
		t.Fatalf("%s", out)
	}
}
