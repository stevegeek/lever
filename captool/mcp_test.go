package captool

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{
		Name: "db", Backend: "127.0.0.1:0", AdminURL: "http://127.0.0.1:0",
		Operations: []Operation{{
			Name: "read", Description: "read rows",
			Params:      []ParamSpec{{Name: "table", Type: "string"}, {Name: "filter", Type: "string"}},
			CaveatParam: map[string]string{"table": "table", "filter": "filter"},
			Backstop:    func(ValidatedContext, map[string]string) error { return nil },
			Handler: func(_ ValidatedContext, a map[string]string) (any, error) {
				return map[string]string{"table": a["table"]}, nil
			},
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rpc(t *testing.T, s *Server, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestInitializeReturnsServerInfo(t *testing.T) {
	w := rpc(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["result"].(map[string]any)["serverInfo"]; !ok {
		t.Fatalf("no serverInfo: %s", w.Body.String())
	}
}

func TestToolsListAdvertisesOperationWithCapability(t *testing.T) {
	w := rpc(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, nil)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	props := tools[0].(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["_capability"]; !ok {
		t.Error("inputSchema must advertise _capability")
	}
	if _, ok := props["table"]; !ok {
		t.Error("inputSchema must advertise declared params")
	}
}

func TestUnknownMethodIsJSONRPCError(t *testing.T) {
	w := rpc(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`, nil)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["error"]; !ok {
		t.Fatalf("unknown method must be a JSON-RPC error, got %s", w.Body.String())
	}
}
