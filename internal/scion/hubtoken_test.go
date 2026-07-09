package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestHubTokenCreateArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "Token: scion_pat_x"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller",
		[]string{"agent:manage", "agent:attach", "project:read"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "hub token create --project lever --name lever-controller --scopes agent:manage,agent:attach,project:read" {
		t.Errorf("args = %q", got)
	}
	if f.Calls[0].Dir != "/lever" {
		t.Errorf("dir = %q, want /lever (project context for token create)", f.Calls[0].Dir)
	}
}

func TestHubTokenCreateParsesTokenLine(t *testing.T) {
	// scion prints a human-readable block; the PAT is on a "Token:" line.
	out := `Created access token: lever-controller
  ID:      a8bf56c4-383e-4e6a-ac2c-db7fe509c688
  Project:   lever (4831d902-d02f-4102-a3ee-ce1fe0ac6e28)
  Scopes:  agent:create, agent:read, project:read
  Expires: 2026-10-07T10:08:14+02:00

Token: scion_pat_tRAqms-gZDFpO6

This token will not be shown again. Store it securely.`
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: out})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"})
	if err != nil {
		t.Fatal(err)
	}
	if tok != "scion_pat_tRAqms-gZDFpO6" {
		t.Errorf("token = %q, want %q", tok, "scion_pat_tRAqms-gZDFpO6")
	}
}

func TestHubTokenCreateFallsBackToBarePAT(t *testing.T) {
	// No "Token:" label, but a scion_pat_ token is present.
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "created\nscion_pat_bare789\n"})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"})
	if err != nil {
		t.Fatal(err)
	}
	if tok != "scion_pat_bare789" {
		t.Errorf("token = %q, want %q", tok, "scion_pat_bare789")
	}
}

func TestHubTokenCreateNoTokenIsError(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "some noise without a token\n"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}); err == nil {
		t.Fatal("expected error when output contains no token")
	}
}

func TestHubTokenCreatePropagatesError(t *testing.T) {
	f := exec.NewFakeRunner() // no script -> unscripted error
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}); err == nil {
		t.Fatal("expected error when the hub token create command fails")
	}
}
