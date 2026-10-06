package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

func TestShareFilePostsToAndPathAndReturnsTheAnswer(t *testing.T) {
	var got wire.FileShareRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathFilesShare {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(wire.FileShareResponse{OK: true, ID: strings.Repeat("a", 32), Name: "v3.xlsm", Size: 8})
	}))
	defer srv.Close()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client()})
	out := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"share_file","arguments":{"to":"c@example.com","path":" /workspace/.lever-files/out/k/v3.xlsm "}}}`)
	if got.To != "c@example.com" || got.Path != "/workspace/.lever-files/out/k/v3.xlsm" || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("posted %+v, answer %s", got, out)
	}
}

func TestShareFileNeedsBothArguments(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://127.0.0.1:1", AgentCN: "worker", Client: http.DefaultClient})
	reply := string(s.Handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"share_file","arguments":{"to":"c@x"}}}`)))
	if !strings.Contains(reply, "-32602") {
		t.Fatalf("%s", reply)
	}
}

func TestContactFilesPassesTheFilter(t *testing.T) {
	var got wire.FilesListRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathFilesList {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"enabled":true,"contacts":[],"uploads":[],"shares":[]}`))
	}))
	defer srv.Close()
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "worker", Client: srv.Client()})
	out := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"contact_files","arguments":{"contact":"c@example.com"}}}`)
	if got.Contact != "c@example.com" || !strings.Contains(out, `"enabled":true`) {
		t.Fatalf("%+v %s", got, out)
	}
}

func TestFileToolsAreListed(t *testing.T) {
	b, _ := json.Marshal(capabilityToolSchemas())
	for _, n := range []string{`"contact_files"`, `"share_file"`} {
		if !strings.Contains(string(b), n) {
			t.Errorf("tools/list lacks %s", n)
		}
	}
}
