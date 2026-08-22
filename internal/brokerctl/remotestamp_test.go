package brokerctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// TestRemoteStampRoundTripAndMismatch pins the contract apply's reuse shortcut
// depends on: a stamp matches only the exact version+config it recorded, and
// every doubt reads as a mismatch (so the proxy restarts rather than serving a
// config the operator has since changed).
func TestRemoteStampRoundTripAndMismatch(t *testing.T) {
	s := StateDir(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Absent file: mismatch, not a crash — a proxy started by an older lever
	// left no stamp and must be restarted, never trusted.
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("an absent stamp must not match")
	}

	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}
	if !s.RemoteStampMatches("v1", "h1") {
		t.Error("a stamp must match what it recorded")
	}
	if s.RemoteStampMatches("v2", "h1") {
		t.Error("a different BINARY must not match — a new lever may serve differently")
	}
	if s.RemoteStampMatches("v1", "h2") {
		t.Error("a different CONFIG must not match — this is the allowed_users defect")
	}

	// Truncated/garbage content reads as a mismatch rather than matching by
	// accident or erroring.
	if err := os.WriteFile(s.RemoteStamp(), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("a truncated stamp must not match")
	}

	// The stamp names a proxy credential-adjacent state dir; keep it owner-only.
	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.RemoteStamp())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode = %v, want 0600", fi.Mode().Perm())
	}
	if filepath.Dir(s.RemoteStamp()) != s.Dir {
		t.Error("the stamp must live beside remote.pid in the state dir")
	}
}

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
