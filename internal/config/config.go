// Package config loads an application config: the declarative description of a
// lever agent-manager application (the manager + its groves).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Manager struct {
	Image          string `yaml:"image"`
	PromptFile     string `yaml:"prompt_file"`
	AllowPorts     []int  `yaml:"allow_ports"`
	CredentialFile string `yaml:"credential_file"`
}

type Grove struct {
	Name  string `yaml:"name"`
	Dir   string `yaml:"dir"`
	Image string `yaml:"image"` // optional; empty ⇒ inherit Manager.Image
}

type ScionConfig struct {
	Source string `yaml:"source"`
}

type App struct {
	Name    string      `yaml:"name"`
	Backend string      `yaml:"backend"`
	Tree    string      `yaml:"tree"`
	Manager Manager     `yaml:"manager"`
	Scion   ScionConfig `yaml:"scion"`
	Groves  []Grove     `yaml:"groves"`

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
	if a.Manager.Image != "" && !imageRE.MatchString(a.Manager.Image) {
		return fmt.Errorf("config: manager.image %q has invalid characters", a.Manager.Image)
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
		if g.Image != "" && !imageRE.MatchString(g.Image) {
			return fmt.Errorf("config: grove %q image %q has invalid characters", g.Name, g.Image)
		}
	}
	return nil
}

// GroveDir returns the absolute path of a grove dir (tree + relative dir).
func (a *App) GroveDir(g Grove) string { return filepath.Join(a.Tree, g.Dir) }

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
