package brokerctl

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// TestRemoteConfigHashTracksRemoteOnly: the hash must move when anything the
// PROXY reads changes, and must NOT move for config the proxy never reads —
// otherwise every unrelated edit would bounce the proxy and drop live sessions.
func TestRemoteConfigHashTracksRemoteOnly(t *testing.T) {
	base := &config.App{Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	h := RemoteConfigHash(base)

	same := &config.App{Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	if RemoteConfigHash(same) != h {
		t.Error("identical remote config must hash identically, or every apply restarts the proxy")
	}

	for name, mutate := range map[string]func(*config.App){
		"allowed_users": func(a *config.App) { a.Remote.AllowedUsers = []string{"me@example.com"} },
		"base_url":      func(a *config.App) { a.Remote.BaseURL = "https://other.ts.net" },
		"port":          func(a *config.App) { a.Remote.Port = 9445 },
		"login_port":    func(a *config.App) { a.Remote.LoginPort = 8449 },
		"enabled":       func(a *config.App) { a.Remote.Enabled = false },
	} {
		changed := &config.App{Remote: base.Remote}
		mutate(changed)
		if RemoteConfigHash(changed) == h {
			t.Errorf("changing %s did not change the hash — the running proxy would keep the old value", name)
		}
	}

	// Broker/worker config is not the proxy's to care about.
	unrelated := &config.App{Remote: base.Remote, Workers: []config.Worker{{Name: "w", Dir: "d"}}}
	if RemoteConfigHash(unrelated) != h {
		t.Error("an unrelated config change must NOT bounce the proxy")
	}
}

// TestRemoteConfigHashCoversWhatTheProxyCaptures: the proxy reads more than the
// `remote:` block at startup — app.Name picks the JAIL it dials and app.Backend
// gates the transport. An independent review caught that hashing app.Remote
// alone let a rename leave a running proxy fronting the old machine's hub while
// apply reused it.
func TestRemoteConfigHashCoversWhatTheProxyCaptures(t *testing.T) {
	base := &config.App{Name: "assistant", Backend: "orbstack",
		Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	h := RemoteConfigHash(base)

	renamed := *base
	renamed.Name = "other"
	if RemoteConfigHash(&renamed) == h {
		t.Error("a renamed instance must not reuse a proxy pointed at the old jail")
	}
	rebacked := *base
	rebacked.Backend = "lima"
	if RemoteConfigHash(&rebacked) == h {
		t.Error("a changed backend must not reuse a proxy built for the other transport")
	}
}
