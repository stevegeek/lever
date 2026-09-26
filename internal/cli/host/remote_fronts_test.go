package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/state"
)

// Wiring tests for the issue #38 remote knobs: each fails if the config value
// stops reaching the component that enforces it.

// hostDecision serves one request with the given Host through the handler
// `remote serve` builds for yaml, and returns the gate's audit decision.
func hostDecision(t *testing.T, yaml, host string) remoteproxy.Decision {
	t.Helper()
	app := loadInstance(t, yaml)
	st := state.ForConfig(t.TempDir())
	var mu sync.Mutex
	var decisions []remoteproxy.Decision
	audit := func(l remoteproxy.AuditLine) { mu.Lock(); decisions = append(decisions, l.Decision); mu.Unlock() }
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no jail in this test")
	}
	_, h, err := buildRemoteHandler(app, st, dial, audit)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.Host = host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	mu.Lock()
	defer mu.Unlock()
	if len(decisions) == 0 {
		t.Fatal("no audit line")
	}
	return decisions[0]
}

const remoteBase = "remote:\n  enabled: true\n  base_url: \"https://vm.exe.xyz:8445\"\n"

// The bind address reaches the Host gate (remoteproxy.Config.BindHost) for any
// specific bind, loopback ones included; a wildcard admits none.
func TestRemoteHandlerAdmitsTheBindAddress(t *testing.T) {
	for _, tc := range []struct {
		name, extra, host string
		wantHostOK        bool
	}{
		{"private bind", "  bind: 10.0.0.5\n", "10.0.0.5:8445", true},
		{"non-default loopback bind", "  bind: 127.0.0.2\n", "127.0.0.2:8445", true},
		{"IPv6 loopback bind", "  bind: \"::1\"\n", "[::1]:8445", true},
		{"default bind: another address is a name like any other", "", "10.0.0.5:8445", false},
		{"wildcard bind admits no address", "  bind: 0.0.0.0\n  allow_wildcard_bind: true\n", "10.0.0.5:8445", false},
		{"wildcard is never a Host", "  bind: 0.0.0.0\n  allow_wildcard_bind: true\n", "0.0.0.0:8445", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := hostDecision(t, remoteBase+tc.extra, tc.host)
			if (d != remoteproxy.DecisionDenyHost) != tc.wantHostOK {
				t.Fatalf("Host %q → %s", tc.host, d)
			}
		})
	}
}

// trust_forwarded_host reaches the gate: an IP-literal Host carrying base_url's
// host in X-Forwarded-Host passes with it and is refused without it.
func TestRemoteHandlerWiresTrustForwardedHost(t *testing.T) {
	serve := func(extra string) remoteproxy.Decision {
		app := loadInstance(t, remoteBase+extra)
		var got remoteproxy.Decision
		audit := func(l remoteproxy.AuditLine) {
			if got == "" {
				got = l.Decision
			}
		}
		dial := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no jail") }
		_, h, err := buildRemoteHandler(app, state.ForConfig(t.TempDir()), dial, audit)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "/api/v1/agents", nil)
		req.Host = "10.9.9.9:9000"
		req.Header.Set("X-Forwarded-Host", "vm.exe.xyz:8445")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
		return got
	}
	if d := serve(""); d != remoteproxy.DecisionDenyHost {
		t.Fatalf("flag off: %s, want deny-host", d)
	}
	if d := serve("  trust_forwarded_host: true\n"); d == remoteproxy.DecisionDenyHost {
		t.Fatal("flag on: the forwarded host must be judged")
	}
}

// A non-loopback bind reaches Serve with its acknowledgement: `remote serve`
// actually listens on it. (The wildcard is used so the test runs on any
// host; without AllowNonLoopback, Serve refuses it.)
func TestServeRemoteBindsANonLoopbackAddress(t *testing.T) {
	port, login := freeRemotePort(t), freeRemotePort(t)
	app := loadInstance(t, fmt.Sprintf(remoteBase+"  port: %d\n  login_port: %d\n  bind: 0.0.0.0\n  allow_wildcard_bind: true\n", port, login))
	dir := t.TempDir()
	st := state.ForConfig(dir + "/lever.yaml")
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := remoteproxy.NewProvider(remoteproxy.ProviderConfig{Port: login})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveRemote(ctx, app, st, provider, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
			_ = c.Close()
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("serve exited without binding: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the proxy never started listening")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// It holds the wildcard, not just loopback.
	if ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port)); err == nil {
		_ = ln.Close()
		cancel()
		t.Fatal("the proxy is not bound to 0.0.0.0")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// apply waits for the spawned proxy where it listens.
func TestNewRemoteControllerProbesTheBindAddress(t *testing.T) {
	for extra, want := range map[string]string{
		"":                   "127.0.0.1:8445",
		"  bind: 10.0.0.5\n": "10.0.0.5:8445",
		"  bind: \"::1\"\n":  "[::1]:8445",
		"  bind: \"::\"\n  allow_wildcard_bind: true\n": "[::1]:8445",
	} {
		app := loadInstance(t, remoteBase+extra)
		rc := newRemoteController(app, state.ForConfig(t.TempDir()), "/x/lever.yaml", "lever", nil)
		if got := rc.addr(); got != want {
			t.Errorf("bind %q: controller dials %s, want %s", extra, got, want)
		}
	}
}

// remote status prints the real listen address as the tailscale serve target.
func TestRemoteStatusTailscaleTargetIsTheListenAddress(t *testing.T) {
	for extra, want := range map[string]string{
		"":                    "http://127.0.0.1:8445",
		"  bind: 127.0.0.2\n": "http://127.0.0.2:8445",
		"  bind: \"::1\"\n":   "http://[::1]:8445",
	} {
		dir := t.TempDir()
		p := writeInstanceInto(t, dir, instanceYAML("x", "remote:\n  enabled: true\n  base_url: \"https://mac.tail1234.ts.net\"\n"+extra))
		t.Chdir(dir)
		out, err := clitest.Exec(t, newRemoteStatusCmd(), p)
		if err != nil {
			t.Fatalf("remote status: %v", err)
		}
		if !strings.Contains(out, "tailscale serve --bg --https=443 "+want+"\n") {
			t.Errorf("bind %q: want target %s in:\n%s", extra, want, out)
		}
	}
}
