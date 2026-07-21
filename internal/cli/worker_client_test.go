package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	res, err := postWorker(context.Background(), srv.Client(), srv.URL, "/worker/start",
		map[string]string{"worker": "worker", "task": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/worker/start" || res.Phase != "running" || res.Worker != "worker" {
		t.Fatalf("path=%s body=%s res=%+v", gotPath, gotBody, res)
	}
}

// TestPostBroker_surfacesBody proves a non-200 broker response has its body
// (the specific deny reason, since task #4a) included in the returned error,
// mirroring agent/capability.go's Request. Before this, postBroker discarded
// the body entirely, so a returned deny reason never reached the caller.
func TestPostBroker_surfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy: may not obtain/delegate (tool=db op=read)", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := postWorker(context.Background(), srv.Client(), srv.URL, "/worker/start", map[string]string{})
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
