package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeBroker starts an httptest broker answering every request with handle,
// which receives the request path and the JSON-decoded body, and returns a
// brokerCaller aimed at it.
func fakeBroker(t *testing.T, handle func(w http.ResponseWriter, gotPath string, gotBody map[string]any)) brokerCaller {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		handle(w, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	return httpCaller{client: srv.Client(), baseURL: srv.URL}
}
