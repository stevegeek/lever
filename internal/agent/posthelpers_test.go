package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// errServer returns a plain-HTTP test server that answers every request with the
// given status code and body. It exists to drive the non-200 error paths of the
// broker POST/GET helpers deterministically, independent of a live broker — the
// helpers' TLS config is unused against an http:// URL.
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

// These tests pin the CURRENT non-200 error contract of each broker helper: the
// exact message prefix, that the status code is surfaced, and — critically — the
// drift the D1 refactor must preserve: Enrol/Renew/ListTools DROP the response
// body while Request/Provision INCLUDE it. The refactor rewrites exactly these
// status-check/error-format lines, so this is the riskiest, previously-unguarded
// surface.

func TestEnrolNon200DropsBody(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "enrol-denied-secret-body")
	_, err := Enrol(context.Background(), srv.URL, mustCA(t).CertPEM(), "ticket", "worker")
	if err == nil {
		t.Fatal("Enrol against a 403 must error")
	}
	if !strings.Contains(err.Error(), "agent: enrol status 403") {
		t.Fatalf("want %q, got: %v", "agent: enrol status 403", err)
	}
	if strings.Contains(err.Error(), "enrol-denied-secret-body") {
		t.Fatalf("Enrol must NOT surface the response body, got: %v", err)
	}
}

func TestRenewNon200DropsBody(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "renew-denied-secret-body")
	id := enrolManager(t, mustCA(t)) // any valid identity: id.Client() must succeed
	_, err := Renew(context.Background(), srv.URL, id)
	if err == nil {
		t.Fatal("Renew against a 403 must error")
	}
	if !strings.Contains(err.Error(), "agent: renew status 403") {
		t.Fatalf("want %q, got: %v", "agent: renew status 403", err)
	}
	if strings.Contains(err.Error(), "renew-denied-secret-body") {
		t.Fatalf("Renew must NOT surface the response body, got: %v", err)
	}
}

func TestRequestNon200IncludesBody(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "policy: may not obtain (tool=db op=read)")
	_, err := Request(context.Background(), srv.URL, srv.Client(), "db", "read", "worker", nil)
	if err == nil {
		t.Fatal("Request against a 403 must error")
	}
	if !strings.Contains(err.Error(), "agent: request status 403") {
		t.Fatalf("want %q, got: %v", "agent: request status 403", err)
	}
	if !strings.Contains(err.Error(), "policy: may not obtain (tool=db op=read)") {
		t.Fatalf("Request must surface the response body, got: %v", err)
	}
}

func TestProvisionNon200IncludesBody(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "policy: not a manager")
	_, err := Provision(context.Background(), srv.URL, srv.Client(), "worker")
	if err == nil {
		t.Fatal("Provision against a 403 must error")
	}
	if !strings.Contains(err.Error(), "agent: provision status 403") {
		t.Fatalf("want %q, got: %v", "agent: provision status 403", err)
	}
	if !strings.Contains(err.Error(), "policy: not a manager") {
		t.Fatalf("Provision must surface the response body, got: %v", err)
	}
}

func TestListToolsNon200DropsBody(t *testing.T) {
	srv := errServer(t, http.StatusForbidden, "tools-denied-secret-body")
	_, err := ListTools(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("ListTools against a 403 must error")
	}
	if !strings.Contains(err.Error(), "agent: list tools status 403") {
		t.Fatalf("want %q, got: %v", "agent: list tools status 403", err)
	}
	if strings.Contains(err.Error(), "tools-denied-secret-body") {
		t.Fatalf("ListTools must NOT surface the response body, got: %v", err)
	}
}
