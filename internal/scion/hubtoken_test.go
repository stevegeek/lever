package scion

import (
	"context"
	"fmt"
	"slices"
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
	if got != "hub token create --project lever --name lever-controller --scopes agent:manage,agent:attach,project:read --expires 360d --non-interactive --json" {
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
	// Scopes are as the hub expanded them.
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
// best-effort, so a scion that prints less still mints. A missing expiry is
// the lower bound of scion's 90-day default, never zero.
func TestHubTokenCreateWithoutExpiryLine(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "Created access token: x\n  ID: 123\n\nToken: scion_pat_a\n"})
	c := New(f, Options{})
	before := time.Now()
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_a" || tok.ID != "123" || len(tok.Scopes) != 0 {
		t.Errorf("got %+v", tok)
	}
	if lo, hi := before.Add(scionDefaultTokenLifetime), time.Now().Add(scionDefaultTokenLifetime); tok.ExpiresAt.Before(lo) || tok.ExpiresAt.After(hi) {
		t.Errorf("expires = %v, want scion's default from the mint start (%v..%v)", tok.ExpiresAt, lo, hi)
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
	if got := strings.Join(f.Calls[0].Args, " "); got != "hub token revoke a8bf56c4 --non-interactive" {
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

// newPinTokenJSON is scion hub token create --json as b41cd6fe prints it
// (token value shortened).
const newPinTokenJSON = `{
  "token": "scion_pat_GxFdTOuBH0tNxyz",
  "accessToken": {
    "id": "92101dbe-b8bc-493e-9a29-e926ab763aaa",
    "name": "t1",
    "prefix": "scion_pat_GxFdTOuBH0tN",
    "projectId": "2ff931c4-bebb-489c-a9b0-fd39a1cd63a8",
    "boundary": {"kind": "project", "projectId": "2ff931c4-bebb-489c-a9b0-fd39a1cd63a8"},
    "scopes": ["agent:create", "agent:lifecycle", "agent:attach", "project:read", "inbox:read"],
    "revoked": false,
    "expiresAt": "2027-10-08T20:09:22.572014457Z",
    "created": "2026-10-13T20:09:22.626445912Z"
  }
}
`

func TestHubTokenCreateReadsJSON(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub token create", proc.Result{Stdout: newPinTokenJSON,
		Stderr: " WARNING: Development authentication enabled - not for production use \n"})
	c := New(f, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "t1", []string{"agent:manage"}, "360d")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_GxFdTOuBH0tNxyz" || tok.ID != "92101dbe-b8bc-493e-9a29-e926ab763aaa" {
		t.Errorf("got %+v", tok)
	}
	if got := strings.Join(tok.Scopes, ","); got != "agent:create,agent:lifecycle,agent:attach,project:read,inbox:read" {
		t.Errorf("scopes = %q", got)
	}
	if want := time.Date(2027, 10, 8, 20, 9, 22, 572014457, time.UTC); !tok.ExpiresAt.Equal(want) {
		t.Errorf("expires = %v, want %v", tok.ExpiresAt, want)
	}
	if len(f.Calls) != 1 {
		t.Errorf("calls = %d, want one mint", len(f.Calls))
	}
}

// jsonFlagRefusingRunner answers token create the way a scion without --json
// on that command does: cobra refuses the flag before anything runs.
type jsonFlagRefusingRunner struct{ *proc.FakeRunner }

func (r *jsonFlagRefusingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && slices.Contains(args, "--json") {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Code: 1, Stderr: "Error: unknown flag: --json\n"}, fmt.Errorf("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// On a scion without --json (e0f61f2d) the mint is retried once without it and
// the text block read; the refused call minted nothing.
func TestHubTokenCreateFallsBackToTextWithoutJSONFlag(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub token create", proc.Result{Stdout: "Created access token: x\n  ID:      abc\n" +
		"  Scopes:  agent:read\n  Expires: 2027-09-09T10:08:14Z\n\nToken: scion_pat_old\n"})
	c := New(&jsonFlagRefusingRunner{FakeRunner: f}, Options{})
	tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "360d")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "scion_pat_old" || tok.ID != "abc" || tok.ExpiresAt.Year() != 2027 {
		t.Errorf("got %+v", tok)
	}
	if len(f.Calls) != 2 || slices.Contains(f.Calls[1].Args, "--json") {
		t.Fatalf("calls = %+v, want the --json try then one call without it", f.Calls)
	}
}

// Any other failure is not retried: the second call could mint a second
// token.
func TestHubTokenCreateDoesNotRetryOtherFailures(t *testing.T) {
	f := proc.NewFakeRunner() // unscripted: every call fails
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "360d"); err == nil {
		t.Fatal("want the failure")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
}

// An expiry scion prints but lever cannot read (the text block's local time,
// or a JSON value that is not RFC 3339) is replaced by the lower bound from
// --expires, so the token is still renewed.
func TestHubTokenCreateUnreadableExpiryIsALowerBound(t *testing.T) {
	for _, out := range []string{
		"Created access token: x\n  ID: 1\n  Expires: 2027-10-08 22:09:22 CEST\n\nToken: scion_pat_t\n",
		`{"token": "scion_pat_t", "accessToken": {"id": "1", "expiresAt": "next year"}}`,
		`{"token": "scion_pat_t", "accessToken": {"id": "1"}}`,
	} {
		f := proc.NewFakeRunner()
		f.Script("scion hub token create", proc.Result{Stdout: out})
		c := New(f, Options{})
		before := time.Now()
		tok, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "360d")
		if err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		lo, hi := before.Add(360*24*time.Hour), time.Now().Add(360*24*time.Hour)
		if tok.Token != "scion_pat_t" || tok.ExpiresAt.Before(lo) || tok.ExpiresAt.After(hi) {
			t.Errorf("%q: got %+v, want expiry in %v..%v", out, tok, lo, hi)
		}
	}
}

func TestHubTokenCreateJSONWithoutTokenIsError(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub token create", proc.Result{Stdout: `{"accessToken": {"id": "1"}}`})
	c := New(f, Options{})
	if _, err := c.HubTokenCreate(context.Background(), "/lever", "lever", "x", []string{"agent:read"}, "360d"); err == nil {
		t.Fatal("want an error for JSON without a token")
	}
}

func TestExpiryLowerBound(t *testing.T) {
	start := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, tc := range []struct {
		expires string
		want    time.Time
	}{
		{"", start.Add(90 * day)},
		{"360d", start.Add(360 * day)},
		{"1y", start.Add(365 * day)},
		{"12h", start.Add(12 * time.Hour)},
		{"90m", start.Add(90 * time.Minute)},
		{"2027-01-02T03:04:05Z", time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"30x", time.Time{}},
		{"d", time.Time{}},
		{"-5d", time.Time{}},
		{"1.5d", time.Time{}},
		{"99999999999999y", time.Time{}},
	} {
		if got := expiryLowerBound(tc.expires, start); !got.Equal(tc.want) {
			t.Errorf("expiryLowerBound(%q) = %v, want %v", tc.expires, got, tc.want)
		}
	}
}
