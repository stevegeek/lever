package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readSettingsEnv reads the env block from a claude settings.json.
func readSettingsEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("settings.json not valid JSON: %v", err)
	}
	return s.Env
}

func TestWriteSettingsEnvMergesNotClobbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing settings: an unrelated top-level key + an existing env var.
	if err := os.WriteFile(path, []byte(`{"model":"sonnet","env":{"EXISTING":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettingsEnv(path, map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_BASE_URL": "http://x/llm"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["model"] != "sonnet" {
		t.Errorf("clobbered unrelated top-level key: model=%v", got["model"])
	}
	env, ok := got["env"].(map[string]any)
	if !ok {
		t.Fatalf("env is not an object: %v", got["env"])
	}
	if env["EXISTING"] != "keep" {
		t.Errorf("clobbered pre-existing env var: EXISTING=%v", env["EXISTING"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" || env["ANTHROPIC_BASE_URL"] != "http://x/llm" {
		t.Errorf("dynamic vars not merged into env: %v", env)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestWriteSettingsEnvCreatesWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := WriteSettingsEnv(path, map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}); err != nil {
		t.Fatal(err)
	}
	if env := readSettingsEnv(t, path); env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("absent-file case did not create env block: %v", env)
	}
}

func TestWriteSettingsEnvEmptyPathNoop(t *testing.T) {
	if err := WriteSettingsEnv("", map[string]string{"X": "y"}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestWriteSettingsEnvRejectsCorruptExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettingsEnv(path, map[string]string{"X": "y"}); err == nil {
		t.Fatal("a corrupt settings.json must not be silently overwritten")
	}
}
