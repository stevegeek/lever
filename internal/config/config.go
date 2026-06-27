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
//   - subscription: the container reaches api.anthropic.com directly with the
//     owner's subscription credentials (dev/personal carve-out).
//   - api-key: the container holds only a capability(llm) token and reaches the
//     model through the broker /llm proxy, which injects the real Console key.
type LLMAuthMode string

const (
	LLMAuthSubscription LLMAuthMode = "subscription"
	LLMAuthAPIKey       LLMAuthMode = "api-key"
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
	Tools           []Tool        `yaml:"tools"`
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
	Source string `yaml:"source"`
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
	Tree     string      `yaml:"tree"`
	Manager  Manager     `yaml:"manager"`
	Scion    ScionConfig `yaml:"scion"`
	Groves   []Grove     `yaml:"groves"`
	Security Security    `yaml:"security"`
	Broker   Broker      `yaml:"broker"`

	dir     string // instance root (the config file's directory)
	treeRel string // tree as the confined relative subdir (before joining to dir)
}

var knownBackends = map[string]bool{"orbstack": true, "linux-docker": true}

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
	app.Manager.CredentialFile = resolvePath(app.Manager.CredentialFile, app.dir)
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
	if !knownBackends[a.Backend] {
		return fmt.Errorf("config: unknown backend %q (known: orbstack, linux-docker)", a.Backend)
	}
	if a.Tree == "" {
		return fmt.Errorf("config: tree is required")
	}
	if a.Manager.Image != "" {
		if err := a.Security.validateImage("manager.image", a.Manager.Image); err != nil {
			return err
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
	if closed, _ := a.ClosedInternetEgress(); closed || a.anyAPIKeyAgent() {
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

// anyAPIKeyAgent reports whether any agent (manager or grove) is api-key.
func (a *App) anyAPIKeyAgent() bool {
	if a.EffectiveManagerLLMAuth() == LLMAuthAPIKey {
		return true
	}
	for _, g := range a.Groves {
		if a.EffectiveGroveLLMAuth(g) == LLMAuthAPIKey {
			return true
		}
	}
	return false
}

// validateBrokerGrants validates tool declarations, grant references, and
// delegate targets. Called by validateBroker after the LLM-auth block.
func (a *App) validateBrokerGrants() error {
	// Known tools + their op sets.
	toolOps := map[string]map[string]bool{}
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

func (a *App) brokerLLMAuthDefault() LLMAuthMode {
	if a.Broker.LLMAuth != "" {
		return a.Broker.LLMAuth
	}
	return LLMAuthSubscription
}

// ClosedInternetEgress resolves the instance-level egress posture (R2). Egress
// is applied jail-wide, not per container, so the jail can be closed only if
// EVERY agent is api-key. Any subscription agent forces the open posture and a
// warning that api-key groves in this instance are not egress-isolated (their
// key-isolation via the proxy still holds; only the belt-and-suspenders egress
// is relaxed).
func (a *App) ClosedInternetEgress() (closed bool, warning string) {
	modes := []LLMAuthMode{a.EffectiveManagerLLMAuth()}
	for _, g := range a.Groves {
		modes = append(modes, a.EffectiveGroveLLMAuth(g))
	}
	anyAPIKey, anySubscription := false, false
	for _, m := range modes {
		switch m {
		case LLMAuthAPIKey:
			anyAPIKey = true
		default:
			anySubscription = true
		}
	}
	if anyAPIKey && anySubscription {
		return false, "llm_auth mixed across instance: api-key agents are not egress-isolated from direct Anthropic reach (key isolation via the proxy still holds); per-container egress is not yet implemented"
	}
	return anyAPIKey && !anySubscription, ""
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
