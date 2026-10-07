package manager

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// managerBootstrapPath MUST be rooted under the container /workspace mount, not
// the jail-level /lever path. Inside the manager container, scion mounts the
// tree at /workspace; /lever does not exist. A wrong root causes LoadBootstrap
// to silently return an empty Bootstrap (file absent → nil error), leaving
// every dispatched worker unable to enrol — with no error surfaced to the operator.
func TestManagerBootstrapPathIsContainerWorkspace(t *testing.T) {
	if !strings.HasPrefix(managerBootstrapPath, "/workspace/") {
		t.Fatalf("managerBootstrapPath = %q; must be under the container /workspace mount, not the jail-level /lever", managerBootstrapPath)
	}
}

func TestAgentStart_callsBroker(t *testing.T) {
	// Point the manager client at a fake broker.
	var gotPath string
	c := fakeBroker(t, func(w http.ResponseWriter, path string, _ map[string]any) {
		gotPath = path
		_ = json.NewEncoder(w).Encode(map[string]string{"worker": "worker", "phase": "running"})
	})

	cmd := newAgentCmd(c)
	cmd.SetArgs([]string{"start", "worker", "--task", "go"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/worker/start" {
		t.Fatalf("path = %s, want /worker/start", gotPath)
	}
}

func TestAgentRecycle_postsWorkerAndTask(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := fakeBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]string{"worker": "deal-1", "phase": "running"})
	})

	cmd := newAgentCmd(c)
	cmd.SetArgs([]string{"recycle", "deal-1", "--task", "close deal 42"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/worker/recycle" {
		t.Fatalf("path = %s, want /worker/recycle", gotPath)
	}
	if gotBody["worker"] != "deal-1" || gotBody["task"] != "close deal 42" {
		t.Fatalf("body = %v, want worker deal-1 and the task", gotBody)
	}
}
