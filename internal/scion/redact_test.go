package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestSecretSetErrorRedactsToken(t *testing.T) {
	f := exec.NewFakeRunner()
	// Leave "hub secret set" unscripted: FakeRunner returns a non-zero Result
	// with an error for unscripted commands.
	c := New(f, Options{})
	err := c.SecretSet(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-secret-123")
	if err == nil {
		t.Fatal("expected error from failing hub secret set")
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

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"hub", "secret", "set", "K", "V"})
	if !strings.Contains(got, "K") {
		t.Fatalf("redactArgs should keep key visible: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("redactArgs should redact value: %q", got)
	}
	if strings.Contains(got, "V") {
		t.Fatalf("redactArgs leaked value: %q", got)
	}

	if got := redactArgs([]string{"list", "--global"}); got != "list --global" {
		t.Fatalf("non-secret args should render verbatim, got %q", got)
	}
}
