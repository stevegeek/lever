package brokerctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/stevegeek/lever/internal/config"
)

// RemoteConfigHash identifies the configuration a `lever remote serve` process
// is running WITH — everything under `remote:` that the proxy reads at startup
// and never re-reads.
//
// It exists because the proxy caches all of it in the handler it builds once:
// ServeHost comes from base_url, the allowed-user set is captured by value, and
// the ports are bound. Changing any of them in lever.yaml therefore has NO
// effect on a running proxy, and apply's "already serving" shortcut cannot see
// the difference. That was a real defect, not a theoretical one: enabling
// `allowed_users` on the live instance left the old process serving, so an
// identity-free request kept returning 200 long after the config said it must
// be refused. Silently ignoring a security-relevant config change is the worst
// available behaviour.
//
// Deliberately NOT brokerctl.ConfigHash: that covers Broker/Workers/Scion, none
// of which the proxy reads, and a broker-only change must not bounce the proxy.
//
// Like ConfigHash, a marshal failure yields "" — a guaranteed mismatch, so the
// proxy restarts. Failing toward a restart is right for a component whose whole
// job is refusing unauthorized requests.
func RemoteConfigHash(app *config.App) string {
	// Name and Backend are in here because the proxy captures them too, not
	// just the `remote:` block: Name selects the JAIL it dials (remote.go
	// machineName) and Backend gates which transport it uses. Renaming the
	// instance would otherwise leave a running proxy fronting the OLD
	// machine's hub while apply happily reused it.
	j, err := json.Marshal(struct {
		Remote  config.Remote
		Name    string
		Backend string
	}{app.Remote, app.Name, app.Backend})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(j)
	return hex.EncodeToString(sum[:])
}

// remoteStampContent is the stamp's on-disk form: version and config hash on
// one line. Plain text rather than JSON because a corrupt or truncated file
// must read as "not what we want" and trigger a restart, which any parse of
// this shape does for free.
func remoteStampContent(version, hash string) string {
	return version + "\n" + hash + "\n"
}

// WriteRemoteStamp records what a freshly spawned proxy is running.
// Best-effort by design: a stamp that cannot be written makes the NEXT apply
// see a mismatch and restart the proxy, which is safe — the failure mode is a
// redundant restart, never a stale process kept alive.
func (s State) WriteRemoteStamp(version, hash string) error {
	return os.WriteFile(s.RemoteStamp(), []byte(remoteStampContent(version, hash)), 0o600)
}

// RemoteStampMatches reports whether the running proxy was started with this
// binary version AND this remote config. Any doubt — absent file, unreadable,
// truncated, different content — is false, so apply restarts.
func (s State) RemoteStampMatches(version, hash string) bool {
	b, err := os.ReadFile(s.RemoteStamp())
	if err != nil {
		return false
	}
	// Compare trimmed so a trailing-newline difference is not a spurious
	// mismatch, while any real difference still is.
	return strings.TrimSpace(string(b)) == strings.TrimSpace(remoteStampContent(version, hash))
}
