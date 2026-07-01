package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGroveCall_postsAndDecodes(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"grove": "worker", "phase": "running"})
	}))
	defer srv.Close()

	// Inject a client + base URL (bypass mTLS bootstrap for the unit test).
	res, err := postGrove(context.Background(), srv.Client(), srv.URL, "/grove/start",
		map[string]string{"grove": "worker", "task": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/grove/start" || res.Phase != "running" || res.Grove != "worker" {
		t.Fatalf("path=%s body=%s res=%+v", gotPath, gotBody, res)
	}
}
