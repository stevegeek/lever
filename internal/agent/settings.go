package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteSettingsEnv merges env into the env block of the claude settings.json
// at path, preserving existing settings and env keys. Claude Code reads that
// block at startup; scion's harness env-overlay path is inert for this
// harness. An empty path is a no-op. Written 0600, parent directory 0700.
//
// Both the directory and the file are agent-writable and boot may run as
// root, so the work is confined to the grandparent (which must exist) and a
// symlink at either component is refused (see errRefusedPath).
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
	// Merge rather than clobber: claude may already have written settings.
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
