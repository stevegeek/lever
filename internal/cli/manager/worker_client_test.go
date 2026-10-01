package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/testutil"
)

func TestWorkerCall_postsAndDecodes(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"worker": "worker", "phase": "running"})
	}))
	defer srv.Close()

	// Inject a client + base URL (bypass mTLS bootstrap for the unit test).
	c := httpCaller{client: srv.Client(), baseURL: srv.URL}
	res, err := workerCall(context.Background(), c, "/worker/start",
		map[string]string{"worker": "worker", "task": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/worker/start" || res.Phase != "running" || res.Worker != "worker" {
		t.Fatalf("path=%s body=%s res=%+v", gotPath, gotBody, res)
	}
}

// TestHTTPCaller_surfacesBody proves a non-200 broker response has its body
// (the specific deny reason, since task #4a) included in the returned error,
// mirroring agent/capability.go's Request. Before this, the caller discarded
// the body entirely, so a returned deny reason never reached the caller.
func TestHTTPCaller_surfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy: may not obtain/delegate (tool=db op=read)", http.StatusForbidden)
	}))
	defer srv.Close()

	c := httpCaller{client: srv.Client(), baseURL: srv.URL}
	_, err := workerCall(context.Background(), c, "/worker/start", map[string]string{})
	if err == nil {
		t.Fatal("want error for non-200 response")
	}
	testutil.WantErrContaining(t, err, "policy: may not obtain/delegate") // the body
	if got := httpjson.Status(err); got != http.StatusForbidden {
		t.Fatalf("httpjson.Status(err) = %d, want 403: %v", got, err)
	}
}

// TestMTLSCaller_missingBootstrapOrIdentity pins the production caller's two
// pre-dial failure modes: an unreadable bootstrap, and a bootstrap with no
// identity beside it, each named in the error.
func TestMTLSCaller_missingBootstrapOrIdentity(t *testing.T) {
	dir := t.TempDir()
	c := mtlsCaller{bootstrapPath: filepath.Join(dir, "bootstrap.json"), idDir: filepath.Join(dir, "id")}
	if err := os.WriteFile(c.bootstrapPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := c.Call(context.Background(), "/worker/list", struct{}{}, nil)
	testutil.WantErrContaining(t, err, "bootstrap "+c.bootstrapPath+":")
	if err := os.WriteFile(c.bootstrapPath, []byte(`{"broker_url":"https://127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = c.Call(context.Background(), "/worker/list", struct{}{}, nil)
	testutil.WantErrContaining(t, err, "agent identity not found in "+c.idDir)
}

// TestBootstrapPath_workerTicket: a worker's bootstrap is the ticket
// $LEVER_BOOTSTRAP names (/run/lever/bootstrap.json), never the manager's
// path, where a worker has no file (or a stale copy in its writable tree).
func TestBootstrapPath_workerTicket(t *testing.T) {
	t.Setenv(bootstrapEnv, "")
	if got := bootstrapPath(); got != managerBootstrapPath {
		t.Fatalf("no $LEVER_BOOTSTRAP: %q, want the manager path %q", got, managerBootstrapPath)
	}
	t.Setenv(bootstrapEnv, "/run/lever/bootstrap.json")
	if got := newMTLSCaller().bootstrapPath; got != "/run/lever/bootstrap.json" {
		t.Fatalf("worker context: the caller reads %q, want the ticket /run/lever/bootstrap.json", got)
	}
}

// TestMTLSCaller_readsTheTicketLeverBootstrapNames: in a worker context the
// call gets as far as the identity, which proves the ticket was the bootstrap
// it read.
func TestMTLSCaller_readsTheTicketLeverBootstrapNames(t *testing.T) {
	dir := t.TempDir()
	ticket := filepath.Join(dir, "run", "lever", "bootstrap.json")
	if err := os.MkdirAll(filepath.Dir(ticket), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticket, []byte(`{"broker_url":"https://127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bootstrapEnv, ticket)
	c := newMTLSCaller()
	c.idDir, c.gatewayURL = filepath.Join(dir, "id"), ""
	err := c.Call(context.Background(), "/msg/send", struct{}{}, nil)
	testutil.WantErrContaining(t, err, "agent identity not found in "+c.idDir)
}

// TestMTLSCaller_gatewayWhenTheBootstrapIsUnreadable: with no readable
// bootstrap the call goes through the agent's loopback gateway, and the
// broker's answer through it stands — a refusal as much as a success. A
// bootstrap that is present but broken is an error, never a fallback.
func TestMTLSCaller_gatewayWhenTheBootstrapIsUnreadable(t *testing.T) {
	var gotPath string
	status := http.StatusOK
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if status != http.StatusOK {
			http.Error(w, "worker w is not running", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"worker": "w", "phase": "running"})
	}))
	defer gw.Close()
	dir := t.TempDir()
	c := mtlsCaller{bootstrapPath: filepath.Join(dir, "lever", "bootstrap.json"), idDir: filepath.Join(dir, "id"), gatewayURL: gw.URL}

	res, err := workerCall(context.Background(), c, "/worker/list", struct{}{})
	if err != nil || gotPath != "/worker/list" || res.Phase != "running" {
		t.Fatalf("missing bootstrap: err=%v path=%q res=%+v, want the gateway's answer", err, gotPath, res)
	}

	status = http.StatusConflict
	_, err = workerCall(context.Background(), c, "/msg/send", struct{}{})
	if httpjson.Status(err) != http.StatusConflict {
		t.Fatalf("a refusal through the gateway: %v, want its 409", err)
	}

	gw.Close()
	_, err = workerCall(context.Background(), c, "/msg/send", struct{}{})
	testutil.WantErrContaining(t, err, "bootstrap "+c.bootstrapPath)
	testutil.WantErrContaining(t, err, "the agent gateway at "+gw.URL+" did not answer")

	if err := os.MkdirAll(filepath.Dir(c.bootstrapPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.bootstrapPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath = ""
	_, err = workerCall(context.Background(), c, "/worker/list", struct{}{})
	if err == nil || gotPath != "" {
		t.Fatalf("a broken bootstrap: err=%v gateway path=%q, want an error and no gateway call", err, gotPath)
	}
}
