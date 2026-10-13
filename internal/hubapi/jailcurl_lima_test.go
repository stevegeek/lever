//go:build integration

package hubapi

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// TestJailCurlAgainstRealVM runs the real curl script through a real jail
// runner against an HTTP endpoint inside a VM. It is the only test that covers
// whether the backend's shell wrapper delivers curlScript intact and whether
// the env K=V prefix reaches the shell so the Authorization header expands.
//
// Skipped unless LEVER_VM_PREFIX is set. Run it as:
//
//	# Lima (start a VM, then serve JSON on :8080 inside it)
//	LEVER_VM_PREFIX='limactl,shell,levercurltest' \
//	  go test -tags integration ./internal/hubapi/ -run TestJailCurlAgainstRealVM -v
//
//	# OrbStack
//	LEVER_VM_PREFIX='orb,-m,<vm>' ...
//
// LEVER_VM_URL overrides the endpoint (default http://127.0.0.1:8080/probe).
func TestJailCurlAgainstRealVM(t *testing.T) {
	prefix := os.Getenv("LEVER_VM_PREFIX")
	if prefix == "" {
		t.Skip("set LEVER_VM_PREFIX (e.g. 'limactl,shell,<vm>') to run the real-VM transport test")
	}
	url := os.Getenv("LEVER_VM_URL")
	if url == "" {
		url = "http://127.0.0.1:8080"
	}

	jr := jail.New(jail.Config{Host: proc.RealRunner{}, Prefix: splitCSV(prefix), UID: "501"})
	j := &JailCurl{Runner: jr, BaseURL: url, Token: func() string { return "probe-token" }}

	status, body, err := j.Do(context.Background(), "GET", "/probe")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200 — the script reached curl but the endpoint did not answer as expected; body=%q", status, body)
	}
	// The echoed Authorization header proves the env prefix survived and the
	// shell expanded $SCION_HUB_TOKEN rather than passing it literally.
	var echoed struct {
		Auth string `json:"auth"`
	}
	if err := json.Unmarshal(body, &echoed); err != nil {
		t.Fatalf("body %q did not decode as JSON: %v", body, err)
	}
	if echoed.Auth != "Bearer probe-token" {
		t.Errorf("echoed Authorization = %q, want %q — the header did not expand correctly",
			echoed.Auth, "Bearer probe-token")
	}
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
