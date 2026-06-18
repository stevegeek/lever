// Package config loads an application config: the declarative description of a
// lever agent-manager application (the manager + its groves).
package config

import (
	"fmt"
	"os"
	"path/filepath"
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

	dir string
}

var knownBackends = map[string]bool{"orbstack": true, "linux-docker": true}

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
	if app.Tree != "" && !filepath.IsAbs(app.Tree) {
		app.Tree = filepath.Join(app.dir, app.Tree)
	}
	if app.Tree != "" {
		if abs, err := filepath.Abs(app.Tree); err == nil {
			app.Tree = abs
		}
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
	if !knownBackends[a.Backend] {
		return fmt.Errorf("config: unknown backend %q (known: orbstack, linux-docker)", a.Backend)
	}
	if a.Tree == "" {
		return fmt.Errorf("config: tree is required")
	}
	for _, g := range a.Groves {
		if g.Name == "" || g.Dir == "" {
			return fmt.Errorf("config: grove needs name + dir (got %+v)", g)
		}
		if filepath.IsAbs(g.Dir) || strings.HasPrefix(filepath.Clean(g.Dir), "..") {
			return fmt.Errorf("config: grove dir %q must be relative and inside the tree", g.Dir)
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

// ManagerPromptPath returns the absolute path to the manager's prompt file
// (relative to the tree), or "" if none is configured.
func (a *App) ManagerPromptPath() string {
	if a.Manager.PromptFile == "" {
		return ""
	}
	return filepath.Join(a.Tree, a.Manager.PromptFile)
}
