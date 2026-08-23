package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerCall_postsAndDecodes(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"worker": "worker", "phase": "running"})
	}))
	defer srv.Close()

	// Inject a client + base URL (bypass mTLS bootstrap for the unit test).
	c := httpCaller{client: srv.Client(), baseURL: srv.URL}
	res, err := workerCall(context.Background(), c, "/worker/start",
		map[string]string{"worker": "worker", "task": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/worker/start" || res.Phase != "running" || res.Worker != "worker" {
		t.Fatalf("path=%s body=%s res=%+v", gotPath, gotBody, res)
	}
}

// TestHTTPCaller_surfacesBody proves a non-200 broker response has its body
// (the specific deny reason, since task #4a) included in the returned error,
// mirroring agent/capability.go's Request. Before this, the caller discarded
// the body entirely, so a returned deny reason never reached the caller.
func TestHTTPCaller_surfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy: may not obtain/delegate (tool=db op=read)", http.StatusForbidden)
	}))
	defer srv.Close()

	c := httpCaller{client: srv.Client(), baseURL: srv.URL}
	_, err := workerCall(context.Background(), c, "/worker/start", map[string]string{})
	if err == nil {
		t.Fatal("want error for non-200 response")
	}
	if !strings.Contains(err.Error(), "policy: may not obtain/delegate") {
		t.Fatalf("error should surface the response body, got: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should still include the status code, got: %v", err)
	}
}

// TestMTLSCaller_missingBootstrapOrIdentity pins the production caller's two
// pre-dial failure modes: an unreadable bootstrap, and a bootstrap with no
// identity beside it, each named in the error.
func TestMTLSCaller_missingBootstrapOrIdentity(t *testing.T) {
	dir := t.TempDir()
	c := mtlsCaller{bootstrapPath: filepath.Join(dir, "bootstrap.json"), idDir: filepath.Join(dir, "id")}
	if err := os.WriteFile(c.bootstrapPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := c.Call(context.Background(), "/worker/list", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "manager bootstrap:") {
		t.Fatalf("err = %v, want a manager bootstrap error", err)
	}
	if err := os.WriteFile(c.bootstrapPath, []byte(`{"broker_url":"https://127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = c.Call(context.Background(), "/worker/list", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "manager identity not found in "+c.idDir) {
		t.Fatalf("err = %v, want a missing-identity error naming %s", err, c.idDir)
	}
}
