//go:build integration

package apply_test

// Live integration test for `lever apply`. This is the automated form of the
// C6 hands-on validation: it brings up a real OrbStack jail, cross-compiles and
// installs scion into it, loads the agent image, runs the full bring-up, starts
// the manager agent with a prompt, and asserts the manager agent boots and
// completes its task — then tears the jail down.
//
// It needs a real machine (OrbStack, a loaded agent image, a Claude credential),
// so it is gated behind the `integration` build tag AND LEVER_IT=1, and skips
// unless the three prerequisite paths are provided via env. Run with:
//
//	LEVER_IT=1 \
//	LEVER_IT_IMAGE=scionlocal/lever-claude:latest \
//	LEVER_IT_SCION_SRC=$LEVER_INSTANCE/vendor/scion-src \
//	LEVER_IT_CRED=$HOME/.scion/oauth-token \
//	go test -tags integration -run TestApplyLiveHelloWorker -timeout 35m ./internal/apply/
//
// It proves the executor end-to-end. The success signal is the manager agent
// reaching activity "completed" via `scion list` in the jail — NOT the HELLO
// file landing in the host tree. scion mounts a project-config working COPY
// (~/.scion/project-configs/<slug>__<id>) as /workspace in the agent container,
// not the live host tree; the agent's writes reach the host only via a `scion
// sync`, which is timing-dependent and out of the executor's control. Asserting
// the host-tree file would make this test flaky (observed across C6 runs). See
// the "agent workspace is a copy+sync" investigation task.
//
// It does NOT exercise manager→worker dispatch: the stock agent image carries no
// scion/lever tooling, so the manager cannot orchestrate the worker. The
// prompt therefore has the manager do the task directly; the worker is
// still registered (exercising the register-worker + path-translation path).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wantHello = "hello from the lever manager"

func TestApplyLiveHelloWorker(t *testing.T) {
	if os.Getenv("LEVER_IT") != "1" {
		t.Skip("live integration test: set LEVER_IT=1 to run")
	}
	image := os.Getenv("LEVER_IT_IMAGE")
	scionSrc := os.Getenv("LEVER_IT_SCION_SRC")
	cred := os.Getenv("LEVER_IT_CRED")
	if image == "" || scionSrc == "" || cred == "" {
		t.Skip("live integration test: requires LEVER_IT_IMAGE, LEVER_IT_SCION_SRC, LEVER_IT_CRED")
	}
	if _, err := exec.LookPath("orb"); err != nil {
		t.Skip("live integration test: `orb` not on PATH")
	}
	if _, err := os.Stat(scionSrc); err != nil {
		t.Skipf("live integration test: scion source %q not found", scionSrc)
	}
	if _, err := os.Stat(cred); err != nil {
		t.Skipf("live integration test: credential %q not found", cred)
	}

	tree := t.TempDir()
	mustWrite(t, filepath.Join(tree, "manager.md"),
		"Create a file named HELLO in your current working directory. Its contents must be\n"+
			"exactly:\n\n"+wantHello+"\n\nThen you are done — do not do anything else.\n")
	if err := os.MkdirAll(filepath.Join(tree, "workers", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(tree, "workers", "worker", ".keep"), "")

	const name = "leverit"
	mustWrite(t, filepath.Join(tree, "lever.yaml"), fmt.Sprintf(`name: %s
backend: orbstack
tree: .
scion:
  source: %s
manager:
  image: %s
  prompt_file: manager.md
  credential_file: %s
  allow_ports: []
workers:
  - name: worker
    dir: workers/worker
`, name, scionSrc, image, cred))

	machine := "lever-" + name
	// Always tear the jail down (atomically removes all in-jail agents/containers).
	t.Cleanup(func() {
		_ = exec.Command("orb", "delete", "-f", machine).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Drive the real CLI so the full buildApplyDeps wiring is exercised.
	apply := exec.CommandContext(ctx, "go", "run",
		"github.com/stevegeek/lever/cmd/lever", "apply", filepath.Join(tree, "lever.yaml"))
	apply.Env = append(os.Environ(), "DOCKER_HOST=") // don't leak a host docker socket into the run
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("lever apply failed: %v\n%s", err, out)
	}

	// The manager agent runs asynchronously inside the jail. Poll scion for its
	// lifecycle state — the reliable, executor-controlled success signal. We want
	// it to reach phase "running" and activity "completed" (task done).
	runUser := strings.TrimSpace(orbOut(t, machine, "whoami"))
	if runUser == "" {
		t.Fatal("could not resolve jail run user")
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		phase, activity := agentState(t, machine, runUser, name)
		if phase == "error" {
			t.Fatalf("manager agent entered phase=error")
		}
		if phase == "running" && activity == "completed" {
			return // success: agent booted with its prompt and completed the task
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager agent did not reach running/completed within deadline (last phase=%q activity=%q)", phase, activity)
		}
		time.Sleep(5 * time.Second)
	}
}

// agentState returns the (phase, activity) of the named agent via `scion list`
// run inside the jail. Missing agent or transient errors yield ("","").
func agentState(t *testing.T, machine, runUser, name string) (string, string) {
	t.Helper()
	out := orbOut(t, machine, "-u", runUser, "env",
		"XDG_RUNTIME_DIR=/run/user/501", "PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true", "SCION_HUB_ENDPOINT=http://127.0.0.1:8080",
		"scion", "list", "--all", "--format", "json")
	i := strings.IndexByte(out, '[')
	if i < 0 {
		return "", ""
	}
	var agents []struct {
		Slug     string `json:"slug"`
		Phase    string `json:"phase"`
		Activity string `json:"activity"`
	}
	if err := json.Unmarshal([]byte(out[i:]), &agents); err != nil {
		return "", ""
	}
	for _, a := range agents {
		if a.Slug == name {
			return a.Phase, a.Activity
		}
	}
	return "", ""
}

// orbOut runs `orb -m <machine> <args...>` on the host and returns combined output.
func orbOut(t *testing.T, machine string, args ...string) string {
	t.Helper()
	full := append([]string{"-m", machine}, args...)
	out, _ := exec.Command("orb", full...).CombinedOutput()
	return string(out)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
