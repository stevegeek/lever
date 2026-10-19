package host

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/servicehost"
	"github.com/stevegeek/lever/internal/state"
)

// doctorServiceHost serves tools (name → allow) behind pinned mutual TLS and
// returns its address and server fingerprint. client is the fingerprint it
// trusts.
func doctorServiceHost(t *testing.T, client servicehost.Fingerprint, tools map[string][]string) (string, servicehost.Fingerprint) {
	t.Helper()
	c, k, err := servicehost.GenerateSelfSigned("sh", x509.ExtKeyUsageServerAuth)
	if err != nil {
		t.Fatal(err)
	}
	cert, fp, err := servicehost.LoadKeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &servicehost.Config{Listen: "127.0.0.1:1", ClientFingerprint: string(client)}
	for name, allow := range tools {
		cfg.Tools = append(cfg.Tools, servicehost.Tool{Name: name, Upstream: "http://127.0.0.1:1/", Allow: allow})
	}
	srv, err := servicehost.NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "t")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = servicehost.ServerTLSConfig(cert, client)
	ts.Config.ErrorLog = slog.NewLogLogger(slog.New(slog.NewTextHandler(io.Discard, nil)).Handler(), slog.LevelError)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "https://"), fp
}

func TestCheckServiceHost(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	clientFP, _, err := servicehost.EnsureKeyPair(st.ServiceHostClientCert("laptop"), st.ServiceHostClientKey("laptop"), "b", x509.ExtKeyUsageClientAuth, false)
	if err != nil {
		t.Fatal(err)
	}
	app := func(addr string, fp servicehost.Fingerprint, ops ...string) *config.App {
		tool := config.Tool{Name: "mail", Host: "laptop", External: true}
		for _, o := range ops {
			tool.Operations = append(tool.Operations, config.Op{Name: o})
		}
		return &config.App{Broker: config.Broker{
			ServiceHosts: []config.ServiceHost{{Name: "laptop", Address: addr, Fingerprint: string(fp)}},
			Tools:        []config.Tool{tool},
		}}
	}
	ctx := context.Background()
	row := func(a *config.App, s state.State) checkResult {
		rows := checkServiceHosts(ctx, a, s, servicehost.Probe)
		if len(rows) != 1 || rows[0].name != "service host laptop" {
			t.Fatalf("rows = %+v", rows)
		}
		return rows[0]
	}

	addr, fp := doctorServiceHost(t, clientFP, map[string][]string{"mail": {"search"}, "notes": {"*"}})
	if r := row(app(addr, fp, "search"), st); !r.ok || r.fix != "" || !strings.Contains(r.detail, "mutual TLS ok") {
		t.Fatalf("healthy: %+v", r)
	}
	if r := row(app(addr, fp, "search", "delete"), st); !r.ok || r.fix == "" || !strings.Contains(r.detail, "mail.delete") {
		t.Fatalf("outside the ceiling should warn: %+v", r)
	}
	other, _ := doctorServiceHost(t, clientFP, map[string][]string{"notes": {"*"}})
	if r := row(app(other, fp, "search"), st); r.ok {
		// other has its own key, so the pin fails first.
		t.Fatalf("other host with our pin: %+v", r)
	}
	otherAddr, otherFP := doctorServiceHost(t, clientFP, map[string][]string{"notes": {"*"}})
	if r := row(app(otherAddr, otherFP, "search"), st); r.ok || !strings.Contains(r.detail, "not served there: mail") {
		t.Fatalf("missing tool: %+v", r)
	}
	if r := row(app(addr, otherFP, "search"), st); r.ok || !strings.Contains(r.detail, "not the pinned one") {
		t.Fatalf("pin mismatch: %+v", r)
	}
	strangerAddr, strangerFP := doctorServiceHost(t, servicehost.Fingerprint("sha256:"+strings.Repeat("0", 64)), map[string][]string{"mail": {"*"}})
	if r := row(app(strangerAddr, strangerFP, "search"), st); r.ok || !strings.Contains(r.fix, string(clientFP)) {
		t.Fatalf("refused client key: %+v", r)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	downAddr := strings.TrimPrefix(down.URL, "http://")
	down.Close()
	if r := row(app(downAddr, fp, "search"), st); r.ok || !strings.Contains(r.detail, "unreachable") {
		t.Fatalf("down: %+v", r)
	}
	if r := row(app(addr, fp, "search"), state.State{Dir: t.TempDir()}); r.ok || !strings.Contains(r.detail, "not paired") {
		t.Fatalf("unpaired: %+v", r)
	}
	unused := app(addr, fp)
	unused.Broker.Tools = nil
	if r := row(unused, st); !r.ok || r.fix == "" {
		t.Fatalf("unused host should warn: %+v", r)
	}
}
