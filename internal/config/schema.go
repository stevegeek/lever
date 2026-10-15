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

// Op declares one operation of a first-party tool. CaveatParam, when set, is
// a guard: the broker checks at registration that the tool-shipped map
// equals it. Empty accepts whatever the tool ships.
type Op struct {
	Name        string            `yaml:"name"`
	CaveatParam map[string]string `yaml:"caveat_param"`
	// Params optionally declares the operation's argument names. When set,
	// the broker rejects mint constraints on any other key at mint time; a
	// mistyped key would otherwise mint a token that fails closed only at
	// call time, far from the mistake.
	Params []string `yaml:"params"`
}

// Tool declares a tool the broker gates behind its mTLS tool routes
// (/mcp/<name>/): a first-party subprocess the broker supervises, or, with
// External, an already-running host MCP server the broker fronts.
type Tool struct {
	Name          string              `yaml:"name"`
	Command       []string            `yaml:"command"`
	Backend       string              `yaml:"backend"`
	Operations    []Op                `yaml:"operations"`
	AllowedValues map[string][]string `yaml:"allowed_values"`
	// External marks a host MCP server the broker fronts but does not spawn
	// (its lifecycle and any TCC grant stay with the user session). The broker
	// enforces the rules and strips the capability token. Command must be
	// empty; Backend is host:port[/path], loopback unless AllowNonLoopback.
	External bool `yaml:"external"`
	// Gate is only valid on an external tool.
	Gate Gate `yaml:"gate"`
	// AllowNonLoopback permits an external Backend beyond a literal loopback
	// IP. The broker proxies host-side, so a non-loopback backend gives the
	// jailed agent a path to another host through the broker, bypassing the
	// jail's LAN-drop egress.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

// EffectiveGate is the declared gate, or GateFine when unset.
func (t Tool) EffectiveGate() Gate {
	return cmp.Or(t.Gate, GateFine)
}

// LLMAuthMode selects how an agent authenticates to the Anthropic API.
//   - api-key (the default): the container holds only a capability token and
//     reaches the model through the broker /llm proxy, which injects the real
//     key host-side; the real key never enters the container.
//   - subscription: the container reaches api.anthropic.com directly with the
//     owner's OAuth credential, which is projected into the container.
type LLMAuthMode string

const (
	LLMAuthSubscription LLMAuthMode = "subscription"
	LLMAuthAPIKey       LLMAuthMode = "api-key"
)

func (m LLMAuthMode) valid() bool {
	return m == "" || m == LLMAuthSubscription || m == LLMAuthAPIKey
}

// Gate selects the capability grain the broker enforces on an external tool.
//   - fine (the default): only the declared operations are callable; a token
//     must name the MCP tool being invoked.
//   - coarse: one wildcard capability ({tool, "*"}) admits every MCP call the
//     server exposes, trusting its whole surface.
type Gate string

const (
	GateFine   Gate = "fine"
	GateCoarse Gate = "coarse"
)

func (g Gate) valid() bool {
	return g == "" || g == GateFine || g == GateCoarse
}

// EgressMode selects the jail's outbound network posture.
//   - open (the default): the LAN and non-allowlisted host-alias ports are
//     dropped, the public internet stays reachable.
//   - closed: a catch-all DROP, so the jail reaches only the broker port.
//     Requires a uniformly api-key instance (a subscription agent needs
//     direct internet to reach Anthropic).
type EgressMode string

const (
	EgressOpen   EgressMode = "open"
	EgressClosed EgressMode = "closed"
)

func (m EgressMode) valid() bool {
	return m == "" || m == EgressOpen || m == EgressClosed
}

// Messaging is the broker's message-routing policy, not capability grants:
// identities come from mTLS, recipients from config.
type Messaging struct {
	// WorkerToWorker permits worker-to-worker sends; unset means allowed.
	WorkerToWorker *bool `yaml:"worker_to_worker"`
}

type Broker struct {
	JailPort        int           `yaml:"jail_port"`
	AdminPort       int           `yaml:"admin_port"`
	GrantTTL        time.Duration `yaml:"grant_ttl"`
	TicketTTL       time.Duration `yaml:"ticket_ttl"`
	ManagerIdentity string        `yaml:"manager_identity"`
	APIKeyFile      string        `yaml:"api_key_file"` // api-key mode
	LLMAuth         LLMAuthMode   `yaml:"llm_auth"`
	// LLMUpstream overrides the /llm proxy target, for tests; never
	// client-controlled.
	LLMUpstream string    `yaml:"llm_upstream"`
	Tools       []Tool    `yaml:"tools"`
	Messaging   Messaging `yaml:"messaging"`
	// AutoReenrol selects which agents the broker re-enrols after an mTLS
	// leaf merely expires (never a revoked or epoch-stale identity).
	AutoReenrol AutoReenrolMode `yaml:"auto_reenrol"`
	// MaxRunningWorkers caps how many workers may run or start at once (see
	// broker.admitWorker); 0 is no cap, and the manager does not count. The
	// json tag keeps it out of brokerctl.ConfigHash while unset.
	MaxRunningWorkers int `yaml:"max_running_workers" json:",omitempty"`
}

// AutoReenrolMode selects which agents the broker's natural-lapse healer
// covers.
type AutoReenrolMode string

const (
	AutoReenrolAll     AutoReenrolMode = "all"
	AutoReenrolManager AutoReenrolMode = "manager"
	AutoReenrolOff     AutoReenrolMode = "off"
)

func (m AutoReenrolMode) valid() bool {
	return m == "" || m == AutoReenrolAll || m == AutoReenrolManager || m == AutoReenrolOff
}

type Manager struct {
	Image string `yaml:"image"`
	// ImageTar is an optional `docker save` archive carrying Image, streamed
	// into the jail so a deploy host needs no docker. Host-only and
	// root-confined like PromptFile: the tar decides what code runs as the
	// agent, so an agent in the mounted tree must not be able to author it.
	ImageTar string `yaml:"image_tar"`
	// Model is passed verbatim as `scion start --model`. Empty leaves the
	// choice to scion, whose default can move with the pin.
	Model      string `yaml:"model"`
	PromptFile string `yaml:"prompt_file"`
	// InstructionsFile is the agent's standing instructions, delivered through
	// scion's `agent_instructions` channel into the agent's user-level
	// ~/.claude/CLAUDE.md, never through argv: PromptFile, the boot task,
	// rides scion's argv into a single tmux word capped at 16 KiB. Host-only
	// and root-confined like PromptFile. Create-time only.
	InstructionsFile string `yaml:"instructions_file"`
	// ReadOnly lists tree directories (tree-relative, forward slashes) the
	// manager container sees read-only: a tree can hold code the host also
	// runs, and an agent that rewrites it gets code execution on the host.
	// Each entry is bind-mounted read-only over itself, and each strict
	// ancestor up to the tree root read-write over itself, because a mount
	// point cannot be renamed or removed: without that pin the agent renames
	// the parent away and creates a writable directory at the protected path.
	// Workers never see these mounts, so no worker dir may overlap an entry.
	// Every entry must be a real directory reached through no symlink.
	// Create-time only: scion stores a record's volumes for life.
	ReadOnly []string `yaml:"read_only"`
	// ClaudeSettings and AfterCompactNote are applied at every start, a
	// resume included.
	ClaudeSettings   ClaudeSettings `yaml:"claude_settings"`
	AfterCompactNote string         `yaml:"after_compact_note"`
	// DailyReset is "HH:MM", host local time: the broker starts the manager
	// on a new conversation once a day then. Empty is off.
	DailyReset string `yaml:"daily_reset"`
	// Resources is create-time only.
	Resources      Resources       `yaml:"resources"`
	AllowPorts     []int           `yaml:"allow_ports"`
	CredentialFile string          `yaml:"credential_file"`
	LLMAuth        LLMAuthMode     `yaml:"llm_auth"`
	Obtain         []Grant         `yaml:"obtain"`
	Delegate       []DelegateGrant `yaml:"delegate"`
}

type Worker struct {
	Name  string `yaml:"name"`
	Dir   string `yaml:"dir"`
	Image string `yaml:"image"` // empty inherits Manager.Image
	// ImageTar requires Image; a worker with no Image inherits the manager's
	// image and its tar.
	ImageTar string `yaml:"image_tar"`
	Model    string `yaml:"model"` // empty inherits Manager.Model
	// InstructionsFile is not inherited from the manager: the manager's
	// manual describes orchestration authority a worker must not hold.
	InstructionsFile string `yaml:"instructions_file"`
	// ClaudeSettings and AfterCompactNote are not inherited from the manager.
	// The json tags keep them out of brokerctl.ConfigHash while unset.
	ClaudeSettings   ClaudeSettings  `yaml:"claude_settings" json:",omitzero"`
	AfterCompactNote string          `yaml:"after_compact_note" json:",omitempty"`
	LLMAuth          LLMAuthMode     `yaml:"llm_auth"`
	Obtain           []Grant         `yaml:"obtain"`
	Delegate         []DelegateGrant `yaml:"delegate"`
	// Recyclable lets the manager discard this worker's record and start it
	// fresh with a new task; its workspace is kept. Without it only the
	// operator's `lever worker purge` discards a record. The json tag keeps
	// it out of brokerctl.ConfigHash while false.
	Recyclable bool `yaml:"recyclable" json:",omitempty"`
	// Resources is not inherited from the manager and is create-time only.
	// The json tag keeps it out of brokerctl.ConfigHash while unset.
	Resources Resources `yaml:"resources" json:",omitzero"`
}

type ScionConfig struct {
	// Binary is a host path to an already-built linux scion binary, installed
	// as-is, so the jail host needs no Go toolchain or egress. Mutually
	// exclusive with Source and Version.
	Binary string `yaml:"binary"`
	// Source is a host path to a scion source checkout to cross-compile.
	// Mutually exclusive with Version.
	Source string `yaml:"source"`
	// Version pins a scion module version or commit that lever fetches and
	// cross-compiles.
	Version string `yaml:"version"`
	// AgentRole overrides the role lever passes to `scion start`. Empty means
	// `baseline` whenever the installed scion has a --role flag. lever probes
	// the binary for the flag rather than guessing from the pin, because
	// scion's default for an unspecified role is full, which would give an
	// agent hub authority (agent create, lifecycle, project-secret-read).
	//
	// `baseline` is heartbeat and self-token-refresh only. `readonly` cannot
	// heartbeat, so a live agent cannot run on it. `full` grants exactly the
	// authority the jail model exists to withhold. Naming any role on a scion
	// with no --role flag is a hard error, never a silent downgrade.
	AgentRole string `yaml:"agent_role"`
	// Telemetry selects the jail's ~/.scion/settings.yaml `telemetry:` block,
	// which scion turns into every agent's SCION_TELEMETRY_* env.
	Telemetry ScionTelemetryMode `yaml:"telemetry"`
}

// ScionTelemetryMode selects the agents' sciontool telemetry posture.
//
//   - off (the default): lever writes `telemetry.enabled: false`. sciontool
//     telemetry defaults on, but with no cloud destination the in-container
//     OTLP receiver does not start, so every Claude hook (every tool call
//     included) dials 127.0.0.1:4317, is refused, and waits out a ~10 s export
//     and a 5 s shutdown timeout. Without a cloud exporter nothing is kept
//     anyway.
//   - scion-default: lever leaves the key to scion and the operator, removing
//     only the exact block `off` wrote. A cloud destination is new egress and
//     a credential in the jail, so lever does not manage it.
type ScionTelemetryMode string

const (
	ScionTelemetryOff          ScionTelemetryMode = "off"
	ScionTelemetryScionDefault ScionTelemetryMode = "scion-default"
)

func (m ScionTelemetryMode) valid() bool {
	return m == "" || m == ScionTelemetryOff || m == ScionTelemetryScionDefault
}

// scionAgentRoles are scion's roles, checked so a typo fails at config load
// rather than as an opaque scion start error.
var scionAgentRoles = []string{"none", "readonly", "baseline", "full"}

// Security holds opt-in image policy, applied to the manager's and every
// worker's image.
type Security struct {
	// AllowedImageRegistries allows an image that equals an entry or is
	// prefixed by "<entry>/". Empty is no restriction.
	AllowedImageRegistries []string `yaml:"allowed_image_registries"`
	// RequireImageDigest requires every image to be pinned by digest rather
	// than a mutable tag.
	RequireImageDigest bool `yaml:"require_image_digest"`
}

// Operator configures the authenticated operator-directive channel. With no
// AllowedSigners there is no channel.
type Operator struct {
	// AllowedSigners is the ssh-keygen allowed_signers file holding the
	// operator public keys. Confined to the instance directory.
	AllowedSigners string `yaml:"allowed_signers"`
	// SigningKey is the default private key for `lever directive send`. Not
	// confined; the key should live outside the tree.
	SigningKey         string        `yaml:"signing_key"`
	DirectiveExpiry    time.Duration `yaml:"directive_expiry"`
	DirectiveExpiryMax time.Duration `yaml:"directive_expiry_max"`
}

// SSH is the opt-in `lever ssh`: an SSH session into an agent over scion's
// hub relay (conduit), for the host operator only. Off, lever keeps the
// relay pinned off. On, it needs a scion with the hub's conduit controls,
// and lever keeps the relay off until it can prove they are in force.
type SSH struct {
	Enabled bool `yaml:"enabled"`
	// DenyPorts are agent-local ports no tunnel or SSH forward may reach,
	// added to the ones lever always denies (its own listeners in the agent).
	DenyPorts []int `yaml:"deny_ports"`
}

// Remote configures the opt-in remote-access proxy: a host-side reverse
// proxy that injects the operator's hub session behind an authenticating
// front that sets a verified identity header. The hub web UI is enabled only
// when this is on.
type Remote struct {
	Enabled bool `yaml:"enabled"`
	// Port is the host loopback port the proxy binds.
	Port int `yaml:"port"`
	// BaseURL is the public origin the operator reaches this instance on. Its
	// host becomes the proxy's ServeHost, which every Origin-bearing request
	// must match. It is not passed to the hub as --base-url, which would also
	// become the agents' SCION_HUB_ENDPOINT, unreachable from the jail.
	BaseURL string `yaml:"base_url"`
	// AllowedUsers, when non-empty, pins requests to these logins, matched
	// exactly against the IdentityHeader value. The same list names the hub
	// users the login path creates. See RemoteUser.
	AllowedUsers []RemoteUser `yaml:"allowed_users"`
	// IdentityHeader is the header the authenticating front puts the
	// verified login in. The proxy trusts it only because only the front can
	// reach the listener, and strips it before forwarding to the hub. It may
	// not name a header that carries a browser credential, routing, or an
	// identity scion reads.
	IdentityHeader string `yaml:"identity_header"`
	// Bind is the address the proxy listens on. Whatever can reach it can
	// assert any identity header, so a non-loopback address must be one the
	// jail's egress rules drop, or the wildcard with AllowWildcardBind. The
	// login provider always stays on loopback.
	Bind string `yaml:"bind"`
	// AllowWildcardBind acknowledges a wildcard Bind, which listens on every
	// host address, including public ones the jail can reach under open
	// egress.
	AllowWildcardBind bool `yaml:"allow_wildcard_bind"`
	// TrustForwardedHost makes the proxy's DNS-rebinding Host check read
	// X-Forwarded-Host on a request whose Host is an IP address, for a front
	// that rewrites Host to an IP. A rebind sends a name in Host, so it stays
	// refused. Off by default: anything that reaches the listener directly
	// can set that header.
	TrustForwardedHost bool `yaml:"trust_forwarded_host"`
	// LoginPort is parsed so an old lever.yaml still loads, and otherwise
	// ignored (App.Warnings names it).
	LoginPort int `yaml:"login_port"`
	// Landing is what the remote origin opens on: the hub's web UI
	// (RemoteLandingConsole, the default) or lever's chat page
	// (RemoteLandingChat). Chat needs an operator-tier allowed_users entry:
	// with no verified login, nothing the page sends would verify.
	Landing string `yaml:"landing"`
	// LabelsFile is a tree-relative JSON file the manager writes, mapping
	// agent names to labels for the chat page. It is jail-written data: read
	// with no symlink on the path, bounded, and only ever shown as text.
	LabelsFile string `yaml:"labels_file"`
	// AgentMessages lets an agent start a message to a contact that lists it,
	// and makes the proxy show a contact only the agent messages lever
	// recorded.
	AgentMessages AgentMessages `yaml:"agent_messages"`
	// Push, Files and Voice need landing: chat.
	Push  Push  `yaml:"push"`
	Files Files `yaml:"files"`
	Voice Voice `yaml:"voice"`
}

// Voice is remote.voice: dictation on the chat page, transcribed by the
// broker tool lever-tool-whisper. The text lands in the message box for
// review, never sent by itself. Audio and transcripts are never stored.
type Voice struct {
	Enabled bool `yaml:"enabled"`
	// Socket must equal the -dictate-socket of the lever-tool-whisper entry
	// in broker.tools, an absolute host path outside the tree.
	Socket string `yaml:"socket"`
	// MaxSeconds bounds one clip; at most the tool's -max-seconds.
	MaxSeconds int `yaml:"max_seconds"`
	// ReadAloud is how the read-aloud button on agent messages speaks. YAML
	// true and false read as device and off.
	ReadAloud ReadAloud `yaml:"read_aloud"`
	// SpeakSocket must equal the -speak-socket of the lever-tool-tts entry in
	// broker.tools, an absolute host path outside the tree.
	SpeakSocket string `yaml:"speak_socket"`
}

const (
	DefaultVoiceMaxSeconds = 300
	MaxVoiceMaxSeconds     = 600
)

// Push is remote.push.
type Push struct {
	Enabled bool `yaml:"enabled"`
	// Subject is the VAPID contact the push services see (RFC 8292).
	Subject string `yaml:"subject"`
	// TestHosts is test only: loopback addresses of a fake push service. The
	// proxy admits them only when LEVER_PUSH_TEST_HOSTS in its environment
	// names the same addresses. `lever doctor` fails while it is set.
	TestHosts []string `yaml:"test_hosts"`
}

// AgentMessages is remote.agent_messages.
type AgentMessages struct {
	Enabled bool `yaml:"enabled"`
	// FollowUpAfter is how long after an unanswered message an agent started
	// it may send one reminder.
	FollowUpAfter time.Duration `yaml:"follow_up_after"`
	MaxChars      int           `yaml:"max_chars"`
}

const (
	DefaultAgentFollowUpAfter = 24 * time.Hour
	DefaultAgentMaxChars      = 4000
	// MaxAgentMaxChars is scion's own cap on one message
	// (messages.MaxMessageLength): a longer text could never be sent.
	MaxAgentMaxChars = 16000
)

// Files is remote.files.
type Files struct {
	Enabled  bool  `yaml:"enabled"`
	MaxBytes int64 `yaml:"max_bytes"`
	// Extensions are lowercase, without the dot.
	Extensions []string `yaml:"extensions"`
	// Uploads and Shares turn one direction off (nil is on). A share already
	// made is not downloadable while Shares is off.
	Uploads *bool `yaml:"uploads"`
	Shares  *bool `yaml:"shares"`
}

const (
	DefaultFilesMaxBytes = 25 << 20
	// MaxFilesMaxBytes bounds max_bytes: a download is copied whole to a
	// private temp file before it is served (chatfiles.CopyVerified).
	MaxFilesMaxBytes = 100 << 20
)

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
	// SharedFolders are tree directories some agents mount at /shared/<name>.
	SharedFolders []SharedFolder `yaml:"shared_folders"`
	Disk          string         `yaml:"disk"`        // Lima guest disk size (e.g. "24GiB"); empty = backend default. Lima-only.
	NestedVirt    bool           `yaml:"nested_virt"` // Lima-only: the manager container gets /dev/kvm (nested KVM guests). Linux hosts only.
	CPUs          int            `yaml:"cpus"`        // Lima guest vCPUs; 0 = Lima default. Lima-only.
	Memory        string         `yaml:"memory"`      // Lima guest memory (e.g. "24GiB"); empty = Lima default. Lima-only.
	// GuestKernel picks the Lima guest's Ubuntu kernel, "hwe" or "ga".
	GuestKernel string `yaml:"guest_kernel"`
	// GuestProfile "small" trims the Lima guest for a small host; see
	// EffectiveGuestProfile.
	GuestProfile string `yaml:"guest_profile"`
	// TreeBackup snapshots the tree on the host (off by default).
	TreeBackup TreeBackup `yaml:"tree_backup"`
	// SSH opts the operator into `lever ssh` (off by default).
	SSH SSH `yaml:"ssh"`

	dir string // instance root (the config file's directory)
}
