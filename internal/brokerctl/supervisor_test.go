package brokerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
)

// Uses /bin/sh indirectly? No — no shell. We launch a real, simple command that
// exits 0 quickly to prove argv assembly + lifecycle, then a long-running one.
// testToolSecret stands in for the per-boot secret Serve mints.
const testToolSecret = "test-tool-secret"

// trackedCount returns the number of currently-tracked child processes.
func trackedCount(s *Supervisor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}

func TestSupervisorStartsConfiguredToolsWithFlags(t *testing.T) {
	// `true` ignores args and exits 0; we only assert Start doesn't error and the
	// process is launched with our injected flags appended (argv inspection via a
	// recording fake is overkill here — assert no error + clean Stop).
	tools := []ToolSpec{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"}}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
}

func TestSupervisorRejectsEmptyCommand(t *testing.T) {
	s := NewSupervisor([]ToolSpec{{Name: "db", Command: nil, Backend: "x"}}, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("a tool with no command must error")
	}
	s.Stop()
}

func TestSupervisorStartCleansUpOnPartialFailure(t *testing.T) {
	// First tool starts fine (`true`); second has an empty command → Start errors.
	// The supervisor must reap the first tool, leaving nothing tracked/running.
	tools := []ToolSpec{
		{Name: "ok", Command: []string{"true"}, Backend: "127.0.0.1:1"},
		{Name: "bad", Command: nil, Backend: "127.0.0.1:2"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start must error when a tool has no command")
	}
	// After a failed Start, no processes remain tracked (cleaned up).
	if n := trackedCount(s); n != 0 {
		t.Fatalf("Start left %d processes tracked after partial-failure cleanup", n)
	}
	s.Stop() // must be safe to call again (no-op)
}

func TestSupervisorSkipsExternalTools(t *testing.T) {
	tools := []ToolSpec{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start with only external tools must succeed (nothing to spawn): %v", err)
	}
	defer s.Stop()
	if n := trackedCount(s); n != 0 {
		t.Fatalf("tracked = %d, want 0 (external tools are fronted, not spawned)", n)
	}
}

func TestSupervisorMixedSpawnsOnlySupervised(t *testing.T) {
	tools := []ToolSpec{
		{Name: "ext", External: true, Backend: "127.0.0.1:3300"},
		{Name: "db", Command: []string{"/bin/sleep", "60"}, Backend: "127.0.0.1:3201"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if n := trackedCount(s); n != 1 {
		t.Fatalf("tracked = %d, want 1 (only the supervised tool spawns)", n)
	}
}

func TestSupervisorPerToolLogs(t *testing.T) {
	dir := t.TempDir()
	tools := []ToolSpec{
		{Name: "alpha", Command: []string{"sh", "-c", "echo ALPHA_OUT"}},
		{Name: "beta", Command: []string{"sh", "-c", "echo BETA_OUT"}},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:0", filepath.Join(dir, "tool-logs"), testToolSecret)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// give the short-lived echoes a moment, then stop (closes files)
	time.Sleep(200 * time.Millisecond)
	s.Stop()

	a, _ := os.ReadFile(filepath.Join(dir, "tool-logs", "alpha.log"))
	b, _ := os.ReadFile(filepath.Join(dir, "tool-logs", "beta.log"))
	if !strings.Contains(string(a), "ALPHA_OUT") {
		t.Fatalf("alpha.log missing its own output: %q", a)
	}
	if strings.Contains(string(a), "BETA_OUT") {
		t.Fatalf("alpha.log leaked beta's output: %q", a)
	}
	if !strings.Contains(string(b), "BETA_OUT") {
		t.Fatalf("beta.log missing its own output: %q", b)
	}
}

func TestToolSpecsCarriesWhatTheSupervisorNeeds(t *testing.T) {
	app := &config.App{Broker: config.Broker{Tools: []config.Tool{
		{Name: "db", Command: []string{"db-server", "-x"}, Backend: "127.0.0.1:3201", Gate: config.GateCoarse},
		{Name: "ext", External: true, Backend: "127.0.0.1:3300"},
	}}}
	dir := app.InstanceDir()
	if !filepath.IsAbs(dir) {
		t.Fatalf("InstanceDir = %q, want an absolute path", dir)
	}
	got, err := ToolSpecs(app)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolSpec{
		{Name: "db", Command: []string{"db-server", "-x"}, Backend: "127.0.0.1:3201", Dir: dir},
		{Name: "ext", External: true, Backend: "127.0.0.1:3300", Dir: dir},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolSpecs = %+v, want %+v", got, want)
	}
	if none, err := ToolSpecs(&config.App{}); err != nil || len(none) != 0 {
		t.Fatal("no tools must map to no specs")
	}
}

// A tool runs in its spec's Dir, and a relative command resolves there:
// config load's tree check resolves the same paths against the instance dir,
// so the two agree whatever directory the broker was started from.
func TestSupervisorRunsToolInItsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(t.TempDir(), "tool-logs")
	s := NewSupervisor([]ToolSpec{{Name: "rel", Command: []string{"./tool.sh"}, Dir: dir}}, "http://127.0.0.1:0", logs, testToolSecret)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.mu.Lock()
	cmd := s.cmds[0]
	s.mu.Unlock()
	_ = cmd.Wait()
	s.Stop()
	out, _ := os.ReadFile(filepath.Join(logs, "rel.log"))
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != dir && got != real {
		t.Fatalf("tool ran in %q, want %q", got, dir)
	}
}

// A bare command is looked up on the supervisor's fixed PATH, not on the
// broker process's own: a name found only on the broker's PATH is refused.
func TestSupervisorResolvesBareCommandOnSupervisorPATH(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "lever-only-here"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := NewSupervisor([]ToolSpec{{Name: "x", Command: []string{"lever-only-here"}}}, "http://127.0.0.1:0", filepath.Join(t.TempDir(), "tool-logs"), testToolSecret)
	err := s.Start(context.Background())
	s.Stop()
	if err == nil || !strings.Contains(err.Error(), config.ToolSupervisorPATH) {
		t.Fatalf("Start = %v, want a not-found error naming the supervisor PATH", err)
	}
}

// The supervisor hands every supervised tool the per-boot shared secret through
// LEVER_TOOL_SECRET (the only inherited variable besides PATH); captool requires
// it on every request so a jail agent that reaches the tool port directly is
// refused. It travels via the environment, never argv (ps-visible).
func TestSupervisorPassesToolSecretInEnv(t *testing.T) {
	dir := t.TempDir()
	tools := []ToolSpec{{Name: "db", Command: []string{"sh", "-c", "echo SECRET=$LEVER_TOOL_SECRET; echo HOME=$HOME"}}}
	s := NewSupervisor(tools, "http://127.0.0.1:0", filepath.Join(dir, "tool-logs"), "per-boot-secret")
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	s.Stop()
	out, _ := os.ReadFile(filepath.Join(dir, "tool-logs", "db.log"))
	if !strings.Contains(string(out), "SECRET=per-boot-secret") {
		t.Fatalf("tool did not receive LEVER_TOOL_SECRET: %q", out)
	}
	// The env stays minimal: nothing else from the host is inherited.
	if !strings.Contains(string(out), "HOME=\n") {
		t.Fatalf("tool env must stay minimal (no inherited HOME): %q", out)
	}
}

func TestNewToolSecretIsRandomHex(t *testing.T) {
	a, err := NewToolSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToolSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || a == b {
		t.Fatalf("NewToolSecret = %q / %q; want two distinct 32-byte hex strings", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("NewToolSecret produced a non-hex rune %q in %q", r, a)
		}
	}
}

// A tool that relies on manager.read_only is held back while the guard
// fails, says why in its log, and starts once the guard passes; the other
// tools start at once.
func TestSupervisorHoldsReadOnlyToolUntilGuardPasses(t *testing.T) {
	logs := filepath.Join(t.TempDir(), "tool-logs")
	tools := []ToolSpec{
		{Name: "free", Command: []string{"/bin/sleep", "60"}},
		{Name: "ro", Command: []string{"/bin/sleep", "60"}, ReadOnly: []string{"tools"}},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:0", logs, testToolSecret)
	s.retry = 10 * time.Millisecond
	var mu sync.Mutex
	pass := false
	s.ReadOnlyGuard = func(_ context.Context, entries []string) error {
		mu.Lock()
		defer mu.Unlock()
		if !reflect.DeepEqual(entries, []string{"tools"}) {
			t.Errorf("guard entries = %v", entries)
		}
		if !pass {
			return errors.New("manager \"m\" does not hold that protection: tools not mounted read-only")
		}
		return nil
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if n := trackedCount(s); n != 1 {
		t.Fatalf("tracked = %d right after Start, want 1 (the read_only tool is held)", n)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(logs, "ro.log"))
		return strings.Contains(string(b), "not starting tool \"ro\"") && strings.Contains(string(b), "lever up --fresh")
	})
	time.Sleep(50 * time.Millisecond)
	if n := trackedCount(s); n != 1 {
		t.Fatalf("tracked = %d while the guard fails, want 1", n)
	}
	b, _ := os.ReadFile(filepath.Join(logs, "ro.log"))
	if c := strings.Count(string(b), "not starting tool"); c != 1 {
		t.Fatalf("the refusal was logged %d times, want once per reason", c)
	}
	mu.Lock()
	pass = true
	mu.Unlock()
	waitFor(t, func() bool { return trackedCount(s) == 2 })
}

// Without a guard (no way to read the manager's mounts) a read_only tool
// never starts: fail closed. Stop ends the retry loop.
func TestSupervisorWithoutGuardNeverStartsReadOnlyTool(t *testing.T) {
	logs := filepath.Join(t.TempDir(), "tool-logs")
	s := NewSupervisor([]ToolSpec{{Name: "ro", Command: []string{"/bin/sleep", "60"}, ReadOnly: []string{"tools"}}}, "http://127.0.0.1:0", logs, testToolSecret)
	s.retry = 10 * time.Millisecond
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(logs, "ro.log"))
		return strings.Contains(string(b), "no read-only check")
	})
	time.Sleep(50 * time.Millisecond)
	if n := trackedCount(s); n != 0 {
		t.Fatalf("tracked = %d, want 0", n)
	}
	s.Stop()
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
