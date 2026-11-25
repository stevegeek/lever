package hubapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSetUserSecret(t *testing.T) {
	key := "PUT /api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN"
	f := &fakeDoer{replies: map[string]reply{key: {status: 200, body: `{"created":true}`}}}
	if err := (&Client{T: f}).SetUserSecret(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-secretvalue"); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(f.bodies[key]), &got); err != nil {
		t.Fatalf("body %q: %v", f.bodies[key], err)
	}
	want := map[string]string{"value": "sk-ant-oat01-secretvalue", "encoding": "raw", "scope": "user",
		"injectionMode": "always", "type": "environment", "target": "CLAUDE_CODE_OAUTH_TOKEN"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body %s = %q, want %q", k, got[k], v)
		}
	}
	if err := AssertionPathAllowed(http.MethodPut, "/api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN"); err != nil {
		t.Fatalf("assertion path refused: %v", err)
	}

	refused := &fakeDoer{replies: map[string]reply{key: {status: 403, body: `{"error":{"code":"forbidden"}}`}}}
	err := (&Client{T: refused}).SetUserSecret(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-secretvalue")
	if err == nil {
		t.Fatal("a refused write must fail")
	}
	if s := err.Error(); strings.Contains(s, "secretvalue") {
		t.Fatalf("the error carries the secret: %q", s)
	}
}
