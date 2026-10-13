package host

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
	"github.com/stevegeek/lever/internal/wire"
)

// buildDepsAgainstFakeBroker points the app's broker admin port at srv (a
// started fake /epoch + /bootstrap server) and returns the live Deps and the
// app buildApplyDeps captured.
func buildDepsAgainstFakeBroker(t *testing.T, srv *httptest.Server) (apply.Deps, *config.App, state.State, string) {
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
	app.Broker.AdminPort = port

	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }
	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	return deps, app, stateFor(p), p
}

// TestStartBrokerReusesMatchingBrokerIdentity: a broker reporting this
// binary's version and config hash is kept. broker.pid is a directory, so the
// restart branch would error in StopBroker; a nil return can only be the
// reuse branch.
func TestStartBrokerReusesMatchingBrokerIdentity(t *testing.T) {
	var epochHits int
	var app *config.App
	var st state.State
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		epochHits++
		id, _ := st.ReadInstanceID()
		_ = json.NewEncoder(w).Encode(broker.EpochAnswer{EpochResponse: wire.EpochResponse{
			Epoch:      1,
			Version:    cli.VersionString(),
			ConfigHash: brokerctl.ConfigHash(app),
		}, InstanceID: id})
	})
	mux.HandleFunc("/bootstrap", func(http.ResponseWriter, *http.Request) {
		t.Error("StartBroker must not hit /bootstrap")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var deps apply.Deps
	deps, app, st, _ = buildDepsAgainstFakeBroker(t, srv)

	if err := os.MkdirAll(st.PID(), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := deps.StartBroker(context.Background()); err != nil {
		t.Fatalf("StartBroker on matching identity must reuse (nil), got: %v", err)
	}
	if epochHits != 1 {
		t.Fatalf("expected exactly one /epoch probe, got %d", epochHits)
	}
}

// TestStartBrokerRestartsOnIdentityMismatch: a broker reporting another
// version is not reused, and the stale broker is stopped before respawning.
// broker.pid is a directory so StopBroker fails, proving the restart branch
// without a real spawn.
func TestStartBrokerRestartsOnIdentityMismatch(t *testing.T) {
	var app *config.App
	var st state.State
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		// This instance's broker, started by an older binary.
		id, _ := st.ReadInstanceID()
		_ = json.NewEncoder(w).Encode(broker.EpochAnswer{EpochResponse: wire.EpochResponse{
			Epoch:      1,
			Version:    "some-older-binary",
			ConfigHash: brokerctl.ConfigHash(app),
		}, InstanceID: id})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var deps apply.Deps
	deps, app, st, _ = buildDepsAgainstFakeBroker(t, srv)

	if err := os.MkdirAll(st.PID(), 0o755); err != nil {
		t.Fatal(err)
	}

	err := deps.StartBroker(context.Background())
	if err == nil {
		t.Fatal("StartBroker on identity mismatch must enter the restart branch (not reuse)")
	}
	// The stop runs before the restart: the error is the stop's, not a spawn's.
	testutil.WantErrContaining(t, err, "stopping the stale broker before restart")
}

// TestMintManagerBootstrapSuccess: the mint composes the ticket from
// /bootstrap, the CA PEM from the state dir, and BrokerURL/AgentCN from the
// app.
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
	// HostAliasV4() is empty for the stub, so the URL uses HostToolAlias().
	wantURL := fmt.Sprintf("https://host.orb.internal:%d", app.EffectiveJailPort())
	if mat.BrokerURL != wantURL {
		t.Errorf("BrokerURL = %q, want %q", mat.BrokerURL, wantURL)
	}
	if mat.AgentCN != app.ManagerCN() {
		t.Errorf("AgentCN = %q, want %q", mat.AgentCN, app.ManagerCN())
	}
}

// TestMintManagerBootstrapLatched: a 403 must map to ErrBootstrapLatched, or
// an idempotent re-apply breaks.
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
	if got := httpjson.Status(err); got != http.StatusInternalServerError {
		t.Fatalf("httpjson.Status(err) = %d, want 500 (the error must carry the status): %v", got, err)
	}
}

// TestBrokerHealthyReturnsOnOK: health succeeds once /epoch answers as this
// instance's broker with this binary's version and this config's hash.
func TestBrokerHealthyReturnsOnOK(t *testing.T) {
	var st state.State
	var app *config.App
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		id, _ := st.ReadInstanceID()
		writeJSONEpoch(w, answer(id, cli.VersionString(), brokerctl.ConfigHash(app)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, a, s, _ := buildDepsAgainstFakeBroker(t, srv)
	app, st = a, s
	if _, err := st.EnsureInstanceID(); err != nil { // Start makes it in a real apply
		t.Fatal(err)
	}

	if err := deps.BrokerHealthy(context.Background()); err != nil {
		t.Fatalf("BrokerHealthy against this instance's broker: %v", err)
	}
}

// A broker that is not the one this apply runs is never adopted: another
// instance's, this instance's with another binary or config, or one without an
// instance id. Health fails at once, naming the port.
func TestBrokerHealthyRefusesAForeignBroker(t *testing.T) {
	var st state.State
	var app *config.App
	var mu sync.Mutex
	var mk func(ours string) broker.EpochAnswer
	mux := http.NewServeMux()
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		id, _ := st.ReadInstanceID()
		mu.Lock()
		a := mk(id)
		mu.Unlock()
		writeJSONEpoch(w, a)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	deps, a, s, _ := buildDepsAgainstFakeBroker(t, srv)
	app, st = a, s
	if _, err := st.EnsureInstanceID(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		mk   func(ours string) broker.EpochAnswer
		want string
	}{
		{"another instance, this binary and config", func(string) broker.EpochAnswer {
			return answer(idOther, cli.VersionString(), brokerctl.ConfigHash(app))
		}, "another lever instance"},
		{"this instance, another binary", func(ours string) broker.EpochAnswer {
			return answer(ours, "0.34.2", brokerctl.ConfigHash(app))
		}, "not as the broker this apply runs"},
		{"this instance, another config", func(ours string) broker.EpochAnswer {
			return answer(ours, cli.VersionString(), "another-config")
		}, "not as the broker this apply runs"},
		{"predates instance ids", func(string) broker.EpochAnswer {
			return answer("", "0.34.2", "another-instance")
		}, "no instance id"},
	} {
		mu.Lock()
		mk = c.mk
		mu.Unlock()
		start := time.Now()
		err := deps.BrokerHealthy(context.Background())
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("127.0.0.1:%d", app.EffectiveAdminPort())) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want the port and %q", c.name, err, c.want)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("%s: took %s: a broker that is not this apply's must fail at once", c.name, time.Since(start))
		}
	}
}

func writeJSONEpoch(w http.ResponseWriter, a broker.EpochAnswer) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

// TestEgressAllowlistCarriesTheLoginPort: the guest's login forwarder reaches
// the provider's host port, so that port must be in the egress allowlist the
// backend turns into iptables ACCEPT rules, or the hub's discovery fetch times
// out and the browser gets a 502. A non-remote instance must not gain it.
func TestEgressAllowlistCarriesTheLoginPort(t *testing.T) {
	build := func(t *testing.T, remote bool) backend.Config {
		t.Helper()
		p := writeTmpConfig(t)
		app, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if remote {
			app.Remote = config.Remote{Enabled: true, BaseURL: "https://demo.tailnet.ts.net"}
		}
		sb := &stubBackend{}
		if _, err := buildApplyDeps(context.Background(), app, p, func(string, string) (backend.Backend, error) { return sb, nil }, inertApplyOpts); err != nil {
			t.Fatalf("buildApplyDeps: %v", err)
		}
		if !sb.up {
			t.Fatal("EnsureUp was never called")
		}
		return sb.upCfg
	}

	off := build(t, false)
	if slices.Contains(off.AllowedPorts, 8447) {
		t.Fatalf("a non-remote instance was granted the login port: %v", off.AllowedPorts)
	}

	on := build(t, true)
	if !slices.Contains(on.AllowedPorts, 8447) {
		t.Fatalf("AllowedPorts = %v, want the login port 8447 — without it the jail drops the forwarder's dial to the host", on.AllowedPorts)
	}
	// Remote access adds exactly one port and widens nothing else.
	if want := append(append([]int{}, off.AllowedPorts...), 8447); !slices.Equal(on.AllowedPorts, want) {
		t.Fatalf("AllowedPorts = %v, want exactly %v — the grant must add one port, not widen the alias", on.AllowedPorts, want)
	}
}

// TestLeverMayClaimTemplate: scion's stock "default" must be claimed, and an
// operator-chosen template must never be replaced.
func TestLeverMayClaimTemplate(t *testing.T) {
	for _, tc := range []struct {
		current string
		want    bool
		why     string
	}{
		{"default", true, "scion's stock default is exactly what ships the placeholder prompt"},
		{"", true, "unset falls back to \"default\" in scion, so the placeholder still applies"},
		{"  default  ", true, "surrounding whitespace is not a deliberate choice"},
		{"lever", false, "already ours — a re-apply must not report a change"},
		{"my-template", false, "an operator's own choice is not lever's to override"},
		{"Default", false, "template names are resolved as directory names, and case matters"},
	} {
		if got := leverMayClaimTemplate(tc.current); got != tc.want {
			t.Errorf("leverMayClaimTemplate(%q) = %v, want %v — %s", tc.current, got, tc.want, tc.why)
		}
	}
}

// TestEnsureScionTelemetryBindsTheConfiguredMode: unset means off (scion's
// default makes every hook wait out OTLP timeouts); scion-default hands the key
// back.
func TestEnsureScionTelemetryBindsTheConfiguredMode(t *testing.T) {
	for _, c := range []struct {
		mode config.ScionTelemetryMode
		off  bool
	}{{"", true}, {config.ScionTelemetryOff, true}, {config.ScionTelemetryScionDefault, false}} {
		sb := &stubBackend{telemetryChanged: true}
		w := &applyWiring{b: sb, app: &config.App{Scion: config.ScionConfig{Telemetry: c.mode}}}
		changed, err := w.ensureScionTelemetry(context.Background())
		if err != nil || !changed {
			t.Fatalf("%q: changed=%v err=%v", c.mode, changed, err)
		}
		if len(sb.telemetryCalls) != 1 || sb.telemetryCalls[0] != c.off {
			t.Fatalf("%q: EnsureScionTelemetry calls = %v, want [%v]", c.mode, sb.telemetryCalls, c.off)
		}
	}
}
