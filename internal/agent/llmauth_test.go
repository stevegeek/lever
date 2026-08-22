package agent

import (
	"strings"
	"testing"
)

// TestHarnessEnvOverlayPinsItsKeys pins the exact key set. Boot and the renew
// sidecar both call this builder and MUST write identical keys — a key added at
// one call site instead of here would be written at first enrol and then
// dropped by the next cert rotation (the settings writer merges, so the loss is
// silent). Update this test deliberately when the contract changes.
func TestHarnessEnvOverlayPinsItsKeys(t *testing.T) {
	got := HarnessEnvOverlay("/home/scion/.lever-id")
	want := map[string]string{
		"CLAUDE_CODE_CLIENT_CERT":              "/home/scion/.lever-id/agent.crt",
		"CLAUDE_CODE_CLIENT_KEY":               "/home/scion/.lever-id/agent.key",
		"NODE_EXTRA_CA_CERTS":                  "/home/scion/.lever-id/ca.crt",
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1",
	}
	if len(got) != len(want) {
		t.Fatalf("key set changed: got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Paths only, never key bytes: every identity value must be under idDir.
	for k, v := range got {
		if strings.Contains(k, "CERT") || strings.Contains(k, "KEY") || strings.Contains(k, "CA_CERTS") {
			if !strings.HasPrefix(v, "/home/scion/.lever-id/") {
				t.Errorf("%s = %q is not a path under the id dir", k, v)
			}
		}
	}
}

// TestHarnessEnvOverlayForcesClassicRenderer states WHY the renderer flag is in
// the overlay at all, so a later reader does not drop it as unrelated to
// identity. Every route to a lever agent is a PTY onto the container's tmux
// (`lever attach`, or scion's web terminal over `lever remote`), and Claude
// Code's default fullscreen renderer draws on the alternate screen, which has
// no scrollback. The operator cannot set this from inside the session either:
// `/tui default` needs a relaunch, and Claude Code refuses to relaunch a
// session started with a --system-prompt replacement — which is exactly what
// scion's harness passes.
func TestHarnessEnvOverlayForcesClassicRenderer(t *testing.T) {
	if got := HarnessEnvOverlay("/id")["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"]; got != "1" {
		t.Fatalf("CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN = %q, want \"1\" — a lever agent is only ever reached through a PTY, where fullscreen rendering has no scrollback", got)
	}
}
