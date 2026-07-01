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

// Manifest is the in-jail view: grove name → resolved image, plus the jail mount
// root. MountRoot is the path the project tree is bind-mounted at INSIDE the jail
// (e.g. /lever). The in-jail manager joins it with a grove's tree-relative dir to
// get the jail-absolute path scion needs for `-g` and `--workspace` — without it
// a grove dispatch falls back to mounting the manager's whole tree. It is the
// jail mount point, NOT a host path, so it carries no host-layout leak.
type Manifest struct {
	MountRoot string          `yaml:"mount_root,omitempty"`
	Groves    []ManifestGrove `yaml:"groves"`
}

// ManifestGrove is one grove's resolved image and LLM-auth mode. LLMAuth is a
// posture flag, not a secret: it tells the in-jail manager whether to convey
// LEVER_LLM_AUTH=api-key to the grove. A compromised manager rewriting it can at
// most change its own grove's mode — it cannot conjure a credential (the
// CLAUDE_CODE_OAUTH_TOKEN secret is host-projected and gated solely by
// Manager.CredentialFile, independent of this flag). See security-model.md §5.
type ManifestGrove struct {
	Name    string      `yaml:"name"`
	Image   string      `yaml:"image"`
	LLMAuth LLMAuthMode `yaml:"llm_auth,omitempty"`
}

// ManifestFromApp builds the sanitized manifest, resolving each grove's image
// and effective LLM-auth mode host-side (override or the broker default).
func ManifestFromApp(a *App) Manifest {
	m := Manifest{}
	for _, g := range a.Groves {
		m.Groves = append(m.Groves, ManifestGrove{
			Name:    g.Name,
			Image:   a.GroveImage(g),
			LLMAuth: a.EffectiveGroveLLMAuth(g),
		})
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

// LLMAuthFor returns the resolved LLM-auth mode for a grove name, or "" if the
// grove is absent (an old manifest without the field also yields "", which the
// caller treats as not-api-key — the safe default).
func (m *Manifest) LLMAuthFor(name string) LLMAuthMode {
	for _, g := range m.Groves {
		if g.Name == name {
			return g.LLMAuth
		}
	}
	return ""
}
