package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

// upstream records the path and Authorization header it received.
func upstream(t *testing.T, gotPath, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		*gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
}

func TestGatedRouteStripsBiscuitAndForwards(t *testing.T) {
	var gotPath, gotAuth string
	up := upstream(t, &gotPath, &gotAuth)
	defer up.Close()

	b := newTestBroker(t)
	b.policy.Routes = []Route{{Operation: "qmd", PathPrefix: "/mcp/qmd/", Backend: up.URL}}

	tok, _ := token.Mint(b.keys.Private, token.Grant{Agent: "scratch", Tools: []string{"qmd"}, Expiry: time.Now().Add(time.Hour)})
	r := requestWith(t, b, "scratch", tok)
	r.URL.Path = "/mcp/qmd/tools/call"
	r.Method = "POST"
	w := httptest.NewRecorder()

	h, err := b.routeHandler(b.policy.Routes[0])
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/tools/call" {
		t.Errorf("upstream path = %q, want /tools/call (prefix stripped)", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("biscuit leaked upstream: Authorization = %q", gotAuth)
	}
}

func TestGatedRouteInjectsSecret(t *testing.T) {
	var gotPath, gotAuth string
	up := upstream(t, &gotPath, &gotAuth)
	defer up.Close()

	b := newTestBroker(t)
	b.policy.Secrets = map[string]string{"github": "ghp_secret"}
	rt := Route{Operation: "github.api", PathPrefix: "/proxy/github/", Backend: up.URL, SecretName: "github"}
	b.policy.Routes = []Route{rt}

	tok, _ := token.Mint(b.keys.Private, token.Grant{Agent: "scratch", Tools: []string{"github.api"}, Expiry: time.Now().Add(time.Hour)})
	r := requestWith(t, b, "scratch", tok)
	r.URL.Path = "/proxy/github/user"
	w := httptest.NewRecorder()

	h, _ := b.routeHandler(rt)
	h.ServeHTTP(w, r)

	if gotAuth != "Bearer ghp_secret" {
		t.Errorf("injected auth = %q, want 'Bearer ghp_secret'", gotAuth)
	}
}

func TestGatedRouteDeniesUngranted(t *testing.T) {
	b := newTestBroker(t)
	rt := Route{Operation: "qmd", PathPrefix: "/mcp/qmd/", Backend: "http://127.0.0.1:1"}
	tok, _ := token.Mint(b.keys.Private, token.Grant{Agent: "scratch", Tools: []string{"calendar"}, Expiry: time.Now().Add(time.Hour)})
	r := requestWith(t, b, "scratch", tok)
	r.URL.Path = "/mcp/qmd/x"
	w := httptest.NewRecorder()
	h, _ := b.routeHandler(rt)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (operation not granted)", w.Code)
	}
}
