package hubapi

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/jail"
)

// TestJailCurlAgainstRealVM runs the REAL curl script through a REAL jail
// runner against a real HTTP endpoint inside a VM. It is the only test that
// covers argv transport: whether the backend's shell wrapper (`limactl shell`,
// `orb -m`) delivers curlScript intact, single quotes and `%{http_code}` and
// all, and whether the `env K=V` prefix reaches the shell so the Authorization
// header expands.
//
// Unit tests cannot cover that — they stub the runner, so the script never
// meets a real shell. This is exactly where a backend difference would hide.
//
// Skipped unless LEVER_VM_PREFIX is set. Run it as:
//
//	# Lima (start a VM, then serve JSON on :8080 inside it)
//	LEVER_VM_PREFIX='limactl,shell,levercurltest' \
//	  go test ./internal/hubapi/ -run TestJailCurlAgainstRealVM -v
//
//	# OrbStack
//	LEVER_VM_PREFIX='orb,-m,lever-assistant' ...
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

	jr := jail.New(leverexec.RealRunner{}, splitCSV(prefix), "501")
	j := &JailCurl{Runner: jr, BaseURL: url, Token: func() string { return "probe-token" }}

	status, body, err := j.Do(context.Background(), "GET", "/probe")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200 — the script reached curl but the endpoint did not answer as expected; body=%q", status, body)
	}
	// The endpoint echoes the Authorization header it received, which proves the
	// `env K=V` prefix survived AND the shell expanded $SCION_HUB_TOKEN inside
	// the script rather than passing it through literally. Decode rather than
	// compare bytes: the probe server's JSON spacing is not part of the contract.
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
