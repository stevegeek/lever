// Package config loads an application config: the declarative description of a
// lever agent-manager application (the manager + its workers).
package config

import (
	"cmp"
	"time"
)

// Grant is a self-obtain capability grant on an agent (mint a token for itself).
type Grant struct {
	Tool string `yaml:"tool"`
	Op   string `yaml:"op"`
}

// DelegateGrant lets an agent mint a (tool, op) token bound to each recipient in To.
type DelegateGrant struct {
	Tool string   `yaml:"tool"`
	Op   string   `yaml:"op"`
	To   []string `yaml:"to"`
}

// Op declares one operation of a first-party tool. CaveatParam, when set, is a
// DECLARED GUARD: the broker validates the tool-shipped map equals it at
// registration (the stored value remains the tool's). Empty ⇒ accept whatever
// the tool ships.
type Op struct {
	Name        string            `yaml:"name"`
	CaveatParam map[string]string `yaml:"caveat_param"`
	// Params optionally declares the operation's argument names. When set,
	// the broker rejects capability-mint constraints on any other key AT MINT
	// TIME (#21) — a mistyped constraint key otherwise mints a strictly
	// over-narrowed token that fails closed only at call time, far from the
	// mistake. Empty keeps the permissive behavior (any key is a constraint).
	Params []string `yaml:"params"`
}

// Tool declares a tool the broker gates behind its mTLS tool routes (/mcp/<name>/): either a
// first-party (capability-aware) subprocess the broker supervises, or — with
// External — an already-running host MCP server the broker fronts.
type Tool struct {
	Name          string              `yaml:"name"`
	Command       []string            `yaml:"command"`
	Backend       string              `yaml:"backend"`
	Operations    []Op                `yaml:"operations"`
	AllowedValues map[string][]string `yaml:"allowed_values"`
	// External marks an already-running host MCP server the broker fronts but
	// does NOT spawn (its lifecycle — and any TCC/Automation grant — stays with
	// the user session). It is registered from config with FirstParty=false, so
	// the broker enforces the rules and strips the capability token. Command
	// must be empty; Backend is the server's own listen address,
	// host:port[/path], loopback unless AllowNonLoopback.
	External bool `yaml:"external"`
	// Gate selects the capability grain (fine|coarse, default fine). Only
	// valid on an external tool.
	Gate Gate `yaml:"gate"`
	// AllowNonLoopback permits an external Backend beyond a literal loopback
	// IP. The broker proxies HOST-side, so a non-loopback backend hands the
	// jailed agent a path to another host THROUGH the broker, bypassing the
	// jail's LAN-drop egress — leave this off unless that is exactly intended.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

// EffectiveGate resolves a tool's capability grain: the declared gate, or
// GateFine when unset.
func (t Tool) EffectiveGate() Gate {
	return cmp.Or(t.Gate, GateFine)
}

// LLMAuthMode selects how an agent authenticates to the Anthropic API.
//   - api-key (the default): the container holds only a capability(llm) token
//     and reaches the model through the broker /llm proxy, which injects the
//     real Console key host-side — the real key never enters the container.
//   - subscription: the container reaches api.anthropic.com directly with the
//     owner's OAuth credential (a dev/personal opt-in — the real token is
//     projected into the container).
type LLMAuthMode string

const (
	LLMAuthSubscription LLMAuthMode = "subscription"
	LLMAuthAPIKey       LLMAuthMode = "api-key"
)

// valid reports whether m is a declared mode or unset (unset resolves to a
// default at the Effective* layer).
func (m LLMAuthMode) valid() bool {
	return m == "" || m == LLMAuthSubscription || m == LLMAuthAPIKey
}

// Gate selects the capability grain the broker enforces on an EXTERNAL tool.
//   - fine (the default): only the declared operations are callable; a token
//     must name the specific MCP tool being invoked (op == params.name).
//   - coarse: one wildcard capability ({tool, "*"}) admits every MCP call the
//     server exposes — wholesale trust of its whole surface. Use for tools
//     where per-operation gating adds nothing; keep sensitive servers fine.
type Gate string

const (
	GateFine   Gate = "fine"
	GateCoarse Gate = "coarse"
)

// valid reports whether g is a declared grain or unset (unset resolves to
// GateFine via EffectiveGate).
func (g Gate) valid() bool {
	return g == "" || g == GateFine || g == GateCoarse
}

// EgressMode selects the jail's outbound network posture. It is independent of
// LLMAuthMode: api-key isolates the credential; egress controls what the agent
// can reach on the network.
//   - open (the default): the allowlist drops the LAN and non-allowlisted
//     host-alias ports but leaves the public internet reachable (so agents can
//     fetch dependencies).
//   - closed: a catch-all DROP so the jail reaches ONLY the broker port — the
//     most locked-down posture. Requires a uniformly api-key instance (a
//     subscription agent needs direct internet to reach Anthropic).
type EgressMode string

const (
	EgressOpen   EgressMode = "open"
	EgressClosed EgressMode = "closed"
)

// valid reports whether m is a declared posture or unset (unset resolves to
// open via ClosedInternetEgress).
func (m EgressMode) valid() bool {
	return m == "" || m == EgressOpen || m == EgressClosed
}

// Messaging is the broker's message-routing policy knobs. Routing policy, not
// capability grants: identities come from mTLS; recipients from config.
type Messaging struct {
	// WorkerToWorker permits worker→worker sends. nil/unset ⇒ true (allowed);
	// pointer-bool so an explicit `false` is distinguishable for stricter
	// hub-and-spoke models.
	WorkerToWorker *bool `yaml:"worker_to_worker"`
}

// Broker holds broker settings + first-party tool declarations.
type Broker struct {
	JailPort        int           `yaml:"jail_port"`
	AdminPort       int           `yaml:"admin_port"`
	GrantTTL        time.Duration `yaml:"grant_ttl"`
	TicketTTL       time.Duration `yaml:"ticket_ttl"`
	ManagerIdentity string        `yaml:"manager_identity"`
	APIKeyFile      string        `yaml:"api_key_file"` // api-key mode
	LLMAuth         LLMAuthMode   `yaml:"llm_auth"`
	// LLMUpstream overrides the /llm proxy target (default https://api.anthropic.com).
	// Set to a fake upstream for testing; never client-controlled. Empty = default.
	LLMUpstream string    `yaml:"llm_upstream"`
	Tools       []Tool    `yaml:"tools"`
	Messaging   Messaging `yaml:"messaging"`
	// AutoReenrol gates the broker's natural-lapse auto-re-enrol healer: which
	// agents it heals after an mTLS leaf ages out naturally (our CA's cert,
	// valid in every way but time — never a revoked or epoch-stale identity).
	// all (default) | manager | off. See #22.
	AutoReenrol AutoReenrolMode `yaml:"auto_reenrol"`
}

// AutoReenrolMode selects which agents the broker's natural-lapse healer
// covers. Empty resolves to AutoReenrolAll (EffectiveAutoReenrol).
type AutoReenrolMode string

const (
	AutoReenrolAll     AutoReenrolMode = "all"
	AutoReenrolManager AutoReenrolMode = "manager"
	AutoReenrolOff     AutoReenrolMode = "off"
)

// valid reports whether m is a declared mode or unset (unset resolves to
// AutoReenrolAll via EffectiveAutoReenrol).
func (m AutoReenrolMode) valid() bool {
	return m == "" || m == AutoReenrolAll || m == AutoReenrolManager || m == AutoReenrolOff
}

type Manager struct {
	Image          string          `yaml:"image"`
	PromptFile     string          `yaml:"prompt_file"`
	AllowPorts     []int           `yaml:"allow_ports"`
	CredentialFile string          `yaml:"credential_file"`
	LLMAuth        LLMAuthMode     `yaml:"llm_auth"`
	Obtain         []Grant         `yaml:"obtain"`
	Delegate       []DelegateGrant `yaml:"delegate"`
}

type Worker struct {
	Name     string          `yaml:"name"`
	Dir      string          `yaml:"dir"`
	Image    string          `yaml:"image"` // optional; empty ⇒ inherit Manager.Image
	LLMAuth  LLMAuthMode     `yaml:"llm_auth"`
	Obtain   []Grant         `yaml:"obtain"`
	Delegate []DelegateGrant `yaml:"delegate"`
}

type ScionConfig struct {
	// Binary is a host path to an already-built linux scion binary, installed
	// into the jail as-is. Unlike Source and Version it needs no Go toolchain,
	// module cache or egress on the machine hosting the jail — that host need
	// not be a build host (issue #27). Mutually exclusive with both.
	Binary string `yaml:"binary"`
	// Source is a host path to a scion source checkout to cross-compile into the
	// jail (local development). Mutually exclusive with Version.
	Source string `yaml:"source"`
	// Version pins a scion module version/commit (e.g. a commit hash or a
	// vX.Y.Z tag) that lever fetches via the Go module system and cross-compiles
	// into the jail — no vendored source tree. Mutually exclusive with Source.
	Version string `yaml:"version"`
	// AgentRole OVERRIDES the role lever stamps on `scion start` (scion#1089).
	// You do not need to set it: empty means lever picks `baseline` itself
	// whenever the installed scion understands roles at all.
	//
	// Leaving it empty is safe by construction, because lever probes the scion
	// BINARY for the --role flag rather than guessing from the pin. That
	// matters: scion#1090 flipped the default for an unspecified role from
	// baseline to FULL — agent create, lifecycle AND project-secret-read — so
	// staying silent on a recent pin would break lever's core invariant that no
	// agent holds hub authority. A commit hash cannot tell lever which side of
	// that change a pin sits on; the binary can.
	//
	// Values are the scion#1089 bundles. `baseline` is what lever wants:
	// heartbeat and self-token-refresh, no agent create/lifecycle/secret scope.
	// `readonly` cannot heartbeat, so a live agent cannot run on it. `full`
	// grants exactly the authority the jail model exists to withhold — set it
	// only if you know why. Naming any role on a scion that has no --role flag
	// is a hard error, never a silent downgrade.
	AgentRole string `yaml:"agent_role"`
}

// scionAgentRoles is the set of roles scion#1089 defines. Validated so a typo
// fails at config load rather than as an opaque scion start error at bring-up.
var scionAgentRoles = []string{"none", "readonly", "baseline", "full"}

// Security holds opt-in image policy. Both default off (empty/false) for
// backward compatibility; when set they apply to manager.image and every worker
// image. See docs-site/_guides/security-model-config-trust.md §5.
type Security struct {
	// AllowedImageRegistries restricts where images may come from: an image is
	// allowed iff it equals, or is prefixed by "<entry>/", one of these entries
	// (a registry host and/or namespace prefix, e.g. "scionlocal" or
	// "ghcr.io/myorg"). Empty ⇒ no restriction.
	AllowedImageRegistries []string `yaml:"allowed_image_registries"`
	// RequireImageDigest requires every image to be pinned by digest
	// (`…@sha256:<hex>`) rather than a mutable tag. False ⇒ tags allowed.
	RequireImageDigest bool `yaml:"require_image_digest"`
}

// Operator configures the authenticated operator-directive channel. The
// zero value disables directives entirely (no allowed_signers = no channel).
type Operator struct {
	// AllowedSigners is the ssh-keygen allowed_signers file holding the
	// operator public keys (principal operator@<name>). Confined to the
	// instance directory.
	AllowedSigners string `yaml:"allowed_signers"`
	// SigningKey is the default private-key path used by `lever directive
	// send` on the host. Not confined (the key should live OUTSIDE the tree).
	SigningKey string `yaml:"signing_key"`
	// DirectiveExpiry / DirectiveExpiryMax follow Broker.GrantTTL's pattern.
	DirectiveExpiry    time.Duration `yaml:"directive_expiry"`
	DirectiveExpiryMax time.Duration `yaml:"directive_expiry_max"`
}

// Remote configures the opt-in remote-access proxy (`lever remote`): a
// host-loopback reverse proxy that injects a dedicated narrow PAT and is
// fronted by `tailscale serve`. Disabled by default; the hub web UI is only
// enabled (--enable-web) when this is on. See
// docs/superpowers/specs/2026-08-16-remote-agent-access-design.md.
type Remote struct {
	Enabled bool `yaml:"enabled"`
	// Port is the host loopback port the proxy binds. Zero = DefaultRemotePort
	// (EffectiveRemotePort). Must not collide with the broker's jail/admin
	// ports — validated.
	Port int `yaml:"port"`
	// BaseURL is the public tailnet origin the operator reaches this
	// instance on. It configures the PROXY, not the hub: its host becomes
	// the proxy's ServeHost, which every Origin-bearing request must match
	// (remoteproxy.Config.ServeHost). It is deliberately NOT forwarded to
	// the hub as --base-url — that flag would also become the agents'
	// SCION_HUB_ENDPOINT, which no jail agent can reach (see
	// scion.ServerOpts.EnableWeb). Required while remote is enabled; must
	// be an absolute https URL.
	BaseURL string `yaml:"base_url"`
	// AllowedUsers, when non-empty, pins requests to these Tailscale login
	// names (Tailscale-User-Login header injected by `tailscale serve`).
	AllowedUsers []string `yaml:"allowed_users"`
	// LoginPort is the HOST loopback port the local OIDC provider binds.
	//
	// It is deliberately NOT the port the hub dials. The hub dials a guest
	// loopback port (GuestLoginIssuerPort, mirrored from backend.GuestLoginIssuerPort), which a forwarder carries
	// to this one — two numbers, because OrbStack mirrors a guest listener
	// onto the host at the same number and one number for both halves left
	// the provider unable to bind its own port. The guest half has no config
	// key at all, so no configuration can make the two halves collide.
	//
	// Zero = DefaultRemoteLoginPort (EffectiveRemoteLoginPort). Validated against the proxy
	// port, the broker's listeners, and the guest port's host mirror.
	LoginPort int `yaml:"login_port"`
}

type App struct {
	Name     string      `yaml:"name"`
	Backend  string      `yaml:"backend"`
	Egress   EgressMode  `yaml:"egress"`
	Tree     string      `yaml:"tree"`
	Manager  Manager     `yaml:"manager"`
	Scion    ScionConfig `yaml:"scion"`
	Workers  []Worker    `yaml:"workers"`
	Security Security    `yaml:"security"`
	Broker   Broker      `yaml:"broker"`
	Operator Operator    `yaml:"operator"`
	Remote   Remote      `yaml:"remote"`
	Disk     string      `yaml:"disk"` // Lima guest disk size (e.g. "24GiB"); empty = backend default. Lima-only.

	dir string // instance root (the config file's directory)
}
