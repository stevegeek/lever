package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestSecretSetErrorRedactsToken(t *testing.T) {
	f := exec.NewFakeRunner()
	// Leave the command unscripted: FakeRunner returns a non-zero Result
	// with an error for unscripted commands.
	c := New(f, Options{})
	err := c.SecretSet(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-secret-123")
	if err == nil {
		t.Fatal("expected error from a failing secret write")
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-secret-123") {
		t.Fatalf("error leaked raw secret: %q", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Fatalf("error should contain redaction marker ***: %q", msg)
	}
	if !strings.Contains(msg, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("error should keep the key name visible: %q", msg)
	}
}

// TestSecretSetScrubsFlagLookingValue covers what position-based masking cannot:
// a value starting with "-" is indistinguishable from a flag, so redactArgs
// masks the key instead and cobra echoes the value back in its own error.
func TestSecretSetScrubsFlagLookingValue(t *testing.T) {
	const secret = "-leading-dash-secret-value"
	f := exec.NewFakeRunner()
	c := New(f, Options{})
	err := c.SecretSet(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", secret)
	if err == nil {
		t.Fatal("expected an error from an unscripted command")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked a flag-shaped secret: %q", err)
	}
}

func TestRedactArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"secret set", []string{"hub", "secret", "set", "K", "V"}, "hub secret set K ***"},
		// The shape SecretSet actually emits. Flags must stay visible.
		{"env set --secret", []string{"hub", "env", "set", "--secret", "--always", "K", "V"},
			"hub env set --secret --always K ***"},
		// Base64 padding must not be mistaken for a KEY=VALUE separator.
		{"padded value", []string{"hub", "env", "set", "--secret", "K", "c2VjcmV0=="},
			"hub env set --secret K ***"},
		{"KEY=VALUE form", []string{"hub", "env", "set", "--secret", "K=V"},
			"hub env set --secret K=***"},
		// Non-secret env writes stay legible: the value is not a credential.
		{"env set plain", []string{"hub", "env", "set", "--project", "--always", "K=V"},
			"hub env set --project --always K=V"},
		{"unrelated", []string{"list", "--global"}, "list --global"},
	}
	for _, tc := range cases {
		if got := redactArgs(tc.args); got != tc.want {
			t.Errorf("%s: redactArgs = %q, want %q", tc.name, got, tc.want)
		}
	}
}
