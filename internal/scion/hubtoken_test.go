package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestHubTokenCreateArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "sometoken"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), []string{"agent:manage", "agent:attach", "project:read"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "hub token create --scopes agent:manage,agent:attach,project:read" {
		t.Errorf("args = %q", got)
	}
}

func TestHubTokenCreateParsesBareToken(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "  pat-abc123  \n"})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), []string{"agent:manage"})
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-abc123" {
		t.Errorf("token = %q, want %q", tok, "pat-abc123")
	}
}

func TestHubTokenCreateParsesJSONToken(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: `{"token":"pat-json456"}`})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), []string{"agent:manage"})
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-json456" {
		t.Errorf("token = %q, want %q", tok, "pat-json456")
	}
}

func TestHubTokenCreatePropagatesError(t *testing.T) {
	f := exec.NewFakeRunner() // no script -> unscripted error
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), []string{"agent:manage"}); err == nil {
		t.Fatal("expected error when the hub token create command fails")
	}
}
