package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteSettingsEnv MERGES env into the `env` block of the claude settings.json
// at path, preserving any existing settings and env keys (read-modify-write).
// This is how the dynamic ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL reach the
// in-container `claude`: Claude Code natively reads its settings.json `env`
// block at startup (verified live 2026-06-28), whereas the scion harness
// env-overlay path is inert for our builtin harness. An empty path is a no-op
// (enrol-only boot). Written 0600, parent directory 0700.
//
// path is <home>/.claude/settings.json: both the directory and the file are
// agent-writable and boot runs this as root, so the read and the write are
// confined to <home> (the grandparent) and a symlink at either component is
// refused (see errRefusedPath). The grandparent must exist.
func WriteSettingsEnv(path string, env map[string]string) error {
	if path == "" {
		return nil
	}
	root, rel := splitAbove(path, 2)
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("settings %s: open %s: %w", path, root, err)
	}
	defer r.Close()
	if err := checkedFile(r, rel); err != nil {
		return err
	}
	if d := filepath.Dir(rel); d != "." {
		if err := r.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("settings dir %s: %w", filepath.Dir(path), err)
		}
	}
	// Merge into existing settings rather than clobber (claude may already have
	// written model/permissions/etc; mcp config lives in a separate ~/.claude.json).
	settings := map[string]any{}
	if b, err := r.ReadFile(rel); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("settings %s: parse existing: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("settings %s: read: %w", path, err)
	}
	existing, _ := settings["env"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range env {
		existing[k] = v
	}
	settings["env"] = existing
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return r.WriteFile(rel, b, 0o600)
}
