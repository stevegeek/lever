package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/fsutil"
)

// RemoteIdentity is everything a `lever remote serve` process reads once at
// startup: the `remote:` block plus the instance name and backend.
// brokerctl.RemoteConfigHash builds one from config.App; state does not
// import config.
type RemoteIdentity struct {
	Enabled      bool
	Port         int
	BaseURL      string
	AllowedUsers []string
	// HubUsers and HubKey are the hub-identity record the proxy signs in
	// with: each login's recorded hub user ID and the assertion key.
	HubUsers map[string]string `json:",omitempty"`
	HubKey   string            `json:",omitempty"`
	// The front's contract: which header carries the login, where the proxy
	// listens, and whether the Host check reads X-Forwarded-Host.
	IdentityHeader     string
	Bind               string
	AllowWildcardBind  bool
	TrustForwardedHost bool
	// Landing decides whether the proxy serves lever's chat page and where
	// "/" goes.
	Landing string
	// Name selects the jail the proxy dials and Backend its transport: a
	// rename must not leave a running proxy fronting the old machine's hub.
	Name    string
	Backend string
	// The chat page's agent list (landing chat only, so a worker change does
	// not bounce a console-landing proxy).
	Workers    []string `json:",omitempty"`
	Tree       string   `json:",omitempty"`
	LabelsFile string   `json:",omitempty"`
	// AgentMessages: the proxy filters a contact's history, events and
	// previews. Omitted when off, so the stamp is unchanged.
	AgentMessages bool `json:",omitempty"`
	// Push is the VAPID subject while remote.push is on; omitted when off so
	// the stamp is unchanged.
	Push string `json:",omitempty"`
	// Files (landing chat only): the limits and every agent's workspace the
	// proxy writes uploads into. Omitted when off.
	Files *FilesIdentity `json:",omitempty"`
	// Voice (landing chat only): the tool's socket the proxy posts clips to
	// and the route's limits. Omitted when off.
	Voice *VoiceIdentity `json:",omitempty"`
}

// VoiceIdentity is the part of remote.voice a running proxy captured. The
// transcription settings are the tool's flags: a change there restarts the
// tool (the broker's stamp), not the proxy.
type VoiceIdentity struct {
	Socket      string
	MaxSeconds  int
	Excluded    []string `json:",omitempty"` // allowed_users voice: false
	NoReadAloud bool     `json:",omitempty"` // remote.voice.read_aloud: off (or false)
	// SpeakSocket is the TTS tool's socket while read_aloud is server
	// (omitted otherwise, so the stamp is unchanged).
	SpeakSocket string `json:",omitempty"`
}

// FilesIdentity is the part of remote.files a running proxy captured.
type FilesIdentity struct {
	MaxBytes   int64
	Extensions []string
	Workspaces []string // "name=dir", config order, manager first
	// Set only when not the default, so the stamp is unchanged without them.
	NoUploads bool     `json:",omitempty"`
	NoShares  bool     `json:",omitempty"`
	Excluded  []string `json:",omitempty"`
}

// RemoteConfigHash identifies the configuration a `lever remote serve` process
// is running with. The proxy captures all of it once (ServeHost, the
// allowed-user set by value, bound ports), so a lever.yaml change has no
// effect on a running proxy; apply compares this hash so a security-relevant
// change such as enabling allowed_users is never silently ignored.
//
// Not brokerctl.ConfigHash: a broker-only change must not bounce the proxy.
// A marshal failure yields "", a guaranteed mismatch, so the proxy restarts:
// failing toward a restart is right for a component whose job is refusing
// unauthorized requests.
func RemoteConfigHash(id RemoteIdentity) string {
	return HashJSON(id)
}

// remoteStampContent is the stamp's on-disk form: version, remote config hash
// and the proxy's pid, one per line. Plain text so a corrupt or truncated
// file reads as a mismatch and triggers a restart.
func remoteStampContent(version, hash string, pid int) string {
	return version + "\n" + hash + "\n" + strconv.Itoa(pid) + "\n"
}

// remoteProxyPID reads remote.pid, which the proxy writes for itself once
// bound. Both stamp operations key off this pid rather than os.Getpid(), so
// the stamp only ever describes the process that owns the pid file.
func (s State) remoteProxyPID() (int, error) {
	pid, err := ReadPID(s.RemotePID())
	if err != nil {
		return 0, fmt.Errorf("state: the remote stamp describes the pid in %s: %w", s.RemotePID(), err)
	}
	return pid, nil
}

// WriteRemoteStamp records what the proxy that owns remote.pid is running.
//
//   - Only the running proxy writes it (remoteproxy.Serve via
//     ServeConfig.Stamp, once bound), so every `lever remote serve`,
//     hand-started ones included, stamps the config it loaded. The state dir
//     is keyed on the config file's directory, so a second config beside
//     lever.yaml shares both files; a stamp written by anyone else could
//     vouch for a proxy enforcing another config's allowed_users.
//   - It names its pid, compared against remote.pid: a proxy that takes
//     over the pid file without stamping invalidates the match instead of
//     inheriting it, and a stamp left after the proxy exits is inert.
//
// Any failure removes the stamp. Callers treat a write failure as a warning,
// so no stamp is the only safe residue: an absent stamp costs a redundant
// restart, a stale one would skip the check.
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
// started by this version with this remote config. Any doubt is false, so
// apply restarts the proxy.
func (s State) RemoteStampMatches(version, hash string) bool {
	pid, err := s.remoteProxyPID()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(s.RemoteStamp())
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == strings.TrimSpace(remoteStampContent(version, hash, pid))
}
