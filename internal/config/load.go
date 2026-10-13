package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/wire"
	"gopkg.in/yaml.v3"
)

// CanonicalName is the config filename at a lever instance root. It is
// resolved from the current directory only, with no walk-up discovery, so a
// `lever.yaml` planted in a parent directory is never picked up.
const CanonicalName = "lever.yaml"

// ManifestName is a legacy runtime manifest in the mount that `lever down`
// removes when it finds one.
const ManifestName = ".lever-manifest.yaml"

// confinedRel reports whether p is a relative path strictly inside its base.
// It does not keep a file out of the mounted tree; insideTree does that.
func confinedRel(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// insideTree is lexical: callers pass both sides through resolveExisting
// first, so a symlink on either side has already been followed.
func insideTree(tree, path string) bool {
	rel, err := filepath.Rel(tree, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// insideTreeByIdentity is insideTree OR-ed with fsutil.InsideByIdentity,
// which also sees a case alias, a firmlink or a bind mount of the tree. A
// path that does not exist is judged by its deepest existing ancestor. An
// identity check that fails is an error: the caller refuses the path.
func insideTreeByIdentity(realTree, path string) (bool, error) {
	if insideTree(realTree, path) {
		return true, nil
	}
	return fsutil.InsideByIdentity(path, realTree)
}

func errCannotPlace(key, path string, err error) error {
	return fmt.Errorf("config: %s (%s): lever cannot tell whether it lies in the mounted tree (%w), and it refuses "+
		"what it cannot check — make the path's directories searchable by you, or move it", key, path, err)
}

// resolveExisting follows every symlink in p's deepest existing ancestor and
// re-joins the missing tail lexically, so a path that does not exist yet is
// placed in the real directory it would land in: a link at the instance root
// into the tree (`img -> ws`) cannot make `img/boot.md` look root-owned.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}

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

// Load reads, resolves, validates (Validate) and host-checks (CheckHost) the
// config at path.
func Load(path string) (*App, error) {
	app, err := LoadNoHostChecks(path)
	if err != nil {
		return nil, err
	}
	if err := app.CheckHost(); err != nil {
		return nil, err
	}
	return app, nil
}

// LoadNoHostChecks is Load without CheckHost.
func LoadNoHostChecks(path string) (*App, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var app App
	if err := yaml.Unmarshal(b, &app); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	app.dir = filepath.Dir(path)
	// The instance root holds the config and boot material and must not be
	// mounted, or a compromised agent could rewrite the config the host
	// trusts on the next bring-up, so `tree: .` is rejected.
	if !confinedRel(app.Tree) {
		return nil, fmt.Errorf("config: tree %q must be a relative subdirectory inside the instance root (not %q, not absolute, no \"..\")", app.Tree, ".")
	}
	app.Tree = filepath.Join(app.dir, app.Tree)
	if abs, err := filepath.Abs(app.Tree); err == nil {
		app.Tree = abs
	}
	// Both sides of every insideTree test are resolved the same way, so a
	// link on either side is seen through. app.Tree stays lexical: it is what
	// gets mounted.
	realTree, err := resolveExisting(app.Tree)
	if err != nil {
		return nil, fmt.Errorf("config: tree %s: %w", app.Tree, err)
	}
	app.Scion.Source = resolvePath(app.Scion.Source, app.dir)
	app.Scion.Binary = resolvePath(app.Scion.Binary, app.dir)
	// At most one scion mode, not exactly one: commands that bring nothing
	// up load configs too, and scionbin.Resolve reports a missing mode.
	scionModes := 0
	for _, v := range []string{app.Scion.Binary, app.Scion.Source, app.Scion.Version} {
		if v != "" {
			scionModes++
		}
	}
	if scionModes > 1 {
		return nil, fmt.Errorf("config: scion.binary, scion.source and scion.version are mutually exclusive")
	}
	// The scion binary or source becomes the engine every agent runs under,
	// so an in-tree path would let a compromised agent choose its own engine.
	for _, m := range []struct{ key, path string }{
		{"scion.binary", app.Scion.Binary},
		{"scion.source", app.Scion.Source},
	} {
		if m.path == "" {
			continue
		}
		p, err := resolveExisting(m.path)
		if err != nil {
			return nil, fmt.Errorf("config: %s (%s): %w", m.key, m.path, err)
		}
		inside, err := insideTreeByIdentity(realTree, p)
		if err != nil {
			return nil, errCannotPlace(m.key, m.path, err)
		}
		if inside {
			return nil, fmt.Errorf("config: %s (%s) is inside the mounted tree (%s); agents can write there, and it would be installed as the engine they run under",
				m.key, m.path, app.Tree)
		}
	}
	// Boot material the host must own. Confinement to the root is not enough,
	// because the tree is inside the root: `prompt_file: workspace/boot.md`
	// with `tree: workspace` would let the manager author its own next boot
	// task. image_tar is the code the agent runs.
	bootFiles := []struct{ key, path string }{
		{"manager.prompt_file", app.ManagerPromptPath()},
		{"manager.instructions_file", app.ManagerInstructionsPath()},
		{"manager.image_tar", app.ManagerImageTarPath()},
	}
	for _, g := range app.Workers {
		bootFiles = append(bootFiles,
			struct{ key, path string }{fmt.Sprintf("workers[%s].instructions_file", g.Name), app.WorkerInstructionsPath(g)},
			struct{ key, path string }{fmt.Sprintf("workers[%s].image_tar", g.Name), app.WorkerImageTarPath(g)})
	}
	for _, m := range bootFiles {
		if m.path == "" {
			continue
		}
		// Abs first: a relative path would fail Rel against the absolute
		// tree and pass the check by accident.
		p, err := filepath.Abs(m.path)
		if err != nil {
			return nil, fmt.Errorf("config: %s (%s): %w", m.key, m.path, err)
		}
		if p, err = resolveExisting(p); err != nil {
			return nil, fmt.Errorf("config: %s (%s): %w", m.key, m.path, err)
		}
		inside, err := insideTreeByIdentity(realTree, p)
		if err != nil {
			return nil, errCannotPlace(m.key, m.path, err)
		}
		if inside {
			return nil, fmt.Errorf("config: %s (%s) is inside the mounted tree (%s); agents can write there, and this file is boot material the host must own — move it up to the instance root",
				m.key, m.path, app.Tree)
		}
		// A missing file is apply's to report; it names the step that needs it.
		if fi, err := os.Stat(p); err == nil && !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("config: %s (%s) is not a regular file", m.key, m.path)
		}
	}
	if r := app.Scion.AgentRole; r != "" && !slices.Contains(scionAgentRoles, r) {
		return nil, fmt.Errorf("config: unknown scion.agent_role %q (valid: %s; omit it entirely to let scion choose)",
			r, strings.Join(scionAgentRoles, ", "))
	}
	app.Manager.CredentialFile = resolvePath(app.Manager.CredentialFile, app.dir)
	// Host secrets are not confined to the root: they should live outside
	// it, and checkHostPathsOutsideTree refuses them in the tree.
	app.Broker.APIKeyFile = resolvePath(app.Broker.APIKeyFile, app.dir)
	app.Operator.SigningKey = resolvePath(app.Operator.SigningKey, app.dir)
	app.injectLLMGrants()
	if err := app.Validate(); err != nil {
		return nil, err
	}
	// After Validate, so manager.read_only and the worker dirs it consults
	// are well formed.
	if err := app.checkHostPathsOutsideTree(); err != nil {
		return nil, err
	}
	return &app, nil
}

// injectLLMGrants adds the implicit llm capability to every api-key agent.
// Idempotent.
func (a *App) injectLLMGrants() {
	add := func(obtain *[]Grant) {
		for _, g := range *obtain {
			if g.Tool == wire.ReservedLLMTool && g.Op == wire.ReservedLLMOp {
				return
			}
		}
		*obtain = append(*obtain, Grant{Tool: wire.ReservedLLMTool, Op: wire.ReservedLLMOp})
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
