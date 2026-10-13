package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// assertOnlyAgentViewOff fails unless the file at path holds exactly what
// lever writes with no claude config: agent view off and the claude-ready
// hook, nothing else.
func assertOnlyAgentViewOff(t *testing.T, path string) {
	t.Helper()
	m := readManaged(t, path)
	env, _ := m["env"].(map[string]any)
	hooks, _ := m["hooks"].(map[string]any)
	cmds := sessionStartCommands(t, m)
	if len(m) != 3 || m[DisableAgentViewSetting] != true || len(env) != 1 || env[DisableAgentViewEnv] != "1" ||
		len(hooks) != 1 || len(cmds) != 1 || cmds[ReadyHookCommand] != "" {
		t.Fatalf("managed settings = %v, want only %s=1, %s: true and the claude-ready hook", m, DisableAgentViewEnv, DisableAgentViewSetting)
	}
	if _, ok := cmds[ReadyHookCommand]; !ok {
		t.Fatalf("managed settings = %v, want the claude-ready hook", m)
	}
}

// Background sessions are off in every agent, with or without claude config.
func TestWriteManagedSettingsNoConfigStillTurnsOffAgentView(t *testing.T) {
	p := managedPath(t)
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	assertOnlyAgentViewOff(t, p)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
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
	if env := m["env"].(map[string]any); env[AutoCompactWindowEnv] != "400000" || env[DisableAgentViewEnv] != "1" {
		t.Errorf("env = %v, want %s=400000 and %s=1", env, AutoCompactWindowEnv, DisableAgentViewEnv)
	}
	if m[DisableAgentViewSetting] != true {
		t.Errorf("%s = %v, want true", DisableAgentViewSetting, m[DisableAgentViewSetting])
	}
	cmds := sessionStartCommands(t, m)
	want := AfterCompactHookCommand(note)
	if _, ready := cmds[ReadyHookCommand]; cmds[want] != "compact" || !ready || cmds[ReadyHookCommand] != "" || len(cmds) != 2 {
		t.Fatalf("SessionStart hooks = %v, want only %q with matcher compact and the claude-ready hook with none", cmds, want)
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
	foreign := `{"permissions":{"deny":["WebFetch"]},"env":{"OTHER":"keep","CLAUDE_CODE_DISABLE_AGENT_VIEW":"0"},"disableAgentView":false,` +
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
	if _, ready := cmds[ReadyHookCommand]; cmds[AfterCompactHookCommand("second")] != "compact" || !ready || len(cmds) != 3 {
		t.Errorf("SessionStart hooks = %v, want the foreign one, lever's second note and one claude-ready hook only", cmds)
	}

	// Unset: lever's keys go, the foreign ones stay.
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	m = readManaged(t, p)
	env = m["env"].(map[string]any)
	if _, ok := env[AutoCompactWindowEnv]; ok {
		t.Error("unset window still delivered")
	}
	// A planted "off" value is overwritten at every start.
	if env[DisableAgentViewEnv] != "1" || env["OTHER"] != "keep" || m[DisableAgentViewSetting] != true {
		t.Errorf("after unset env = %v, %s = %v; want agent view off and the foreign key kept", env, DisableAgentViewSetting, m[DisableAgentViewSetting])
	}
	if cmds := sessionStartCommands(t, m); len(cmds) != 2 || cmds["sciontool hook --dialect=claude"] == "" {
		t.Errorf("after unset SessionStart hooks = %v, want the foreign one and the claude-ready hook only", cmds)
	}
}

func TestWriteManagedSettingsUnsetLeavesOnlyAgentViewOff(t *testing.T) {
	p := managedPath(t)
	if err := WriteManagedSettings(p, &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManagedSettings(p, nil); err != nil {
		t.Fatal(err)
	}
	assertOnlyAgentViewOff(t, p)
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
	// An envelope with no claude block removes lever's settings but keeps
	// agent view off.
	assertOnlyAgentViewOff(t, c.ManagedSettingsPath)
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
	// Nothing from the invalid block; agent view is still off.
	assertOnlyAgentViewOff(t, c.ManagedSettingsPath)
	if !strings.Contains(strings.Join(logged, "\n"), "claude block refused") {
		t.Fatalf("no log line for the refused block: %q", logged)
	}
}

// Under rootless podman the pre-start hook runs as the agent user; a managed
// settings directory it cannot create must not stop the agent.
func TestBootSurvivesAnUnwritableManagedSettingsDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory")
	}
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

	// No claude config: only the agent-view switch was to be written, and
	// the log says that, not that claude settings went missing.
	writeBootstrapClaude(t, c.BootstrapPath, nil)
	logged = nil
	if err := Boot(context.Background(), c); err != nil {
		t.Fatalf("an unwritable managed settings dir stopped the boot: %v", err)
	}
	all := strings.Join(logged, "\n")
	if !strings.Contains(all, "agent-view switch not written (") || !strings.Contains(all, "background sessions stay possible") ||
		strings.Contains(all, "claude settings not delivered") {
		t.Fatalf("log for a switch-only write failure: %q", logged)
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
	assertOnlyAgentViewOff(t, c.ManagedSettingsPath)
}
