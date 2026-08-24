package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/httpjson"
)

// errServer returns a plain-HTTP test server that answers every request with the
// given status code and body. It drives the non-200 error paths of the broker
// helpers deterministically, independent of a live broker — the helpers' TLS
// config is unused against an http:// URL.
func errServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustCA generates a throwaway CA for building standalone identities/clients.
func mustCA(t *testing.T) *ca.CA {
	t.Helper()
	c, err := ca.Generate()
	if err != nil {
		t.Fatalf("mustCA: %v", err)
	}
	return c
}

// assertWireError pins the single non-200 error contract shared by every broker
// helper: the helper's "agent: <op>" prefix, a *httpjson.StatusError carrying the
// status code, and the trimmed response body in the message.
func assertWireError(t *testing.T, err error, prefix string, code int, body string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s against a %d must error", prefix, code)
	}
	if !strings.HasPrefix(err.Error(), prefix+": ") {
		t.Fatalf("want prefix %q, got: %v", prefix, err)
	}
	var se *httpjson.StatusError
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("want *httpjson.StatusError with code %d, got: %v", code, err)
	}
	if !strings.Contains(err.Error(), body) {
		t.Fatalf("error must surface the response body %q, got: %v", body, err)
	}
}

func TestEnrolNon200(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "ticket: unknown")
	_, err := Enrol(context.Background(), srv.URL, mustCA(t).CertPEM(), "ticket", "worker")
	assertWireError(t, err, "agent: enrol", 403, "ticket: unknown")
}

func TestRenewNon200(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "renew: denied")
	id := enrolManager(t, mustCA(t)) // any valid identity: id.Client() must succeed
	_, err := Renew(context.Background(), srv.URL, id)
	assertWireError(t, err, "agent: renew", 403, "renew: denied")
}

func TestRequestNon200(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "policy: may not obtain (tool=db op=read)")
	_, err := Request(context.Background(), srv.URL, srv.Client(), "db", "read", "worker", nil)
	assertWireError(t, err, "agent: request", 403, "policy: may not obtain (tool=db op=read)")
}

func TestProvisionNon200(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "policy: not a manager")
	_, err := Provision(context.Background(), srv.URL, srv.Client(), "worker")
	assertWireError(t, err, "agent: provision", 403, "policy: not a manager")
}

func TestListToolsNon200(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "tools: denied")
	_, err := ListTools(context.Background(), srv.URL, srv.Client())
	assertWireError(t, err, "agent: list tools", 403, "tools: denied")
}
