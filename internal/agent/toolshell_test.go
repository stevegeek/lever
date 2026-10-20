package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The package's tests run without an image: no lever-toolshell unless a test
// installs one.
func init() { toolShellPath = "/nonexistent/lever-toolshell" }

func withToolShell(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lever-toolshell")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	old := toolShellPath
	toolShellPath = p
	t.Cleanup(func() { toolShellPath = old })
	return p
}

// T0312: with lever-toolshell in the image, Claude Code's shell prefix points
// at it; without it, or with one that can't run, the key is removed, since a
// prefix that fails would fail every tool command.
func TestWriteManagedSettingsShellPrefix(t *testing.T) {
	path := managedPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"`+ShellPrefixEnv+`":"/tmp/agent-planted","KEEP":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteManagedSettings(path, nil); err != nil {
		t.Fatal(err)
	}
	env := readManaged(t, path)["env"].(map[string]any)
	if _, ok := env[ShellPrefixEnv]; ok || env["KEEP"] != "1" {
		t.Fatalf("no toolshell in the image: env = %v, want the prefix removed and other keys kept", env)
	}

	withToolShell(t, 0o644)
	if err := WriteManagedSettings(path, nil); err != nil {
		t.Fatal(err)
	}
	if env := readManaged(t, path)["env"].(map[string]any); env[ShellPrefixEnv] != nil {
		t.Fatalf("a toolshell that can't run: env = %v, want no prefix", env)
	}

	p := withToolShell(t, 0o755)
	if err := WriteManagedSettings(path, nil); err != nil {
		t.Fatal(err)
	}
	if env := readManaged(t, path)["env"].(map[string]any); env[ShellPrefixEnv] != p {
		t.Fatalf("env = %v, want %s=%s", env, ShellPrefixEnv, p)
	}
}

// The wrapper itself (cmd/lever-agent/scionhook/toolshell): it runs $1 with
// a hard process limit of pids.max minus 256 and oom_score_adj 500, and
// leaves the limit alone when pids.max is "max", unreadable or too small.
// Only a few processes run here: the limit is read back, never reached.
func TestToolShellSetsLimitsAndRunsTheCommand(t *testing.T) {
	if _, err := os.Stat("/proc/self/oom_score_adj"); err != nil {
		t.Skip("needs /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil || bash == "" {
		t.Skip("needs bash")
	}
	script, err := filepath.Abs("../../cmd/lever-agent/scionhook/toolshell")
	if err != nil {
		t.Fatal(err)
	}
	run := func(pidsMax string) (string, string) {
		t.Helper()
		f := filepath.Join(t.TempDir(), "pids.max")
		if pidsMax != "" {
			if err := os.WriteFile(f, []byte(pidsMax+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bash, script, `echo "$(ulimit -Hu) $(cat /proc/self/oom_score_adj) it's run"`)
		cmd.Env = append(os.Environ(), "LEVER_TOOLSHELL_PIDS_MAX="+f)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("toolshell with pids.max %q: %v", pidsMax, err)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 4 || fields[2] != "it's" || fields[3] != "run" {
			t.Fatalf("toolshell did not run the command as one shell line: %q", out)
		}
		return fields[0], fields[1]
	}
	before, err := exec.Command(bash, "-c", "ulimit -Hu").Output()
	if err != nil {
		t.Fatal(err)
	}
	unchanged := strings.TrimSpace(string(before))
	cur, _ := strconv.Atoi(unchanged)
	want := 1792
	if unchanged != "unlimited" && cur < want {
		t.Skipf("the hard process limit here (%s) is below the test's", unchanged)
	}
	if lim, adj := run("2048"); lim != strconv.Itoa(want) || adj != "500" {
		t.Fatalf("pids.max 2048: limit %s, oom_score_adj %s; want %d and 500", lim, adj, want)
	}
	for _, pm := range []string{"max", "", "junk", "300"} {
		if lim, adj := run(pm); lim != unchanged || adj != "500" {
			t.Fatalf("pids.max %q: limit %s, oom_score_adj %s; want %s unchanged and 500", pm, lim, adj, unchanged)
		}
	}
}
