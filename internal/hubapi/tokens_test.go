package hubapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const patSecret = "scion_pat_SECRETSECRETSECRET"

func TestCreateTokenSendsTheRequestAndReturnsTheSecret(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/auth/tokens": {status: 201, body: `{"token":"` + patSecret + `",
	  "accessToken":{"id":"t-1","name":"lever-controller","prefix":"scion_pat_SEC","projectId":"p-1",
	  "scopes":["agent:manage","project:read"],"revoked":false,"expiresAt":"2027-10-08T00:00:00Z","created":"2026-10-13T00:00:00Z"}}`}}}
	exp := time.Date(2027, 10, 8, 0, 0, 0, 0, time.FixedZone("x", 3600))
	secret, tok, err := (&Client{T: f}).CreateToken(context.Background(), "lever-controller", "p-1", []string{"agent:manage", "project:read"}, exp)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if secret != patSecret {
		t.Fatal("wrong secret returned")
	}
	want := Token{ID: "t-1", Name: "lever-controller", ProjectID: "p-1", Scopes: []string{"agent:manage", "project:read"},
		ExpiresAt: time.Date(2027, 10, 8, 0, 0, 0, 0, time.UTC), Prefix: "scion_pat_SEC"}
	if !reflect.DeepEqual(tok, want) {
		t.Fatalf("token = %+v, want %+v", tok, want)
	}
	got := decodeBody(t, f, "POST /api/v1/auth/tokens")
	wantBody := map[string]any{"name": "lever-controller", "projectId": "p-1",
		"scopes": []any{"agent:manage", "project:read"}, "expiresAt": "2027-10-07T23:00:00Z"}
	if !reflect.DeepEqual(got, wantBody) {
		t.Fatalf("body = %v, want %v", got, wantBody)
	}
}

// Every failure path of a mint must keep the secret out of the error text,
// including an answer that carries it but is otherwise unusable.
func TestCreateTokenNeverPutsTheSecretInAnError(t *testing.T) {
	cases := map[string]reply{
		"non-2xx with token":   {status: 500, body: `{"token":"` + patSecret + `"}`},
		"no accessToken":       {status: 201, body: `{"token":"` + patSecret + `"}`},
		"accessToken no id":    {status: 201, body: `{"token":"` + patSecret + `","accessToken":{}}`},
		"bad accessToken type": {status: 201, body: `{"token":"` + patSecret + `","accessToken":"` + patSecret + `"}`},
		"bad token type":       {status: 201, body: `{"token":["` + patSecret + `"]}`},
		"truncated":            {status: 201, body: `{"token":"` + patSecret},
		"transport":            {err: errors.New("exit status 7")},
	}
	for name, r := range cases {
		f := &fakeDoer{replies: map[string]reply{"POST /api/v1/auth/tokens": r}}
		secret, _, err := (&Client{T: f}).CreateToken(context.Background(), "n", "p", []string{"project:read"}, time.Time{})
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if secret != "" {
			t.Errorf("%s: no secret may be returned with an error", name)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: secret in the error: %v", name, err)
		}
	}
}

func TestCreateTokenKeepsTheHubsReasonWhenItCarriesNoToken(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/auth/tokens": {status: 403, body: `{"error":{"code":"scope_violation","message":"scope not held"}}`}}}
	_, _, err := (&Client{T: f}).CreateToken(context.Background(), "n", "p", []string{"x"}, time.Time{})
	assertAPIStatus(t, err, 403)
	if !strings.Contains(err.Error(), "scope not held") {
		t.Fatalf("error should carry the hub's reason: %v", err)
	}
	if _, has := decodeBody(t, f, "POST /api/v1/auth/tokens")["expiresAt"]; has {
		t.Fatal("a zero expiry is left to the hub's default")
	}
}

func TestListTokens(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/auth/tokens": {status: 200, body: `{"items":[
	  {"id":"t-1","name":"lever-controller","projectId":"p","scopes":["project:read"],"revoked":true},
	  {"id":"t-2","name":"x","projectId":"p","scopes":[],"expiresAt":"2027-01-01T00:00:00Z"}]}`}}}
	got, err := (&Client{T: f}).ListTokens(context.Background())
	if err != nil || len(got) != 2 || !got[0].Revoked || got[1].ExpiresAt.Year() != 2027 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	f.replies["GET /api/v1/auth/tokens"] = reply{status: 200, body: `{"tokens":[]}`}
	if _, err := (&Client{T: f}).ListTokens(context.Background()); err == nil {
		t.Fatal("a list without items must fail")
	}
	f.replies["GET /api/v1/auth/tokens"] = reply{status: 403, body: `{}`}
	_, err = (&Client{T: f}).ListTokens(context.Background())
	assertAPIStatus(t, err, 403)
}

func TestRevokeToken(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"POST /api/v1/auth/tokens/t-1/revoke":  {status: 204},
		"POST /api/v1/auth/tokens/t-2/revoke":  {status: 200, body: `{}`},
		"POST /api/v1/auth/tokens/gone/revoke": {status: 404, body: `{"error":"access token not found"}`},
		"POST /api/v1/auth/tokens/bad/revoke":  {status: 401, body: `{}`},
	}}
	c := &Client{T: f}
	for _, id := range []string{"t-1", "t-2"} {
		if err := c.RevokeToken(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if err := c.RevokeToken(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: want ErrNotFound, got %v", err)
	}
	err := c.RevokeToken(context.Background(), "bad")
	assertAPIStatus(t, err, 401)
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a 401 is not ErrNotFound")
	}
	if err := c.RevokeToken(context.Background(), ""); err == nil {
		t.Fatal("an empty id must fail")
	}
}
