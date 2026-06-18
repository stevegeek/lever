package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ManifestName is the sanitized runtime manifest the host writes INTO the mount
// at apply time, for the in-jail manager to resolve grove images. It carries
// ONLY grove name→image (images already resolved host-side from the full
// config). It deliberately contains no host paths, no credential, no tree/ports
// — so a compromised manager rewriting it can at most pick a different
// already-loaded image for a grove it dispatches (no host escalation, no secret
// leak). See security-model.md §5.
const ManifestName = ".lever-manifest.yaml"

// Manifest is the in-jail view: grove name → resolved image.
type Manifest struct {
	Groves []ManifestGrove `yaml:"groves"`
}

// ManifestGrove is one grove's resolved image.
type ManifestGrove struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
}

// ManifestFromApp builds the sanitized manifest, resolving each grove's image
// host-side (its override or the inherited manager image).
func ManifestFromApp(a *App) Manifest {
	m := Manifest{}
	for _, g := range a.Groves {
		m.Groves = append(m.Groves, ManifestGrove{Name: g.Name, Image: a.GroveImage(g)})
	}
	return m
}

// WriteManifest marshals m to <dir>/ManifestName.
func WriteManifest(dir string, m Manifest) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), b, 0o644)
}

// LoadManifest reads and parses a manifest file.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

// ImageFor returns the resolved image for a grove name, or ("", false).
func (m *Manifest) ImageFor(name string) (string, bool) {
	for _, g := range m.Groves {
		if g.Name == name {
			return g.Image, true
		}
	}
	return "", false
}
