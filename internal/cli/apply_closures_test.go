package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
)

// buildDepsAgainstFakeBroker loads writeTmpConfig's app, points its broker
// admin port at srv (a fake /epoch + /bootstrap httptest server), and returns
// the live Deps so StartBroker/BrokerHealthy/MintManagerBootstrap dial the
// fake instead of a real broker. srv MUST already be started (its port is read
// from srv.URL). The returned app is the one buildApplyDeps captured, so its
// EffectiveJailPort()/ManagerCN()/ConfigHash feed the closures under test.
func buildDepsAgainstFakeBroker(t *testing.T, srv *httptest.Server) (apply.Deps, *config.App, brokerctl.State, string) {
	t.Helper()
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	// buildApplyDeps computes adminURL from app.EffectiveAdminPort(); overriding
	// it to the fake server's loopback port makes the closures dial the fake.
	app.Broker.AdminPort = port

	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }
	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	return deps, app, stateFor(p), p
}

// TestStartBrokerReusesMatchingBrokerIdentity proves the #19 broker-reuse
// shortcut: when a running broker's /epoch reports THIS binary's version and
// THIS config's hash, StartBroker keeps it (returns nil) without restarting.
// broker.pid is seeded AS A DIRECTORY so that IF StartBroker had wrongly taken
// the restart branch, StopBroker would fail reading it and StartBroker would
// error — so a nil return here can only be the reuse branch (which never
// touches the pid file), not a silent fall-through to spawn.
func TestStartBrokerReusesMatchingBrokerIdentity(t *testing.T) {
	var epochHits int
	var app *config.App
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		epochHits++
		_ = json.NewEncoder(w).Encode(broker.EpochResponse{
			Epoch:      1,
			Version:    versionString(),
			ConfigHash: brokerctl.ConfigHash(app),
		})
	})
	mux.HandleFunc("/bootstrap", func(http.ResponseWriter, *http.Request) {
		t.Error("StartBroker must not hit /bootstrap")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var deps apply.Deps
	var state brokerctl.State
	deps, app, state, _ = buildDepsAgainstFakeBroker(t, srv)

	// Seed broker.pid as a directory: the restart branch reads it via StopBroker
	// and would error; the reuse branch never touches it.
	if err := os.MkdirAll(state.PID(), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := deps.StartBroker(context.Background()); err != nil {
		t.Fatalf("StartBroker on matching identity must reuse (nil), got: %v", err)
	}
	if epochHits != 1 {
		t.Fatalf("expected exactly one /epoch probe, got %d", epochHits)
	}
}

// TestStartBrokerRestartsOnIdentityMismatch proves the negative half of #19:
// when /epoch reports a DIFFERENT version (a broker predating this binary, or a
// changed tool config), StartBroker refuses to reuse it and enters the restart
// branch, which stops the stale broker BEFORE re-spawning. broker.pid is a
// directory so StopBroker fails there — surfacing the restart-branch entry as a
// deterministic error WITHOUT the real `lever broker serve` spawn that would
// otherwise follow (and without which this branch is untestable in-process).
func TestStartBrokerRestartsOnIdentityMismatch(t *testing.T) {
	var app *config.App
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(broker.EpochResponse{
			Epoch:      1,
			Version:    "some-older-binary",
			ConfigHash: brokerctl.ConfigHash(app),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var deps apply.Deps
	var state brokerctl.State
	deps, app, state, _ = buildDepsAgainstFakeBroker(t, srv)

	if err := os.MkdirAll(state.PID(), 0o755); err != nil {
		t.Fatal(err)
	}

	err := deps.StartBroker(context.Background())
	if err == nil {
		t.Fatal("StartBroker on identity mismatch must enter the restart branch (not reuse)")
	}
	if !strings.Contains(err.Error(), "stopping the stale broker before restart") {
		t.Fatalf("mismatch must stop the stale broker BEFORE restart; got err: %v", err)
	}
}

// TestMintManagerBootstrapSuccess proves the mint closure composes the full
// BootstrapMaterial: the ticket from /bootstrap's JSON, the CA PEM read from
// the state dir, and BrokerURL/AgentCN derived from the app (jail port +
// manager CN). This is the payoff the E2 refactor claims — the closure body had
// no fast-suite coverage before.
func TestMintManagerBootstrapSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("/bootstrap method = %q, want POST", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "tkt-abc-123"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, app, state, _ := buildDepsAgainstFakeBroker(t, srv)

	// The mint closure reads the CA PEM from state.CACert(); seed it.
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const caPEM = "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(state.CACert(), []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}

	mat, err := deps.MintManagerBootstrap(context.Background())
	if err != nil {
		t.Fatalf("MintManagerBootstrap: %v", err)
	}
	if mat.Ticket != "tkt-abc-123" {
		t.Errorf("Ticket = %q, want %q", mat.Ticket, "tkt-abc-123")
	}
	if mat.BrokerCA != caPEM {
		t.Errorf("BrokerCA = %q, want the seeded CA PEM", mat.BrokerCA)
	}
	// HostToolAlias() ("host.orb.internal" for the stub) since HostAliasV4() is empty.
	wantURL := fmt.Sprintf("https://host.orb.internal:%d", app.EffectiveJailPort())
	if mat.BrokerURL != wantURL {
		t.Errorf("BrokerURL = %q, want %q", mat.BrokerURL, wantURL)
	}
	if mat.AgentCN != app.ManagerCN() {
		t.Errorf("AgentCN = %q, want %q", mat.AgentCN, app.ManagerCN())
	}
}

// TestMintManagerBootstrapLatched proves the 403 -> ErrBootstrapLatched mapping
// (a spent single-use latch on an idempotent re-apply) survives exactly — a
// transcription slip here would compile and silently break re-apply.
func TestMintManagerBootstrapLatched(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, _, _, _ := buildDepsAgainstFakeBroker(t, srv)

	_, err := deps.MintManagerBootstrap(context.Background())
	if !errors.Is(err, apply.ErrBootstrapLatched) {
		t.Fatalf("403 from /bootstrap must map to apply.ErrBootstrapLatched, got: %v", err)
	}
}

// TestMintManagerBootstrapNon200 proves a non-200/non-403 status is a hard
// error (not a latch and not a silent success).
func TestMintManagerBootstrapNon200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, _, _, _ := buildDepsAgainstFakeBroker(t, srv)

	_, err := deps.MintManagerBootstrap(context.Background())
	if err == nil {
		t.Fatal("non-200 /bootstrap must error")
	}
	if errors.Is(err, apply.ErrBootstrapLatched) {
		t.Fatal("a 500 must NOT be treated as a spent latch")
	}
	if !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("error should name the status code, got: %v", err)
	}
}

// TestBrokerHealthyReturnsOnOK proves the health poll succeeds as soon as
// /epoch answers 200.
func TestBrokerHealthyReturnsOnOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, _, _, _ := buildDepsAgainstFakeBroker(t, srv)

	if err := deps.BrokerHealthy(context.Background()); err != nil {
		t.Fatalf("BrokerHealthy against a 200 /epoch: %v", err)
	}
}
