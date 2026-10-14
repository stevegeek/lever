//go:build integration

package apply_test

// Live integration test for `lever apply`: it brings up a real OrbStack jail,
// runs the full bring-up, and asserts the manager agent boots and completes
// its task, then tears the jail down.
//
// It needs OrbStack, a loaded agent image and a Claude credential, so it is
// gated behind the `integration` build tag and LEVER_IT=1, and skips unless the
// prerequisite paths are set. Run with:
//
//	LEVER_IT=1 \
//	LEVER_IT_IMAGE=scionlocal/lever-claude:latest \
//	LEVER_IT_SCION_SRC=$LEVER_INSTANCE/vendor/scion-src \
//	LEVER_IT_CRED=$HOME/.scion/oauth-token \
//	go test -tags integration -run TestApplyLiveHelloWorker -timeout 35m ./internal/apply/
//
// The success signal is the manager reaching activity "completed" via `scion
// list`, not the HELLO file in the host tree: scion mounts a project-config
// working copy as /workspace, and the agent's writes reach the host only via a
// timing-dependent `scion sync`, so asserting the host file would be flaky.
//
// It does not exercise manager-to-worker dispatch: the stock agent image
// carries no scion/lever tooling, so the manager does the task directly. A
// worker is still configured to exercise load-image dedup.

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
	t.Cleanup(func() {
		_ = exec.Command("orb", "delete", "-f", machine).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// The real CLI, so the full buildApplyDeps wiring is exercised.
	apply := exec.CommandContext(ctx, "go", "run",
		"github.com/stevegeek/lever/cmd/lever", "apply", filepath.Join(tree, "lever.yaml"))
	apply.Env = append(os.Environ(), "DOCKER_HOST=") // don't leak a host docker socket into the run
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("lever apply failed: %v\n%s", err, out)
	}

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

// agentState yields ("","") for a missing agent or a transient error.
func agentState(t *testing.T, machine, runUser, name string) (string, string) {
	t.Helper()
	out := orbOut(t, machine, "-u", runUser, "env",
		"XDG_RUNTIME_DIR=/run/user/501", "PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENDPOINT=http://127.0.0.1:8080",
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
