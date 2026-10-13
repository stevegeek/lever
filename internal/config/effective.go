package config

import (
	"cmp"
	"fmt"
	"net"
	"net/textproto"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/opsig"
)

// Default broker ports. They are fixed rather than allocated so apply and the
// separately spawned `lever broker serve` agree without plumbing; set explicit
// ports to run more than one instance's broker on a host.
const (
	DefaultBrokerJailPort  = 8443
	DefaultBrokerAdminPort = 8444
)

// DefaultRemotePort is remote.port when unset.
const DefaultRemotePort = 8445

// DefaultDirectiveExpiry is operator.directive_expiry when unset.
const DefaultDirectiveExpiry = 10 * time.Minute

func (a *App) llmAuthModes() (anyAPIKey, anySubscription bool) {
	mark := func(m LLMAuthMode) {
		if m == LLMAuthAPIKey {
			anyAPIKey = true
		} else {
			anySubscription = true
		}
	}
	mark(a.EffectiveManagerLLMAuth())
	for _, g := range a.Workers {
		mark(a.EffectiveWorkerLLMAuth(g))
	}
	return
}

func (a *App) mixedLLMAuth() bool {
	anyAPIKey, anySubscription := a.llmAuthModes()
	return anyAPIKey && anySubscription
}

// AnyAPIKeyAgent reports whether any agent is api-key.
func (a *App) AnyAPIKeyAgent() bool {
	anyAPIKey, _ := a.llmAuthModes()
	return anyAPIKey
}

// AnyRecyclableWorker reports whether some worker sets recyclable: the
// manager's skill teaches `lever-manager agent recycle` only then.
func (a *App) AnyRecyclableWorker() bool {
	return slices.ContainsFunc(a.Workers, func(w Worker) bool { return w.Recyclable })
}

// WorkerDir returns the absolute path of a worker dir.
func (a *App) WorkerDir(g Worker) string { return filepath.Join(a.Tree, g.Dir) }

// EffectiveManagerLLMAuth resolves the manager's LLM-auth mode.
func (a *App) EffectiveManagerLLMAuth() LLMAuthMode {
	return cmp.Or(a.Manager.LLMAuth, a.brokerLLMAuthDefault())
}

// EffectiveWorkerLLMAuth resolves a worker's LLM-auth mode.
func (a *App) EffectiveWorkerLLMAuth(g Worker) LLMAuthMode {
	return cmp.Or(g.LLMAuth, a.brokerLLMAuthDefault())
}

// EffectiveJailPort is the broker's in-jail mTLS port.
func (a *App) EffectiveJailPort() int {
	return cmp.Or(a.Broker.JailPort, DefaultBrokerJailPort)
}

// EffectiveAdminPort is the broker's loopback admin port.
func (a *App) EffectiveAdminPort() int {
	return cmp.Or(a.Broker.AdminPort, DefaultBrokerAdminPort)
}

// RemoteEnabled reports whether the remote-access proxy is configured on.
func (a *App) RemoteEnabled() bool { return a.Remote.Enabled }

// ScionWebAssets reports whether lever builds scion's SPA and stages it into
// the guest, and so whether it passes `--web-assets-dir` to the hub and
// whether doctor needs node. These must not disagree: with the flag but no
// staged assets, scion serves an empty directory instead of its embedded
// UI. Must agree with guest.ScionSpec.BuildsWebAssets.
func (a *App) ScionWebAssets() bool {
	return a.RemoteEnabled() && a.Scion.Binary == "" && (a.Scion.Source != "" || a.Scion.Version != "")
}

// EffectiveRemotePort is the proxy's host loopback port.
func (a *App) EffectiveRemotePort() int {
	return cmp.Or(max(a.Remote.Port, 0), DefaultRemotePort)
}

// EffectiveAllowedPorts is the complete set of host loopback ports the jail
// may reach through the host alias: the egress allowlist's ACCEPT set. It is
// the broker's jail port and manager.allow_ports. A port no longer needed
// drops out of the next rebuild.
func (a *App) EffectiveAllowedPorts() []int {
	ports := append([]int{a.EffectiveJailPort()}, a.Manager.AllowPorts...)
	out := ports[:0:0]
	for _, p := range ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Warnings are settings that load but no longer do anything, one sentence
// each. Unlike RemoteWarnings they apply whether or not remote access is on.
func (a *App) Warnings() []string {
	var out []string
	if a.Remote.LoginPort != 0 {
		out = append(out, "remote.login_port is no longer used (the hub login needs no port); remove it")
	}
	return out
}

// DefaultRemoteIdentityHeader is remote.identity_header when unset: the
// header `tailscale serve` puts the tailnet login in.
const DefaultRemoteIdentityHeader = "Tailscale-User-Login"

// DefaultRemoteBind is remote.bind when unset: host loopback only.
const DefaultRemoteBind = "127.0.0.1"

// EffectiveRemoteIdentityHeader is in canonical form, as net/http keys
// headers.
func (a *App) EffectiveRemoteIdentityHeader() string {
	return textproto.CanonicalMIMEHeaderKey(cmp.Or(strings.TrimSpace(a.Remote.IdentityHeader), DefaultRemoteIdentityHeader))
}

// EffectiveRemoteBind is the address the proxy listens on.
func (a *App) EffectiveRemoteBind() string {
	return cmp.Or(strings.TrimSpace(a.Remote.Bind), DefaultRemoteBind)
}

// Values of remote.landing.
const (
	RemoteLandingConsole = "console"
	RemoteLandingChat    = "chat"
)

// EffectiveRemoteLanding is remote.landing with unset read as the console.
func (a *App) EffectiveRemoteLanding() string {
	return cmp.Or(a.Remote.Landing, RemoteLandingConsole)
}

// RemoteLandingChat reports whether the proxy serves lever's chat page.
func (a *App) RemoteLandingChat() bool {
	return a.RemoteEnabled() && a.Remote.Landing == RemoteLandingChat
}

// AgentMessagesOn reports whether agents may start messages to contacts.
func (a *App) AgentMessagesOn() bool { return a.RemoteEnabled() && a.Remote.AgentMessages.Enabled }

// PushOn reports whether the proxy sends Web Push notifications.
func (a *App) PushOn() bool { return a.RemoteEnabled() && a.Remote.Push.Enabled }

// EffectiveAgentFollowUpAfter is follow_up_after with its default.
func (a *App) EffectiveAgentFollowUpAfter() time.Duration {
	return cmp.Or(a.Remote.AgentMessages.FollowUpAfter, DefaultAgentFollowUpAfter)
}

// EffectiveAgentMaxChars is max_chars with its default.
func (a *App) EffectiveAgentMaxChars() int {
	return cmp.Or(a.Remote.AgentMessages.MaxChars, DefaultAgentMaxChars)
}

// FilesOn reports whether the chat page's file exchange is on.
func (a *App) FilesOn() bool { return a.RemoteLandingChat() && a.Remote.Files.Enabled }

// EffectiveFilesMaxBytes is max_bytes with its default.
func (a *App) EffectiveFilesMaxBytes() int64 {
	return cmp.Or(a.Remote.Files.MaxBytes, DefaultFilesMaxBytes)
}

// EffectiveFilesExtensions is extensions with its default, as a copy.
func (a *App) EffectiveFilesExtensions() []string {
	if len(a.Remote.Files.Extensions) == 0 {
		return slices.Clone(chatfiles.DefaultExtensions)
	}
	return slices.Clone(a.Remote.Files.Extensions)
}

// FilesUploadsOn reports whether logins may upload.
func (a *App) FilesUploadsOn() bool {
	return a.FilesOn() && (a.Remote.Files.Uploads == nil || *a.Remote.Files.Uploads)
}

// FilesSharesOn reports whether agents may share files.
func (a *App) FilesSharesOn() bool {
	return a.FilesOn() && (a.Remote.Files.Shares == nil || *a.Remote.Files.Shares)
}

// FilesExcludedLogins is every allowed_users login with files: false, while
// files are on.
func (a *App) FilesExcludedLogins() []string {
	if !a.FilesOn() {
		return nil
	}
	var out []string
	for _, u := range a.Remote.AllowedUsers {
		if !u.FilesAllowed() {
			out = append(out, u.Login)
		}
	}
	return out
}

// AgentWorkspaces maps each agent name to its workspace, tree-relative:
// the manager's is the tree ("."), a worker's its dir. Inside its container
// each one is /workspace.
func (a *App) AgentWorkspaces() map[string]string {
	out := map[string]string{a.Name: "."}
	for _, w := range a.Workers {
		out[w.Name] = path.Clean(filepath.ToSlash(w.Dir))
	}
	return out
}

// RemoteBindLoopback reports whether the proxy listens on loopback only, the
// posture every other remote-access check assumes.
func (a *App) RemoteBindLoopback() bool {
	ip := net.ParseIP(a.EffectiveRemoteBind())
	return ip != nil && ip.IsLoopback()
}

// RemoteBindWildcard reports whether remote.bind is 0.0.0.0 or "::".
func (a *App) RemoteBindWildcard() bool {
	ip := net.ParseIP(a.EffectiveRemoteBind())
	return ip != nil && ip.IsUnspecified()
}

// RemoteListenAddr is the host:port the proxy binds.
func (a *App) RemoteListenAddr() string {
	return net.JoinHostPort(a.EffectiveRemoteBind(), strconv.Itoa(a.EffectiveRemotePort()))
}

// RemoteProbeAddr is the host:port a host-side caller dials to reach the
// proxy: the loopback of the same family for a wildcard bind, which cannot be
// dialled.
func (a *App) RemoteProbeAddr() string {
	host := a.EffectiveRemoteBind()
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.To4() == nil {
			host = "::1"
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(a.EffectiveRemotePort()))
}

// RemoteWarnings are the remote settings that load but weaken a default
// protection, one sentence each.
func (a *App) RemoteWarnings() []string {
	if !a.RemoteEnabled() {
		return nil
	}
	var out []string
	if !a.RemoteBindLoopback() {
		out = append(out, fmt.Sprintf("remote.bind is %s, not loopback: anything that can reach %s can set %s to any login "+
			"and ride the operator's hub session, so the host firewall must admit only the authenticating front to that port",
			a.EffectiveRemoteBind(), a.RemoteListenAddr(), a.EffectiveRemoteIdentityHeader()))
	}
	if a.Remote.TrustForwardedHost {
		out = append(out, "remote.trust_forwarded_host is on: on a request whose Host is an IP address, the proxy's Host "+
			"check reads X-Forwarded-Host instead, which any client that reaches the listener directly can set")
	}
	if len(a.Remote.AllowedUsers) == 0 && a.EffectiveRemoteIdentityHeader() != DefaultRemoteIdentityHeader {
		out = append(out, fmt.Sprintf("remote.identity_header is %s but allowed_users is empty: the header is never checked, "+
			"so everyone the front lets through (every user it grants access to, not only you) rides the placeholder "+
			"operator's hub session — list your login in allowed_users", a.EffectiveRemoteIdentityHeader()))
	}
	return out
}

// EffectiveAutoReenrol is validated at load, so callers may switch on the
// three constants exhaustively.
func (a *App) EffectiveAutoReenrol() AutoReenrolMode {
	return cmp.Or(a.Broker.AutoReenrol, AutoReenrolAll)
}

// EffectiveScionTelemetry is the agents' sciontool telemetry posture.
func (a *App) EffectiveScionTelemetry() ScionTelemetryMode {
	return cmp.Or(a.Scion.Telemetry, ScionTelemetryOff)
}

func (a *App) brokerLLMAuthDefault() LLMAuthMode {
	return cmp.Or(a.Broker.LLMAuth, LLMAuthAPIKey)
}

// ClosedInternetEgress reports whether `egress: closed` is set. validateBroker
// guarantees it implies a uniformly api-key instance.
func (a *App) ClosedInternetEgress() bool {
	return a.Egress == EgressClosed
}

// WorkerByName returns the configured worker with the given name, or false.
func (a *App) WorkerByName(name string) (Worker, bool) {
	for _, g := range a.Workers {
		if g.Name == name {
			return g, true
		}
	}
	return Worker{}, false
}

// ManagerCN returns the manager's cert CN.
func (a *App) ManagerCN() string {
	return cmp.Or(a.Broker.ManagerIdentity, "manager")
}

// ManagerPromptPath returns the absolute path to the manager's prompt file,
// or "". It is resolved at the instance root, not under the mounted tree, so
// a compromised agent cannot rewrite its own next boot prompt.
func (a *App) ManagerPromptPath() string {
	if a.Manager.PromptFile == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.PromptFile)
}

// ManagerInstructionsPath is resolved at the instance root like
// ManagerPromptPath: the file becomes the agent's CLAUDE.md, so an agent in
// the mount must not be able to author it.
func (a *App) ManagerInstructionsPath() string {
	if a.Manager.InstructionsFile == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.InstructionsFile)
}

// WorkerInstructionsPath has no fallback to the manager's file: the
// manager's manual describes orchestration authority a worker must not read.
func (a *App) WorkerInstructionsPath(g Worker) string {
	if g.InstructionsFile == "" {
		return ""
	}
	return filepath.Join(a.dir, g.InstructionsFile)
}

// OperatorAllowedSignersPath returns the absolute path to the operator's
// allowed_signers file, resolved at the instance root, or "".
func (a *App) OperatorAllowedSignersPath() string {
	if a.Operator.AllowedSigners == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Operator.AllowedSigners)
}

// WorkerToWorkerMessaging reports whether workers may message each other.
func (a *App) WorkerToWorkerMessaging() bool {
	if v := a.Broker.Messaging.WorkerToWorker; v != nil {
		return *v
	}
	return true
}

// DirectivesEnabled reports whether the operator-directive channel is
// active: without allowed_signers there is no key to verify a directive
// against, so the channel stays off.
func (a *App) DirectivesEnabled() bool {
	return a.Operator.AllowedSigners != ""
}

// EffectiveDirectiveExpiry is operator.directive_expiry with its default.
func (a *App) EffectiveDirectiveExpiry() time.Duration {
	return cmp.Or(max(a.Operator.DirectiveExpiry, 0), DefaultDirectiveExpiry)
}

// EffectiveDirectiveExpiryMax needs no clamp: validateOperator rejects a
// value above opsig.MaxExpiry.
func (a *App) EffectiveDirectiveExpiryMax() time.Duration {
	return cmp.Or(max(a.Operator.DirectiveExpiryMax, 0), opsig.MaxExpiry)
}

// OperatorPrincipal is the ssh-keygen allowed_signers principal for this
// instance's operator identity.
func (a *App) OperatorPrincipal() string {
	return "operator@" + a.Name
}

// VoiceOn reports whether the chat page offers dictation.
func (a *App) VoiceOn() bool { return a.RemoteLandingChat() && a.Remote.Voice.Enabled }

// EffectiveVoiceMaxSeconds is remote.voice.max_seconds with its default.
func (a *App) EffectiveVoiceMaxSeconds() int {
	return cmp.Or(max(a.Remote.Voice.MaxSeconds, 0), DefaultVoiceMaxSeconds)
}

// VoiceExcludedLogins is every allowed_users login with voice: false, while
// voice is on.
func (a *App) VoiceExcludedLogins() []string {
	if !a.VoiceOn() {
		return nil
	}
	var out []string
	for _, u := range a.Remote.AllowedUsers {
		if !u.VoiceAllowed() {
			out = append(out, u.Login)
		}
	}
	return out
}

// Lima guest kernel flavours (guest_kernel).
const (
	GuestKernelHWE = "hwe" // linux-generic-hwe-<release>
	GuestKernelGA  = "ga"  // the release kernel the cloud image ships
)

// EffectiveGuestKernel defaults to hwe with nested_virt because a nested VM
// crashed on the 6.8 GA kernel, while an instance that runs no nested VM has
// no reason to change kernels. "" off Lima.
func (a *App) EffectiveGuestKernel() string {
	if a.Backend != BackendLima {
		return ""
	}
	if a.GuestKernel != "" {
		return a.GuestKernel
	}
	if a.NestedVirt {
		return GuestKernelHWE
	}
	return GuestKernelGA
}
