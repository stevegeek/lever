package config

import (
	"cmp"
	"path/filepath"
	"slices"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
)

// Default broker ports, used when the config leaves jail_port/admin_port unset
// (0). They are fixed constants rather than dynamically allocated so the apply
// process and the separately-spawned `lever broker serve` (which re-reads the
// same config) agree on the ports without any cross-process plumbing. Set
// explicit ports to run more than one instance's broker on the host at once.
const (
	DefaultBrokerJailPort  = 8443
	DefaultBrokerAdminPort = 8444
)

// Default remote-access ports, used when remote.port / remote.login_port are
// unset. The block reads 8443 jail, 8444 admin, 8445 proxy, 8446 the jail's
// mirrored login forwarder (GuestLoginIssuerPort), 8447 the provider.
const (
	DefaultRemotePort      = 8445
	DefaultRemoteLoginPort = 8447
)

// DefaultDirectiveExpiry is operator.directive_expiry when unset.
const DefaultDirectiveExpiry = 10 * time.Minute

// llmAuthModes scans every agent's effective LLM-auth mode and reports whether
// any is api-key and whether any is subscription. The single source of truth for
// the api-key/subscription/mixed predicates below.
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

// mixedLLMAuth reports whether the instance mixes api-key and subscription
// agents — an unsupported configuration (see validateBroker / security-model-credentials.md §6.1).
func (a *App) mixedLLMAuth() bool {
	anyAPIKey, anySubscription := a.llmAuthModes()
	return anyAPIKey && anySubscription
}

// AnyAPIKeyAgent reports whether any agent (manager or worker) is api-key.
// Exported so brokerctl can decide whether to register the reserved llm pseudo-tool.
func (a *App) AnyAPIKeyAgent() bool {
	anyAPIKey, _ := a.llmAuthModes()
	return anyAPIKey
}

// WorkerDir returns the absolute path of a worker dir (tree + relative dir).
func (a *App) WorkerDir(g Worker) string { return filepath.Join(a.Tree, g.Dir) }

// EffectiveManagerLLMAuth resolves the manager's LLM-auth mode: the broker
// default (subscription when unset).
func (a *App) EffectiveManagerLLMAuth() LLMAuthMode {
	return cmp.Or(a.Manager.LLMAuth, a.brokerLLMAuthDefault())
}

// EffectiveWorkerLLMAuth resolves a worker's LLM-auth mode: its own override else
// the broker default.
func (a *App) EffectiveWorkerLLMAuth(g Worker) LLMAuthMode {
	return cmp.Or(g.LLMAuth, a.brokerLLMAuthDefault())
}

// EffectiveJailPort is the broker's in-jail mTLS port: the configured value, or
// DefaultBrokerJailPort when unset (0).
func (a *App) EffectiveJailPort() int {
	return cmp.Or(a.Broker.JailPort, DefaultBrokerJailPort)
}

// EffectiveAdminPort is the broker's loopback admin port: the configured value,
// or DefaultBrokerAdminPort when unset (0).
func (a *App) EffectiveAdminPort() int {
	return cmp.Or(a.Broker.AdminPort, DefaultBrokerAdminPort)
}

// RemoteEnabled reports whether the remote-access proxy is configured on.
func (a *App) RemoteEnabled() bool { return a.Remote.Enabled }

// ScionWebAssets reports whether lever builds scion's SPA on the host and
// stages it into the guest — and therefore also whether it passes
// `--web-assets-dir` to the hub, and whether `lever doctor` needs a node
// toolchain present.
//
// One predicate for all three, because they must not disagree. Passing the flag
// when the assets were never staged is worse than not passing it: scion then
// serves an empty directory instead of falling back to whatever it has embedded.
//
// Both halves matter. Remote off means no UI is served, so nothing needs
// building. Binary mode means there is no scion source tree to build the SPA
// from, and a binary the operator built themselves may already embed one — see
// guest.ScionSpec.BuildsWebAssets, which this must agree with.
func (a *App) ScionWebAssets() bool {
	return a.RemoteEnabled() && a.Scion.Binary == "" && (a.Scion.Source != "" || a.Scion.Version != "")
}

// EffectiveRemotePort is the proxy's host loopback port: the configured
// value, or DefaultRemotePort — adjacent to the broker's 8443/8444 block.
func (a *App) EffectiveRemotePort() int {
	return cmp.Or(max(a.Remote.Port, 0), DefaultRemotePort)
}

// EffectiveAllowedPorts is the complete set of HOST loopback ports the jail may
// reach through the host alias — the egress allowlist's ACCEPT set
// (internal/egress.BuildRules), and the only thing that decides whether a
// jail→host dial succeeds.
//
// Three contributors, and the third is easy to miss:
//
//   - the broker's jail port, which every agent needs;
//   - manager.allow_ports, the operator's own host tools;
//   - the remote-access login port, when remote access is on. The guest's
//     login forwarder exists solely to reach that host port, so without this
//     grant the allowlist drops its dial — the forwarder listens, the hub's
//     discovery fetch times out, and the browser gets a 502 with every process
//     apparently healthy. That was a live failure, and the containment layer
//     was right: an unallowlisted host port SHOULD be dropped. The grant is
//     narrow (one port, only while remote.enabled) rather than any widening of
//     the alias rule.
//
// One function so the three cannot disagree, and so a port that stops being
// needed stops being granted: with remote access turned off the login port is
// simply absent from the next rebuild, the same convergence the forwarder
// itself gets from DisableHubLogin.
func (a *App) EffectiveAllowedPorts() []int {
	ports := append([]int{a.EffectiveJailPort()}, a.Manager.AllowPorts...)
	if a.RemoteEnabled() {
		ports = append(ports, a.EffectiveRemoteLoginPort())
	}
	// Deduplicate: an operator may well have listed the login port in
	// manager.allow_ports already (that was the only way to reach it before
	// this function granted it). A repeat is harmless to iptables — BuildRules
	// would just emit the same ACCEPT twice — but a set is what this returns.
	out := ports[:0:0]
	for _, p := range ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// EffectiveRemoteLoginPort is the HOST loopback port the local OIDC provider
// binds: the configured value, or DefaultRemoteLoginPort.
//
// 8447, not 8446: 8446 is the guest-side issuer port (GuestLoginIssuerPort),
// which the container runtime mirrors onto the host at the same number.
func (a *App) EffectiveRemoteLoginPort() int {
	return cmp.Or(max(a.Remote.LoginPort, 0), DefaultRemoteLoginPort)
}

// EffectiveAutoReenrol is the natural-lapse healer gate: the configured value,
// or AutoReenrolAll when unset. Validated at load (Validate rejects unknown
// values), so callers may switch on the three constants exhaustively.
func (a *App) EffectiveAutoReenrol() AutoReenrolMode {
	return cmp.Or(a.Broker.AutoReenrol, AutoReenrolAll)
}

func (a *App) brokerLLMAuthDefault() LLMAuthMode {
	return cmp.Or(a.Broker.LLMAuth, LLMAuthAPIKey)
}

// ClosedInternetEgress reports the jail's egress posture, applied jail-wide. It
// is an explicit, independent knob (App.Egress) — NOT derived from llm_auth:
// closed iff `egress: closed` is set. validateBroker guarantees `closed`
// implies a uniformly api-key instance, so a subscription agent is never left
// unable to reach Anthropic.
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

// ManagerCN returns the manager's cert CN (broker.manager_identity, default "manager").
func (a *App) ManagerCN() string {
	return cmp.Or(a.Broker.ManagerIdentity, "manager")
}

// ManagerPromptPath returns the absolute path to the manager's prompt file, or
// "" if none is configured. The prompt is resolved at the instance ROOT (host
// side), NOT under the mounted tree — so a compromised agent in the mount can't
// rewrite the manager's own next boot prompt. Validate() confines PromptFile to
// the root.
func (a *App) ManagerPromptPath() string {
	if a.Manager.PromptFile == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.PromptFile)
}

// ManagerInstructionsPath returns the absolute path to the manager's standing
// instructions file, or "" when none is configured. Resolved at the instance
// ROOT like ManagerPromptPath, and for the same reason: the file becomes the
// agent's CLAUDE.md, so an agent in the mount must not be able to author it.
func (a *App) ManagerInstructionsPath() string {
	if a.Manager.InstructionsFile == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.InstructionsFile)
}

// WorkerInstructionsPath returns the absolute path to a worker's own standing
// instructions file, or "" when the worker names none. There is deliberately
// no fallback to the manager's (contrast WorkerImage/WorkerModel): the
// manager's manual describes orchestration authority a worker must not read,
// so a worker gets lever instructions only when its config says so.
func (a *App) WorkerInstructionsPath(g Worker) string {
	if g.InstructionsFile == "" {
		return ""
	}
	return filepath.Join(a.dir, g.InstructionsFile)
}

// OperatorAllowedSignersPath returns the absolute path to the operator's
// allowed_signers file, or "" if directives are disabled (AllowedSigners
// unset). Resolved at the instance ROOT (host side), like ManagerPromptPath —
// Operator.AllowedSigners itself stays a confined-relative string on the
// struct; this is the accessor a consumer (the broker's signature verifier)
// joins against the instance dir to actually read the file.
func (a *App) OperatorAllowedSignersPath() string {
	if a.Operator.AllowedSigners == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Operator.AllowedSigners)
}

// WorkerToWorkerMessaging reports whether workers may message each other
// (default true; broker.messaging.worker_to_worker: false disables).
func (a *App) WorkerToWorkerMessaging() bool {
	if v := a.Broker.Messaging.WorkerToWorker; v != nil {
		return *v
	}
	return true
}

// DirectivesEnabled reports whether the operator-directive channel is
// active. allowed_signers is the sole gate: unset means there is no key
// material to verify a directive's signature against, so the channel stays
// off (fail closed by omission — no `operator:` block needed to opt out).
func (a *App) DirectivesEnabled() bool {
	return a.Operator.AllowedSigners != ""
}

// EffectiveDirectiveExpiry is the configured operator.directive_expiry, or
// DefaultDirectiveExpiry when unset (0).
func (a *App) EffectiveDirectiveExpiry() time.Duration {
	return cmp.Or(max(a.Operator.DirectiveExpiry, 0), DefaultDirectiveExpiry)
}

// EffectiveDirectiveExpiryMax is the configured operator.directive_expiry_max,
// or opsig.MaxExpiry when unset (0). That is a hard ceiling: validateOperator
// rejects a configured value above it, so a loaded *App never has to clamp
// here — a config asking for more never gets past Load.
func (a *App) EffectiveDirectiveExpiryMax() time.Duration {
	return cmp.Or(max(a.Operator.DirectiveExpiryMax, 0), opsig.MaxExpiry)
}

// OperatorPrincipal is the ssh-keygen allowed_signers principal for this
// instance's operator identity.
func (a *App) OperatorPrincipal() string {
	return "operator@" + a.Name
}
