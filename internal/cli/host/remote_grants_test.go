package host

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/state"
)

const grantsUsers = "  allowed_users:\n    - op@example.com\n    - { login: c1@example.com, tier: contact, agents: [demo] }\n"

// A proxy started under one config admits a login only while the config apply
// last fenced grants it the same: removing, re-tiering or narrowing a login
// refuses it at once, the others keep working, and a missing or unreadable
// file refuses everyone.
func TestRemoteLoginCurrentFollowsTheFence(t *testing.T) {
	proxyApp := loadInstance(t, remoteBase+grantsUsers)
	st := state.ForConfig(t.TempDir())
	current := remoteLoginCurrent(proxyApp, st)
	if err := current("op@example.com"); err == nil {
		t.Fatal("no fence written yet: want a refusal")
	}
	fence := func(cfg string) {
		t.Helper()
		if err := fenceRemoteLogins(loadInstance(t, cfg), st, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	fence(remoteBase + grantsUsers)
	for _, l := range []string{"op@example.com", "c1@example.com"} {
		if err := current(l); err != nil {
			t.Fatalf("%s under the same config: %v", l, err)
		}
	}
	if err := current("stranger@example.com"); err == nil {
		t.Fatal("a login the proxy was not started with")
	}
	for name, cfg := range map[string]string{
		"removed":   "  allowed_users:\n    - op@example.com\n",
		"re-tiered": "  allowed_users:\n    - op@example.com\n    - c1@example.com\n",
	} {
		fence(remoteBase + cfg)
		if err := current("c1@example.com"); err == nil {
			t.Errorf("%s: the contact is still admitted", name)
		}
		if err := current("op@example.com"); err != nil {
			t.Errorf("%s: the unchanged operator is refused: %v", name, err)
		}
	}
	fence("remote:\n  enabled: false\n  base_url: \"https://vm.exe.xyz:8445\"\n" + grantsUsers)
	if err := current("op@example.com"); err == nil {
		t.Fatal("remote access off: want a refusal")
	}
	fence(remoteBase + grantsUsers)
	if err := os.WriteFile(st.RemoteGrantsFile(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := current("op@example.com"); err == nil {
		t.Fatal("an unreadable fence: want a refusal")
	}
}

func TestFenceRemoteLoginsStopsTheProxyWhenRemoteIsOff(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	stops := 0
	stop := func() error { stops++; return nil }
	if err := fenceRemoteLogins(loadInstance(t, remoteBase+grantsUsers), st, stop); err != nil || stops != 0 {
		t.Fatalf("on: err %v stops %d", err, stops)
	}
	if err := os.WriteFile(st.RemoteStamp(), []byte("v\nh\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fenceRemoteLogins(loadInstance(t, remoteBase+grantsUsers), st, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.RemoteStamp()); err != nil {
		t.Fatalf("remote on: the stamp must stay: %v", err)
	}
	off := loadInstance(t, "remote:\n  enabled: false\n  base_url: \"https://vm.exe.xyz:8445\"\n")
	if err := fenceRemoteLogins(off, st, stop); err != nil || stops != 1 {
		t.Fatalf("off: err %v stops %d", err, stops)
	}
	// Else re-enabling remote access lets up restart the proxy before an apply.
	if _, err := os.Stat(st.RemoteStamp()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("remote off: the stamp must go, stat = %v", err)
	}
	if g, err := st.LoadRemoteGrants(); err != nil || g.Enabled || len(g.Logins) != 0 {
		t.Fatalf("off fence %+v %v", g, err)
	}
	if err := fenceRemoteLogins(off, st, func() error { return errors.New("boom") }); err == nil {
		t.Fatal("a failed stop must fail the fence")
	}
}

// The handler `remote serve` builds asks the fence on every request.
func TestRemoteHandlerRefusesAFencedOffLogin(t *testing.T) {
	app := loadInstance(t, remoteBase+grantsUsers)
	st := state.ForConfig(t.TempDir())
	if err := fenceRemoteLogins(loadInstance(t, remoteBase+"  allowed_users:\n    - op@example.com\n"), st, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	var got remoteproxy.Decision
	audit := func(l remoteproxy.AuditLine) {
		if got == "" {
			got = l.Decision
		}
	}
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no jail") }
	h, _, err := buildRemoteHandler(app, st, "", dial, remoteHubLogin{}, audit, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.Host = "vm.exe.xyz:8445"
	req.Header.Set("Tailscale-User-Login", "c1@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req.WithContext(ctx))
	if got != remoteproxy.DecisionDenyRevoked || rw.Code != 403 {
		t.Fatalf("decision %s status %d, want %s 403", got, rw.Code, remoteproxy.DecisionDenyRevoked)
	}
}
