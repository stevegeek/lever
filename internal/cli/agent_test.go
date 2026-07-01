package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// managerBootstrapPath MUST be rooted under the container /workspace mount, not
// the jail-level /lever path. Inside the manager container, scion mounts the
// tree at /workspace; /lever does not exist. A wrong root causes LoadBootstrap
// to silently return an empty Bootstrap (file absent → nil error), leaving
// every dispatched grove unable to enrol — with no error surfaced to the operator.
func TestManagerBootstrapPathIsContainerWorkspace(t *testing.T) {
	if !strings.HasPrefix(managerBootstrapPath, "/workspace/") {
		t.Fatalf("managerBootstrapPath = %q; must be under the container /workspace mount, not the jail-level /lever", managerBootstrapPath)
	}
}

func TestAgentStart_callsBroker(t *testing.T) {
	// Point the manager client at a fake broker.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"grove": "worker", "phase": "running"})
	}))
	defer srv.Close()

	// Seed a manager bootstrap + identity that resolve to srv.URL with srv.Client().
	// Override the two package seams for the test:
	oldCall := groveCallFn
	groveCallFn = func(ctx context.Context, endpoint string, body any) (groveResult, error) {
		return postGrove(ctx, srv.Client(), srv.URL, endpoint, body)
	}
	defer func() { groveCallFn = oldCall }()

	cmd := newAgentCmd()
	cmd.SetArgs([]string{"start", "worker", "--task", "go"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/grove/start" {
		t.Fatalf("path = %s, want /grove/start", gotPath)
	}
}
