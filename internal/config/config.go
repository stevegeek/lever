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
	Name string `yaml:"name"`
	Dir  string `yaml:"dir"`
}

type App struct {
	Name    string  `yaml:"name"`
	Backend string  `yaml:"backend"`
	Tree    string  `yaml:"tree"`
	Manager Manager `yaml:"manager"`
	Groves  []Grove `yaml:"groves"`

	dir string
}

var knownBackends = map[string]bool{"orbstack": true, "linux-docker": true}

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
