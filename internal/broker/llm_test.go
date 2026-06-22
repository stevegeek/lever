package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

func TestLLMProxySwapsKey(t *testing.T) {
	var gotAPIKey, gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	b := newTestBroker(t)
	b.policy.LLM = LLMConfig{Mode: LLMAPIKey, APIKey: "sk-ant-real", Backend: up.URL}

	tok, _ := token.Mint(b.keys.Private, token.Grant{Agent: "scratch", Tools: []string{"llm"}, Expiry: time.Now().Add(time.Hour)})
	r := requestWith(t, b, "scratch", tok)
	r.URL.Path = "/llm/v1/messages"
	r.Method = "POST"
	w := httptest.NewRecorder()

	b.llmHandler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if gotAPIKey != "sk-ant-real" {
		t.Errorf("x-api-key = %q, want injected real key", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("biscuit leaked upstream: %q", gotAuth)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
}
