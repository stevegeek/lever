package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// managedPath is a managed-settings path under a fresh temp "/etc".
func managedPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "claude-code", "managed-settings.json")
}

func readManaged(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("managed settings not valid JSON: %v", err)
	}
	return m
}

// sessionStartCommands lists every SessionStart hook command with its matcher.
func sessionStartCommands(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	hooks, _ := m["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	for _, g := range groups {
		gm := g.(map[string]any)
		matcher, _ := gm["matcher"].(string)
		for _, h := range gm["hooks"].([]any) {
			out[h.(map[string]any)["command"].(string)] = matcher
		}
	}
	return out
}

func TestWriteManagedSettingsNoConfigTouchesNothing(t *testing.T) {
	p := managedPath(t)
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no config must create nothing, stat dir: %v", err)
	}
	if err := WriteManagedSettings("", &wire.Claude{AutoCompactWindow: 400000}); err != nil {
		t.Fatalf("empty path must be a no-op: %v", err)
	}
}

func TestWriteManagedSettingsWritesWindowAndHook(t *testing.T) {
	p := managedPath(t)
	note := "Re-read NOTES.md and the open cards before you go on."
	if err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: note}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 (claude runs as the scion user and must read it)", fi.Mode().Perm())
	}
	m := readManaged(t, p)
	if env := m["env"].(map[string]any); env[AutoCompactWindowEnv] != "400000" {
		t.Errorf("env = %v, want %s=400000", env, AutoCompactWindowEnv)
	}
	cmds := sessionStartCommands(t, m)
	want := AfterCompactHookCommand(note)
	if cmds[want] != "compact" || len(cmds) != 1 {
		t.Fatalf("SessionStart hooks = %v, want only %q with matcher compact", cmds, want)
	}
	// The shell that runs the hook sees no character of the note itself.
	if strings.ContainsAny(strings.TrimPrefix(want, "lever-agent after-compact --note-b64 "), " ;'\"$`\\") {
		t.Fatalf("hook command %q carries shell metacharacters", want)
	}
}

func TestWriteManagedSettingsMergesAndReplaces(t *testing.T) {
	p := managedPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := `{"permissions":{"deny":["WebFetch"]},"env":{"OTHER":"keep"},` +
		`"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"sciontool hook --dialect=claude"}]}],` +
		`"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"sciontool hook --dialect=claude"}]}]}}`
	if err := os.WriteFile(p, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 200000, AfterCompactNote: "first"}); err != nil {
		t.Fatal(err)
	}
	// A second start with a new note replaces lever's hook, never adds one.
	if err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 300000, AfterCompactNote: "second"}); err != nil {
		t.Fatal(err)
	}
	m := readManaged(t, p)
	if _, ok := m["permissions"]; !ok {
		t.Error("dropped a foreign top-level key")
	}
	env := m["env"].(map[string]any)
	if env["OTHER"] != "keep" || env[AutoCompactWindowEnv] != "300000" {
		t.Errorf("env = %v", env)
	}
	if _, ok := m["hooks"].(map[string]any)["Stop"]; !ok {
		t.Error("dropped a foreign hook event")
	}
	cmds := sessionStartCommands(t, m)
	if cmds["sciontool hook --dialect=claude"] != "*" {
		t.Errorf("dropped the foreign SessionStart hook: %v", cmds)
	}
	if cmds[AfterCompactHookCommand("second")] != "compact" || len(cmds) != 2 {
		t.Errorf("SessionStart hooks = %v, want the foreign one and lever's second note only", cmds)
	}

	// Unset: lever's keys go, the foreign ones stay.
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	m = readManaged(t, p)
	if _, ok := m["env"].(map[string]any)[AutoCompactWindowEnv]; ok {
		t.Error("unset window still delivered")
	}
	if cmds := sessionStartCommands(t, m); len(cmds) != 1 || cmds["sciontool hook --dialect=claude"] == "" {
		t.Errorf("after unset SessionStart hooks = %v, want the foreign one only", cmds)
	}
}

func TestWriteManagedSettingsRemovesFileItEmptied(t *testing.T) {
	p := managedPath(t)
	if err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file holding only lever's keys must go when they are unset: %v", err)
	}
}

func TestWriteManagedSettingsRefusesSymlink(t *testing.T) {
	p := managedPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 400000})
	if !errors.Is(err, errRefusedPath) {
		t.Fatalf("err = %v, want errRefusedPath", err)
	}
	if b, _ := os.ReadFile(target); string(b) != `{}` {
		t.Fatalf("wrote through the link: %s", b)
	}
}

func TestAfterCompactOutput(t *testing.T) {
	note := "Re-read STATE.md"
	b, err := AfterCompactOutput(base64.StdEncoding.EncodeToString([]byte(note)))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("output %s is not JSON: %v", b, err)
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q", out.HookSpecificOutput.HookEventName)
	}
	if want := AfterCompactMarker + " " + note; out.HookSpecificOutput.AdditionalContext != want {
		t.Errorf("additionalContext = %q, want %q", out.HookSpecificOutput.AdditionalContext, want)
	}
	for _, bad := range []string{"", "!!!", base64.StdEncoding.EncodeToString([]byte("a\nb"))} {
		if _, err := AfterCompactOutput(bad); err == nil {
			t.Errorf("AfterCompactOutput(%q) accepted", bad)
		}
	}
}

// writeBootstrapClaude rewrites c's bootstrap with a claude block.
func writeBootstrapClaude(t *testing.T, path string, claude *wire.Claude) {
	t.Helper()
	bs, err := LoadBootstrap(path)
	if err != nil {
		t.Fatal(err)
	}
	bs.Claude = claude
	b, _ := json.Marshal(bs)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBootWritesManagedSettingsFromBootstrap(t *testing.T) {
	env := testBroker(t)
	c := baseBootConfig(t, env)
	c.ManagedSettingsPath = managedPath(t)
	writeBootstrapClaude(t, c.BootstrapPath, &wire.Claude{AutoCompactWindow: 250000})
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := readManaged(t, c.ManagedSettingsPath)["env"].(map[string]any)[AutoCompactWindowEnv]; got != "250000" {
		t.Fatalf("window = %v, want 250000", got)
	}
	// The next start (identity kept) re-applies the envelope as it is now.
	writeBootstrapClaude(t, c.BootstrapPath, nil)
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.ManagedSettingsPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an envelope with no claude block must remove lever's settings: %v", err)
	}
}

// The manager's envelope is in the tree it writes: boot drops a block the
// host would never have staged, logs it, and still boots the agent.
func TestBootDropsInvalidClaudeBlock(t *testing.T) {
	env := testBroker(t)
	c := baseBootConfig(t, env)
	c.ManagedSettingsPath = managedPath(t)
	var logged []string
	c.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	writeBootstrapClaude(t, c.BootstrapPath, &wire.Claude{AfterCompactNote: "a\nb"})
	if err := Boot(context.Background(), c); err != nil {
		t.Fatalf("an invalid claude block stopped the boot: %v", err)
	}
	if _, err := os.Stat(c.ManagedSettingsPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("wrote managed settings from an invalid block")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "claude block refused") {
		t.Fatalf("no log line for the refused block: %q", logged)
	}
}

// Under rootless podman the pre-start hook runs as the agent user; a managed
// settings directory it cannot create must not stop the agent.
func TestBootSurvivesAnUnwritableManagedSettingsDir(t *testing.T) {
	env := testBroker(t)
	c := baseBootConfig(t, env)
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	c.ManagedSettingsPath = filepath.Join(ro, "claude-code", "managed-settings.json")
	var logged []string
	c.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	writeBootstrapClaude(t, c.BootstrapPath, &wire.Claude{AutoCompactWindow: 250000})
	if err := Boot(context.Background(), c); err != nil {
		t.Fatalf("an unwritable managed settings dir stopped the boot: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "claude settings not delivered") {
		t.Fatalf("no log line for the failed write: %q", logged)
	}
}

// The marker must not start like the skills' trusted operator-note marker.
func TestAfterCompactMarkerIsNotTheOperatorNoteMarker(t *testing.T) {
	if strings.HasPrefix(AfterCompactMarker, "[lever: operator note") || strings.HasPrefix(AfterCompactMarker, "[lever:") {
		t.Fatalf("AfterCompactMarker %q shares a prefix with lever's message markers", AfterCompactMarker)
	}
}

// With no readable envelope, boot removes lever's values and says so.
func TestBootLogsRemovalWithoutBootstrap(t *testing.T) {
	env := testBroker(t)
	c := baseBootConfig(t, env)
	c.ManagedSettingsPath = managedPath(t)
	writeBootstrapClaude(t, c.BootstrapPath, &wire.Claude{AutoCompactWindow: 250000})
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var logs []string
	c.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	if err := os.Remove(c.BootstrapPath); err != nil {
		t.Fatal(err)
	}
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "no readable bootstrap envelope") || !strings.Contains(logs[0], c.ManagedSettingsPath) {
		t.Fatalf("logs = %q", logs)
	}
	if _, err := os.Stat(c.ManagedSettingsPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lever's values must be removed: %v", err)
	}
}
