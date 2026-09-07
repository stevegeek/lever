package scionhook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// runHook runs the pre-start hook with a stub lever-agent on PATH that prints
// its argv, under the given env, and returns what the stub saw. root stands
// in for "/" only through the env var: the mount probe is on the real
// /run/lever, which a test host does not have.
func runHook(t *testing.T, env map[string]string) string {
	t.Helper()
	bin := t.TempDir()
	stub := filepath.Join(bin, "lever-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	full := map[string]string{"PATH": bin + ":" + os.Getenv("PATH"), "HOME": t.TempDir()}
	for k, v := range env {
		full[k] = v
	}
	res, err := proc.RealRunner{}.Run(context.Background(), full, "sh", "pre-start")
	if err != nil {
		t.Fatalf("hook: %v (stderr %q)", err, res.Stderr)
	}
	return res.Stdout
}

func bootstrapArg(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range lines {
		if l == "--bootstrap" && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no --bootstrap in %q", out)
	return ""
}

// The hook is valid POSIX sh (the agent image runs it under busybox/dash).
func TestPreStartHookParses(t *testing.T) {
	if res, err := (proc.RealRunner{}).Run(context.Background(), nil, "sh", "-n", "pre-start"); err != nil {
		t.Fatalf("sh -n: %v (%s)", err, res.Stderr)
	}
}

// LEVER_BOOTSTRAP (set by the broker for a worker) wins; without it, and with
// no /run/lever mount on this host, the manager's in-tree path is used.
func TestPreStartHookSelectsBootstrapPath(t *testing.T) {
	if got := bootstrapArg(t, runHook(t, map[string]string{"LEVER_BOOTSTRAP": "/run/lever/bootstrap.json"})); got != "/run/lever/bootstrap.json" {
		t.Fatalf("with LEVER_BOOTSTRAP: --bootstrap %q", got)
	}
	if _, err := os.Stat("/run/lever/bootstrap.json"); err == nil {
		t.Skip("this host has a /run/lever/bootstrap.json; the fallback branch cannot be observed")
	}
	if got := bootstrapArg(t, runHook(t, nil)); got != "/workspace/.lever/bootstrap.json" {
		t.Fatalf("without LEVER_BOOTSTRAP: --bootstrap %q", got)
	}
	out := runHook(t, map[string]string{"LEVER_LLM_AUTH": "api-key"})
	if !strings.Contains(out, "--llm-auth\napi-key") {
		t.Fatalf("llm-auth not forwarded: %q", out)
	}
}
