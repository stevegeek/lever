// Package config loads an application config: the declarative description of a
// lever agent-manager application (the manager + its groves).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/backend"
	"gopkg.in/yaml.v3"
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
}

// Tool declares a first-party (capability-aware) tool the broker supervises.
type Tool struct {
	Name          string              `yaml:"name"`
	Command       []string            `yaml:"command"`
	Backend       string              `yaml:"backend"`
	Operations    []Op                `yaml:"operations"`
	AllowedValues map[string][]string `yaml:"allowed_values"`
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

// Default broker ports, used when the config leaves jail_port/admin_port unset
// (0). They are fixed constants rather than dynamically allocated so the apply
// process and the separately-spawned `lever broker serve` (which re-reads the
// same config) agree on the ports without any cross-process plumbing. Set
// explicit ports to run more than one instance's broker on the host at once.
const (
	DefaultBrokerJailPort  = 8443
	DefaultBrokerAdminPort = 8444
)

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
	LLMUpstream string `yaml:"llm_upstream"`
	Tools       []Tool `yaml:"tools"`
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

type Grove struct {
	Name     string          `yaml:"name"`
	Dir      string          `yaml:"dir"`
	Image    string          `yaml:"image"` // optional; empty ⇒ inherit Manager.Image
	LLMAuth  LLMAuthMode     `yaml:"llm_auth"`
	Obtain   []Grant         `yaml:"obtain"`
	Delegate []DelegateGrant `yaml:"delegate"`
}

type ScionConfig struct {
	// Source is a host path to a scion source checkout to cross-compile into the
	// jail (local development). Mutually exclusive with Version.
	Source string `yaml:"source"`
	// Version pins a scion module version/commit (e.g. a commit hash or a
	// vX.Y.Z tag) that lever fetches via the Go module system and cross-compiles
	// into the jail — no vendored source tree. Mutually exclusive with Source.
	Version string `yaml:"version"`
}

// Security holds opt-in image policy. Both default off (empty/false) for
// backward compatibility; when set they apply to manager.image and every grove
// image. See security-model.md §5.
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

type App struct {
	Name     string      `yaml:"name"`
	Backend  string      `yaml:"backend"`
	Egress   EgressMode  `yaml:"egress"`
	Tree     string      `yaml:"tree"`
	Manager  Manager     `yaml:"manager"`
	Scion    ScionConfig `yaml:"scion"`
	Groves   []Grove     `yaml:"groves"`
	Security Security    `yaml:"security"`
	Broker   Broker      `yaml:"broker"`

	dir     string // instance root (the config file's directory)
	treeRel string // tree as the confined relative subdir (before joining to dir)
}

// validateBackend rejects a config's backend unless lever can run it. The set
// of valid backends lives in package backend (the single source; implemented
// only — roadmap/rejected backends are documentation), not a list duplicated
// here, so nothing is ever silently swapped for the default.
func validateBackend(name string) error {
	if name == "" {
		return fmt.Errorf("config: backend is required (valid: %s)", strings.Join(backend.Names(), ", "))
	}
	if _, ok := backend.Lookup(name); !ok {
		return fmt.Errorf("config: unknown backend %q (valid: %s)", name, strings.Join(backend.Names(), ", "))
	}
	return nil
}

// CanonicalName is the config filename for a lever instance — a manifest at the
// instance root (package.json / Cargo.toml style). It is resolved from the
// current directory ONLY — there is deliberately no walk-up discovery, so a
// `lever.yaml` planted in a parent directory can never be picked up. See
// security-model.md §5.
const CanonicalName = "lever.yaml"

// nameRE constrains an instance/grove name: it becomes a jail machine name
// (`lever-<name>`) and a shell token in the scion-install path.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// imageRE constrains a container image reference to safe OCI-ref characters
// (no whitespace or shell metacharacters).
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]*$`)

// digestRE matches an image pinned by content digest (e.g. `…@sha256:<hex>`).
var digestRE = regexp.MustCompile(`@[a-z0-9]+:[0-9a-fA-F]{32,}$`)

// validateImage checks an image ref against the charset, the optional registry
// allowlist, and the optional digest-pin requirement. field names the source
// for error messages (e.g. "manager.image").
func (s Security) validateImage(field, ref string) error {
	if !imageRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q has invalid characters", field, ref)
	}
	if len(s.AllowedImageRegistries) > 0 && !registryAllowed(ref, s.AllowedImageRegistries) {
		return fmt.Errorf("config: %s %q is not from an allowed registry (allowed: %s)", field, ref, strings.Join(s.AllowedImageRegistries, ", "))
	}
	if s.RequireImageDigest && !digestRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q must be pinned by digest (…@sha256:<hex>); a mutable tag is not allowed", field, ref)
	}
	return nil
}

// registryAllowed reports whether ref starts with one of the allowed prefixes,
// matched on whole path components (so "scionlocal" allows "scionlocal/x" but
// not "scionlocalevil/x").
func registryAllowed(ref string, prefixes []string) bool {
	for _, p := range prefixes {
		if ref == p || strings.HasPrefix(ref, p+"/") {
			return true
		}
	}
	return false
}

// confinedRel reports whether p is a relative path that stays strictly inside
// its base (not absolute, not ".", no ".." escape). Used for `tree` and
// `prompt_file` so neither can point outside the instance root.
func confinedRel(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// resolvePath expands a leading ~/ to the home dir, makes a relative path
// relative to baseDir, and returns an absolute path. Empty in -> empty out.
func resolvePath(p, baseDir string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	} else if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

func Load(path string) (*App, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var app App
	if err := yaml.Unmarshal(b, &app); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	app.dir = filepath.Dir(path)
	// `tree:` is the bind-mounted workspace and MUST be a confined subdirectory
	// of the instance root (the config's own directory). The root itself is NOT
	// mounted — it holds the config and the boot prompt, which must stay out of
	// the agent-writable mount (a compromised agent could otherwise rewrite the
	// config the host trusts on the next bring-up). So `tree: .` (root == mount)
	// is rejected. See security-model.md §5.
	if !confinedRel(app.Tree) {
		return nil, fmt.Errorf("config: tree %q must be a relative subdirectory inside the instance root (not %q, not absolute, no \"..\")", app.Tree, ".")
	}
	app.treeRel = app.Tree
	app.Tree = filepath.Join(app.dir, app.Tree)
	if abs, err := filepath.Abs(app.Tree); err == nil {
		app.Tree = abs
	}
	app.Scion.Source = resolvePath(app.Scion.Source, app.dir)
	if app.Scion.Source != "" && app.Scion.Version != "" {
		return nil, fmt.Errorf("config: scion.source and scion.version are mutually exclusive")
	}
	app.Manager.CredentialFile = resolvePath(app.Manager.CredentialFile, app.dir)
	app.injectLLMGrants()
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}

func (a *App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("config: name is required")
	}
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("config: name %q must match %s (it becomes the jail machine name and a shell token)", a.Name, nameRE)
	}
	if err := validateBackend(a.Backend); err != nil {
		return err
	}
	if a.Tree == "" {
		return fmt.Errorf("config: tree is required")
	}
	if a.Manager.Image != "" {
		if err := a.Security.validateImage("manager.image", a.Manager.Image); err != nil {
			return err
		}
	}
	// manager.allow_ports opens a host-loopback port to the jailed agent (via
	// the egress allowlist's per-port ACCEPT on the host alias); the broker's
	// admin port (/bootstrap, /revoke, /bump-epoch — unauthenticated, meant to
	// be reachable only from the host loopback) must never be listed there,
	// or the allowlist — the ONLY thing isolating that API from the guest —
	// would hand the jail a direct path to it.
	adminPort := a.EffectiveAdminPort()
	for _, p := range a.Manager.AllowPorts {
		if p == adminPort {
			return fmt.Errorf("config: manager.allow_ports must not include the broker admin port (%d) — this would hand the jailed agent a direct, unauthenticated path to /bootstrap, /revoke, /bump-epoch (the egress allowlist is the only thing isolating the host-loopback admin API from the guest)", adminPort)
		}
	}
	// prompt_file is host-only (read at the root, NOT in the mount) and must stay
	// inside the instance root.
	if a.Manager.PromptFile != "" && !confinedRel(a.Manager.PromptFile) {
		return fmt.Errorf("config: manager.prompt_file %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Manager.PromptFile)
	}
	for _, g := range a.Groves {
		if g.Name == "" || g.Dir == "" {
			return fmt.Errorf("config: grove needs name + dir (got %+v)", g)
		}
		if !nameRE.MatchString(g.Name) {
			return fmt.Errorf("config: grove name %q must match %s", g.Name, nameRE)
		}
		if filepath.IsAbs(g.Dir) || strings.HasPrefix(filepath.Clean(g.Dir), "..") {
			return fmt.Errorf("config: grove dir %q must be relative and inside the tree", g.Dir)
		}
		if g.Image != "" {
			if err := a.Security.validateImage(fmt.Sprintf("grove %q image", g.Name), g.Image); err != nil {
				return err
			}
		}
	}
	if err := a.validateBroker(); err != nil {
		return err
	}
	return nil
}

// validateBroker fails closed on capability-config mistakes: duplicate tool
// names, grants referencing an undeclared tool/op, and delegate.to naming an
// undeclared agent. A grove with no grants is fine (default-deny ⇒ no path).
func (a *App) validateBroker() error {
	// LLM-auth: validate the enum and, when any agent is api-key, require an
	// api_key_file that exists at 0600 (fail closed on a world/group-readable key).
	validMode := func(m LLMAuthMode) bool {
		return m == "" || m == LLMAuthSubscription || m == LLMAuthAPIKey
	}
	if !validMode(a.Broker.LLMAuth) {
		return fmt.Errorf("config: broker.llm_auth %q invalid (want subscription|api-key)", a.Broker.LLMAuth)
	}
	if !validMode(a.Manager.LLMAuth) {
		return fmt.Errorf("config: manager.llm_auth %q invalid (want subscription|api-key)", a.Manager.LLMAuth)
	}
	for _, g := range a.Groves {
		if !validMode(g.LLMAuth) {
			return fmt.Errorf("config: grove %s llm_auth %q invalid (want subscription|api-key)", g.Name, g.LLMAuth)
		}
	}
	// Mixed instances are UNSUPPORTED: the OAuth credential is a single jail-wide
	// Hub secret and egress is applied jail-wide, so a subscription agent forces
	// the real token into — and open internet egress for — the api-key agents'
	// containers, defeating their key isolation. An instance must be uniformly
	// api-key OR uniformly subscription. See security-model.md §6.1. (Resolved
	// modes, so a broker/manager default that disagrees with a grove override is
	// caught too.)
	if a.mixedLLMAuth() {
		return fmt.Errorf("config: llm_auth is mixed across the instance (both api-key and subscription agents) — this is unsupported; an instance must be uniformly api-key or uniformly subscription (see security-model.md §6.1)")
	}
	// Egress is an independent posture (not derived from llm_auth). `closed`
	// requires a uniformly api-key instance — a subscription agent needs direct
	// internet to reach Anthropic, which a closed jail forbids.
	switch a.Egress {
	case "", EgressOpen, EgressClosed:
	default:
		return fmt.Errorf("config: egress %q invalid (want open|closed)", a.Egress)
	}
	if a.Egress == EgressClosed {
		if _, anySub := a.llmAuthModes(); anySub {
			return fmt.Errorf("config: egress: closed requires every agent to be api-key (a subscription agent needs direct internet to reach Anthropic)")
		}
	}
	if a.AnyAPIKeyAgent() {
		if a.Broker.APIKeyFile == "" {
			return fmt.Errorf("config: broker.api_key_file is required when llm_auth is api-key")
		}
		fi, err := os.Stat(a.Broker.APIKeyFile)
		if err != nil {
			return fmt.Errorf("config: broker.api_key_file %q: %w", a.Broker.APIKeyFile, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			return fmt.Errorf("config: broker.api_key_file %q must be 0600, got %#o", a.Broker.APIKeyFile, perm)
		}
	}
	return a.validateBrokerGrants()
}

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
	for _, g := range a.Groves {
		mark(a.EffectiveGroveLLMAuth(g))
	}
	return
}

// mixedLLMAuth reports whether the instance mixes api-key and subscription
// agents — an unsupported configuration (see validateBroker / security-model.md §6.1).
func (a *App) mixedLLMAuth() bool {
	anyAPIKey, anySubscription := a.llmAuthModes()
	return anyAPIKey && anySubscription
}

// AnyAPIKeyAgent reports whether any agent (manager or grove) is api-key.
// Exported so brokerctl can decide whether to register the reserved llm pseudo-tool.
func (a *App) AnyAPIKeyAgent() bool {
	anyAPIKey, _ := a.llmAuthModes()
	return anyAPIKey
}

// injectLLMGrants adds the implicit obtain {llm, generate} capability to every
// api-key agent (R3). LLM access is universal in api-key mode; a grove opts out
// with llm_auth: subscription. Idempotent.
func (a *App) injectLLMGrants() {
	add := func(obtain *[]Grant) {
		for _, g := range *obtain {
			if g.Tool == "llm" && g.Op == "generate" {
				return
			}
		}
		*obtain = append(*obtain, Grant{Tool: "llm", Op: "generate"})
	}
	if a.EffectiveManagerLLMAuth() == LLMAuthAPIKey {
		add(&a.Manager.Obtain)
	}
	for i := range a.Groves {
		if a.EffectiveGroveLLMAuth(a.Groves[i]) == LLMAuthAPIKey {
			add(&a.Groves[i].Obtain)
		}
	}
}

// validateBrokerGrants validates tool declarations, grant references, and
// delegate targets. Called by validateBroker after the LLM-auth block.
func (a *App) validateBrokerGrants() error {
	// Known tools + their op sets.
	toolOps := map[string]map[string]bool{}
	// Built-in reserved pseudo-tool: llm (broker /llm proxy, no backend subprocess).
	toolOps["llm"] = map[string]bool{"generate": true}
	for _, t := range a.Broker.Tools {
		if t.Name == "" {
			return fmt.Errorf("config: broker.tools entry has empty name")
		}
		if _, dup := toolOps[t.Name]; dup {
			return fmt.Errorf("config: duplicate broker tool %q", t.Name)
		}
		ops := map[string]bool{}
		for _, o := range t.Operations {
			ops[o.Name] = true
		}
		toolOps[t.Name] = ops
	}
	// Known agent identities: the manager CN + every grove name.
	agents := map[string]bool{a.ManagerCN(): true}
	for _, g := range a.Groves {
		agents[g.Name] = true
	}
	checkCap := func(who, tool, op string) error {
		ops, ok := toolOps[tool]
		if !ok {
			return fmt.Errorf("config: %s grants tool %q which is not a declared broker.tool", who, tool)
		}
		if !ops[op] {
			return fmt.Errorf("config: %s grants %q on tool %q which has no such operation", who, op, tool)
		}
		return nil
	}
	validate := func(who string, obtain []Grant, delegate []DelegateGrant) error {
		for _, g := range obtain {
			if err := checkCap(who+".obtain", g.Tool, g.Op); err != nil {
				return err
			}
		}
		for _, d := range delegate {
			if err := checkCap(who+".delegate", d.Tool, d.Op); err != nil {
				return err
			}
			for _, to := range d.To {
				if !agents[to] {
					return fmt.Errorf("config: %s delegates to %q which is not a declared agent", who, to)
				}
			}
		}
		return nil
	}
	if err := validate("manager", a.Manager.Obtain, a.Manager.Delegate); err != nil {
		return err
	}
	for _, g := range a.Groves {
		if err := validate("grove "+g.Name, g.Obtain, g.Delegate); err != nil {
			return err
		}
	}
	return nil
}

// GroveDir returns the absolute path of a grove dir (tree + relative dir).
func (a *App) GroveDir(g Grove) string { return filepath.Join(a.Tree, g.Dir) }

// EffectiveManagerLLMAuth resolves the manager's LLM-auth mode: the broker
// default (subscription when unset).
func (a *App) EffectiveManagerLLMAuth() LLMAuthMode {
	if a.Manager.LLMAuth != "" {
		return a.Manager.LLMAuth
	}
	return a.brokerLLMAuthDefault()
}

// EffectiveGroveLLMAuth resolves a grove's LLM-auth mode: its own override else
// the broker default.
func (a *App) EffectiveGroveLLMAuth(g Grove) LLMAuthMode {
	if g.LLMAuth != "" {
		return g.LLMAuth
	}
	return a.brokerLLMAuthDefault()
}

// EffectiveJailPort is the broker's in-jail mTLS port: the configured value, or
// DefaultBrokerJailPort when unset (0).
func (a *App) EffectiveJailPort() int {
	if a.Broker.JailPort != 0 {
		return a.Broker.JailPort
	}
	return DefaultBrokerJailPort
}

// EffectiveAdminPort is the broker's loopback admin port: the configured value,
// or DefaultBrokerAdminPort when unset (0).
func (a *App) EffectiveAdminPort() int {
	if a.Broker.AdminPort != 0 {
		return a.Broker.AdminPort
	}
	return DefaultBrokerAdminPort
}

func (a *App) brokerLLMAuthDefault() LLMAuthMode {
	if a.Broker.LLMAuth != "" {
		return a.Broker.LLMAuth
	}
	return LLMAuthAPIKey
}

// ClosedInternetEgress reports the jail's egress posture, applied jail-wide. It
// is an explicit, independent knob (App.Egress) — NOT derived from llm_auth:
// closed iff `egress: closed` is set. validateBroker guarantees `closed`
// implies a uniformly api-key instance, so a subscription agent is never left
// unable to reach Anthropic. The warning return is retained for the apply call
// site but is always empty now that the posture is explicit.
func (a *App) ClosedInternetEgress() (closed bool, warning string) {
	return a.Egress == EgressClosed, ""
}

// GroveImage returns the container image a grove should run on: its own
// `image:` if set, else the manager image (the common single-image case, and
// the image apply already loads into the jail). The manager dispatches groves
// later, so this is the single source of truth both apply (what to load) and
// lever-manager (what to pass to `scion start`) resolve against.
func (a *App) GroveImage(g Grove) string {
	if g.Image != "" {
		return g.Image
	}
	return a.Manager.Image
}

// GroveByName returns the configured grove with the given name, or false.
func (a *App) GroveByName(name string) (Grove, bool) {
	for _, g := range a.Groves {
		if g.Name == name {
			return g, true
		}
	}
	return Grove{}, false
}

// ManagerCN returns the manager's cert CN (broker.manager_identity, default "manager").
func (a *App) ManagerCN() string {
	if a.Broker.ManagerIdentity != "" {
		return a.Broker.ManagerIdentity
	}
	return "manager"
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
