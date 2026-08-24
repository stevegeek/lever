package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/fsutil"
)

// RemoteIdentity is everything a `lever remote serve` process reads at
// startup and never re-reads: the `remote:` block plus the instance name and
// backend. brokerctl.RemoteConfigHash builds one from config.App; state
// deliberately does not import config.
type RemoteIdentity struct {
	Enabled      bool
	Port         int
	BaseURL      string
	AllowedUsers []string
	LoginPort    int
	// Name selects the JAIL the proxy dials and Backend gates which
	// transport it uses. Renaming the instance would otherwise leave a
	// running proxy fronting the OLD machine's hub while apply happily
	// reused it.
	Name    string
	Backend string
}

// RemoteConfigHash identifies the configuration a `lever remote serve` process
// is running WITH.
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
// Like brokerctl.ConfigHash, a marshal failure yields "" — a guaranteed mismatch, so the
// proxy restarts. Failing toward a restart is right for a component whose whole
// job is refusing unauthorized requests.
func RemoteConfigHash(id RemoteIdentity) string {
	return HashJSON(id)
}

// remoteStampContent is the stamp's on-disk form: the lever version, the
// remote config hash, and the pid of the proxy the stamp describes, one per
// line. Plain text rather than JSON because a corrupt or truncated file must
// read as "not what we want" and trigger a restart, which any parse of this
// shape does for free.
func remoteStampContent(version, hash string, pid int) string {
	return version + "\n" + hash + "\n" + strconv.Itoa(pid) + "\n"
}

// remoteProxyPID reads the pid recorded in remote.pid — the file the proxy
// process writes for itself once its listeners are bound
// (daemon.WritePIDFile). Both stamp operations key off THIS number rather
// than os.Getpid(), so the stamp is only ever a statement about the process
// that currently owns the pid file, whoever wrote it.
func (s State) remoteProxyPID() (int, error) {
	pid, err := ReadPID(s.RemotePID())
	if err != nil {
		return 0, fmt.Errorf("state: the remote stamp describes the pid in %s: %w", s.RemotePID(), err)
	}
	return pid, nil
}

// WriteRemoteStamp records what the proxy that owns remote.pid is running.
//
// Two properties make the stamp trustworthy, and both were once missing:
//
//   - The RUNNING PROXY writes it. remoteproxy.Serve calls this through
//     ServeConfig.Stamp as soon as its listeners are bound, so every
//     `lever remote serve` — apply's detached child and an operator's
//     hand-started one alike — leaves a stamp for the config IT loaded.
//     Before that, only apply wrote the stamp while ANY serve wrote
//     remote.pid. The state dir is keyed on the config file's DIRECTORY
//     (StateDir), so a second config file beside lever.yaml shares both files:
//     a proxy started by hand against it inherited the stamp a previous apply
//     had left, satisfying all three reuse conditions — alive, listening,
//     stamp matching — while the live process went on enforcing the other
//     config's allowed_users, and apply reported success.
//   - It names its pid. A stamp is compared against remote.pid, so a proxy
//     that takes over the pid file WITHOUT stamping — an older lever that
//     never learned to, a future starter that forgets — invalidates the match
//     instead of inheriting it. The two files describe one process, or the
//     stamp does not match. It also means a stamp left behind after the proxy
//     exits is inert: removePIDFile takes remote.pid with it.
//
// Any failure removes the stamp rather than leaving one that describes a
// process that is not there. Callers treat a write failure as a warning (see
// remoteproxy.Serve), and no stamp is the only safe residue: an absent stamp
// costs the next apply a redundant restart, a stale one costs it the check.
func (s State) WriteRemoteStamp(version, hash string) error {
	pid, err := s.remoteProxyPID()
	if err != nil {
		_ = os.Remove(s.RemoteStamp())
		return err
	}
	if err := fsutil.WriteFileAtomic(s.RemoteStamp(), []byte(remoteStampContent(version, hash, pid)), 0o600); err != nil {
		_ = os.Remove(s.RemoteStamp())
		return err
	}
	return nil
}

// RemoteStampMatches reports whether the proxy that owns remote.pid was
// started by this binary version with this remote config. Any doubt — no pid
// file, a garbage pid, an absent, unreadable, truncated or differing stamp —
// is false, so apply restarts the proxy.
func (s State) RemoteStampMatches(version, hash string) bool {
	pid, err := s.remoteProxyPID()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(s.RemoteStamp())
	if err != nil {
		return false
	}
	// Compare trimmed so a trailing-newline difference is not a spurious
	// mismatch, while any real difference still is.
	return strings.TrimSpace(string(b)) == strings.TrimSpace(remoteStampContent(version, hash, pid))
}
