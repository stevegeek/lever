// Package test holds the live end-to-end scripts; this test keeps the Lima
// e2e's log masking in step with lever's own redaction.
package test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/logging"
)

// Every credential lever's redaction masks, the e2e's mask_secrets masks too:
// it prints the tails of lever's logs and is the backstop if a line escaped
// lever's own masking.
func TestE2EMaskCoversLeversRedaction(t *testing.T) {
	for _, tool := range []string{"bash", "perl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	cases := []struct{ line, secret string }{
		{"podman run -e CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abcdefghijklmnop img", "abcdefghijklmnop"},
		{"podman run --env=SCION_HUB_SECRET_VALUE=opaque-value-1 img", "opaque-value-1"},
		{"podman run -e HUB_URL=http://hub:9810/opaque-value-2 img", "opaque-value-2"},
		{"raw sk-ant-api03-abcdefghijklmnop", "abcdefghijklmnop"},
		{"token ghs_ABCDEFGHIJKLMNOPQRSTUV", "ABCDEFGHIJKLMNOPQRSTUV"},
		{"pat scion_pat_abcdefghijkl in use", "abcdefghijkl"},
		{"token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig-part", "eyJzdWIiOiJ4In0"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz"},
		{"Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"X-Lever-Token: 0123456789abcdef", "0123456789abcdef"},
		{"use bearer abcdefghijklmnopqrstuvwxyz now", "abcdefghijklmnopqrstuvwxyz"},
		{"scion server start --session-secret=hunter2hunter2", "hunter2hunter2"},
		{"scion server start --session-secret VALUE123", "VALUE123"},
		{"x --api-token=abc123", "abc123"},
		{"x --api-password opaque-value-3", "opaque-value-3"},
	}
	var in strings.Builder
	for _, c := range cases {
		if strings.Contains(logging.Redact(c.line), c.secret) {
			t.Fatalf("case %q: lever's redaction leaves %q; fix the case", c.line, c.secret)
		}
		in.WriteString(c.line + "\n")
	}
	in.WriteString("reading --token-file /path/x\n")
	cmd := exec.Command("bash", "-c", `source <(sed -n '/^mask_secrets() {/,/^}/p' lima-e2e.sh) && mask_secrets`)
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mask_secrets: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(cases)+1 {
		t.Fatalf("mask_secrets printed %d lines, want %d:\n%s", len(lines), len(cases)+1, out)
	}
	for i, c := range cases {
		if strings.Contains(lines[i], c.secret) {
			t.Errorf("mask_secrets leaves %q in %q", c.secret, lines[i])
		}
	}
	if got := lines[len(cases)]; got != "reading --token-file /path/x" {
		t.Errorf("a path-valued flag was masked: %q", got)
	}
}
