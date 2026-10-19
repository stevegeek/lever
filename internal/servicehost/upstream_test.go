package servicehost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type upstreamSeen struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  []string
}

// fakeUpstream answers tools/list as an event stream, a call to "stale" with
// 404 (an unknown session), and every other call as JSON with a session id.
func fakeUpstream(t *testing.T, seen *upstreamSeen) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.headers = append(seen.headers, r.Header.Clone())
		seen.bodies = append(seen.bodies, string(b))
		seen.mu.Unlock()
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(b, &msg)
		switch {
		case msg.Method == "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"+
				"data: {\"jsonrpc\":\"2.0\",\"id\":"+string(msg.ID)+",\"result\":{\"tools\":[{\"name\":\"search\"},{\"name\":\"delete_all\"}]}}\n\n")
		case msg.Params.Name == "stale":
			http.Error(w, "unknown session", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{"content":[{"type":"text","text":"ok"}]}}`)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestUpstreamAddsCredentialsAndPassesOnlySessionHeaders(t *testing.T) {
	seen := &upstreamSeen{}
	up := fakeUpstream(t, seen)
	bearer := writeSecret(t, t.TempDir(), "bearer", "Bearer up-token")
	r := newRig(t, 0, Tool{Name: "notes", Upstream: up.URL + "/mcp", HeaderFiles: map[string]string{"Authorization": bearer},
		Headers: map[string]string{"X-Api-Version": "2"}, Allow: []string{"search", "stale"}})

	req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/mcp/notes", strings.NewReader(call("7", "search", `"q":"x"`)))
	req.Header.Set("Authorization", "Bearer agent-forged")
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("Mcp-Session-Id", "sess-1")
	req.Header.Set("X-Forwarded-For", "10.0.0.9")
	req.Header.Set(CallerHeader, "manager")
	resp, err := r.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resultText(t, string(body)) != "ok" || resp.Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("call: %d %s session %q", resp.StatusCode, body, resp.Header.Get("Mcp-Session-Id"))
	}
	h := seen.headers[0]
	if h.Get("Authorization") != "Bearer up-token" || h.Get("X-Api-Version") != "2" || h.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("upstream headers = %v", h)
	}
	for _, k := range []string{"Cookie", "X-Forwarded-For", CallerHeader} {
		if h.Get(k) != "" {
			t.Fatalf("SECURITY: inbound %s reached the upstream: %v", k, h)
		}
	}

	// An event-stream tools/list comes back as filtered JSON.
	resp2, body2 := r.post("/mcp/notes", `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	if resp2.Header.Get("Content-Type") != "application/json" || !strings.Contains(body2, `"search"`) || strings.Contains(body2, "delete_all") || !strings.Contains(body2, `"id":8`) {
		t.Fatalf("tools/list = %s %s", resp2.Header.Get("Content-Type"), body2)
	}

	// The upstream's 404 for an unknown session reaches the client unchanged.
	if resp3, _ := r.post("/mcp/notes", call("9", "stale", "")); resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", resp3.StatusCode)
	}
	// The ceiling holds for an upstream too.
	if resp4, _ := r.post("/mcp/notes", call("10", "delete_all", "")); resp4.StatusCode != http.StatusForbidden {
		t.Fatalf("outside the ceiling: %d, want 403", resp4.StatusCode)
	}
	if n := len(seen.bodies); n != 3 {
		t.Fatalf("upstream saw %d requests, want 3 (the refused call never reached it)", n)
	}
}

func TestUpstreamDownIs503WithoutTheAddress(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	r := newRig(t, 0, Tool{Name: "notes", Upstream: url, Allow: []string{AllOperations}})
	resp, body := r.post("/mcp/notes", call("1", "search", ""))
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "127.0.0.1") {
		t.Fatalf("down upstream: %d %q", resp.StatusCode, body)
	}
}

func TestResponseFromSSE(t *testing.T) {
	raw := []byte("data: {\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"roots/list\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\n" +
		"data: \"id\":5,\"result\":{\"x\":1}}\n\n")
	got, ok := responseFromSSE(raw, json.Number("5"))
	if !ok || !strings.Contains(string(got), `"x":1`) {
		t.Fatalf("got %s %v", got, ok)
	}
	if _, ok := responseFromSSE([]byte("data: {\"id\":1,\"method\":\"x\"}\n\n"), json.Number("1")); ok {
		t.Fatal("a server request was taken for the response")
	}
}
