package manager

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
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

// execCmd runs cmd with argv, capturing stdout and stderr into one buffer,
// and returns the combined output with Execute's error.
func execCmd(t *testing.T, cmd *cobra.Command, argv ...string) (string, error) {
	t.Helper()
	cmd.SetArgs(argv)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}
