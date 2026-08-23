package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/broker/registry"
	"gopkg.in/yaml.v3"
)

// CanonicalName is the config filename for a lever instance — a manifest at the
// instance root (package.json / Cargo.toml style). It is resolved from the
// current directory ONLY — there is deliberately no walk-up discovery, so a
// `lever.yaml` planted in a parent directory can never be picked up. See
// docs-site/_guides/security-model-config-trust.md §5.
const CanonicalName = "lever.yaml"

// ManifestName is the filename of the legacy sanitized runtime manifest the host
// used to write INTO the mount at apply time. The file is no longer written by
// `lever apply` (the manifest was removed as write-only dead code). The const is
// retained so `lever down` can remove any legacy manifest left from a prior
// version (see internal/cli/down.go clearStagedRuntimeState).
const ManifestName = ".lever-manifest.yaml"

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
	// is rejected. See docs-site/_guides/security-model-config-trust.md §5.
	if !confinedRel(app.Tree) {
		return nil, fmt.Errorf("config: tree %q must be a relative subdirectory inside the instance root (not %q, not absolute, no \"..\")", app.Tree, ".")
	}
	app.Tree = filepath.Join(app.dir, app.Tree)
	if abs, err := filepath.Abs(app.Tree); err == nil {
		app.Tree = abs
	}
	app.Scion.Source = resolvePath(app.Scion.Source, app.dir)
	app.Scion.Binary = resolvePath(app.Scion.Binary, app.dir)
	// At most one scion mode. NOT "exactly one": config.Load also runs for
	// commands that never bring anything up (doctor, msg, attach), and a
	// minimal config legitimately declares no scion block at all — the missing
	// mode is reported at bring-up by resolveScionBinary instead.
	scionModes := 0
	for _, v := range []string{app.Scion.Binary, app.Scion.Source, app.Scion.Version} {
		if v != "" {
			scionModes++
		}
	}
	if scionModes > 1 {
		return nil, fmt.Errorf("config: scion.binary, scion.source and scion.version are mutually exclusive")
	}
	// Neither the prebuilt binary nor the source checkout may live inside the
	// mounted tree. Whatever they point at is cross-compiled or installed as
	// root at /usr/local/bin/scion and becomes the engine every agent runs
	// under — and the tree is exactly the place agents CAN write. An in-tree
	// path would let a compromised agent choose its own engine on the next
	// bring-up.
	for _, m := range []struct{ key, path string }{
		{"scion.binary", app.Scion.Binary},
		{"scion.source", app.Scion.Source},
	} {
		if m.path == "" {
			continue
		}
		if rel, err := filepath.Rel(app.Tree, m.path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("config: %s (%s) is inside the mounted tree (%s); agents can write there, and it would be installed as the engine they run under",
				m.key, m.path, app.Tree)
		}
	}
	if r := app.Scion.AgentRole; r != "" && !slices.Contains(scionAgentRoles, r) {
		return nil, fmt.Errorf("config: unknown scion.agent_role %q (valid: %s; omit it entirely to let scion choose)",
			r, strings.Join(scionAgentRoles, ", "))
	}
	app.Manager.CredentialFile = resolvePath(app.Manager.CredentialFile, app.dir)
	// APIKeyFile is a host-side secret like CredentialFile: expanded against
	// the instance dir (and ~/), not confined, because the key should live
	// outside the agent-writable tree.
	app.Broker.APIKeyFile = resolvePath(app.Broker.APIKeyFile, app.dir)
	// SigningKey is a host-side path OUTSIDE the instance tree by design (the
	// operator's private key must never live where a compromised agent could
	// read it) — expanded like CredentialFile/Scion.Source, but deliberately
	// NOT confined.
	app.Operator.SigningKey = resolvePath(app.Operator.SigningKey, app.dir)
	app.injectLLMGrants()
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}

// injectLLMGrants adds the implicit obtain {llm, generate} capability to every
// api-key agent (R3). LLM access is universal in api-key mode; a worker opts out
// with llm_auth: subscription. Idempotent.
func (a *App) injectLLMGrants() {
	add := func(obtain *[]Grant) {
		for _, g := range *obtain {
			if g.Tool == registry.ReservedLLMTool && g.Op == registry.ReservedLLMOp {
				return
			}
		}
		*obtain = append(*obtain, Grant{Tool: registry.ReservedLLMTool, Op: registry.ReservedLLMOp})
	}
	if a.EffectiveManagerLLMAuth() == LLMAuthAPIKey {
		add(&a.Manager.Obtain)
	}
	for i := range a.Workers {
		if a.EffectiveWorkerLLMAuth(a.Workers[i]) == LLMAuthAPIKey {
			add(&a.Workers[i].Obtain)
		}
	}
}
