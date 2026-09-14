package scion

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

func TestHubTokenCreateArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "Token: scion_pat_x"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller",
		[]string{"agent:manage", "agent:attach", "project:read"}, "360d"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "hub token create --project lever --name lever-controller --scopes agent:manage,agent:attach,project:read --expires 360d" {
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
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: out})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_tRAqms-gZDFpO6" {
		t.Errorf("token = %q, want %q", tok.Token, "scion_pat_tRAqms-gZDFpO6")
	}
	// The record lever keeps beside the token comes from the same block: the
	// hub's id for the token (what `hub token revoke` takes), the scopes AS
	// THE HUB EXPANDED them, and the expiry.
	if tok.ID != "a8bf56c4-383e-4e6a-ac2c-db7fe509c688" {
		t.Errorf("id = %q", tok.ID)
	}
	if got := strings.Join(tok.Scopes, ","); got != "agent:create,agent:read,project:read" {
		t.Errorf("scopes = %q", got)
	}
	want := time.Date(2026, 10, 7, 10, 8, 14, 0, time.FixedZone("", 2*3600))
	if !tok.ExpiresAt.Equal(want) {
		t.Errorf("expires = %v, want %v", tok.ExpiresAt, want)
	}
}

// The token line is the one hard requirement; the rest of the block is
// best-effort, so a scion that prints less still mints.
func TestHubTokenCreateWithoutExpiryLine(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "Created access token: x\n  ID: 123\n\nToken: scion_pat_a\n"})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_a" || tok.ID != "123" || !tok.ExpiresAt.IsZero() || len(tok.Scopes) != 0 {
		t.Errorf("got %+v", tok)
	}
}

func TestHubTokenCreateOmitsExpiresFlagWhenEmpty(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "Token: scion_pat_x"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); strings.Contains(got, "--expires") {
		t.Errorf("args = %q, want no --expires flag", got)
	}
}

func TestHubTokenRevokeArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "revoked"})
	c := New(f, Options{})
	if err := c.HubTokenRevoke(context.Background(), "/lever", "a8bf56c4"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); got != "hub token revoke a8bf56c4" {
		t.Errorf("args = %q", got)
	}
	if f.Calls[0].Dir != "/lever" {
		t.Errorf("dir = %q, want /lever", f.Calls[0].Dir)
	}
}

func TestHubTokenRevokePropagatesError(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{})
	if err := c.HubTokenRevoke(context.Background(), "/lever", "nope"); err == nil {
		t.Fatal("expected error when the revoke command fails")
	}
}

func TestHubTokenCreateFallsBackToBarePAT(t *testing.T) {
	// No "Token:" label, but a scion_pat_ token is present.
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "created\nscion_pat_bare789\n"})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_bare789" {
		t.Errorf("token = %q, want %q", tok.Token, "scion_pat_bare789")
	}
}

func TestHubTokenCreateNoTokenIsError(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "some noise without a token\n"})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}, ""); err == nil {
		t.Fatal("expected error when output contains no token")
	}
}

func TestHubTokenCreatePropagatesError(t *testing.T) {
	f := proc.NewFakeRunner() // no script -> unscripted error
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "lever-controller", []string{"agent:manage"}, ""); err == nil {
		t.Fatal("expected error when the hub token create command fails")
	}
}
