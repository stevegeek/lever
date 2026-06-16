//go:build integration

package apply_test

// Live integration test for `lever apply`. This is the automated form of the
// C6 hands-on validation: it brings up a real OrbStack jail, cross-compiles and
// installs scion into it, loads the agent image, runs the full bring-up, starts
// the manager agent with a prompt, and asserts the agent creates HELLO on the
// host through the bind mount — then tears the jail down.
//
// It needs a real machine (OrbStack, a loaded agent image, a Claude credential),
// so it is gated behind the `integration` build tag AND LEVER_IT=1, and skips
// unless the three prerequisite paths are provided via env. Run with:
//
//	LEVER_IT=1 \
//	LEVER_IT_IMAGE=scionlocal/lever-claude:latest \
//	LEVER_IT_SCION_SRC=$HOME/lever-instance/vendor/scion-src \
//	LEVER_IT_CRED=$HOME/.scion/oauth-token \
//	go test -tags integration -run TestApplyLiveHelloGrove -timeout 35m ./internal/apply/
//
// It proves the executor end-to-end. It does NOT exercise manager→worker
// dispatch: the stock agent image carries no scion/lever tooling, so the manager
// cannot orchestrate the worker grove. The prompt therefore has the manager do
// the task directly; the worker grove is still registered (exercising the
// register-grove + path-translation path).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const wantHello = "hello from the lever manager"

func TestApplyLiveHelloGrove(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(tree, "groves", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(tree, "groves", "worker", ".keep"), "")

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
groves:
  - name: worker
    dir: groves/worker
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
		"github.com/lever-to/lever/cmd/lever", "apply", filepath.Join(tree, "lever.yaml"))
	apply.Env = append(os.Environ(), "DOCKER_HOST=") // don't leak a host docker socket into the run
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("lever apply failed: %v\n%s", err, out)
	}

	// The manager agent runs asynchronously; poll the host tree for HELLO.
	hello := filepath.Join(tree, "HELLO")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if b, err := os.ReadFile(hello); err == nil {
			if got := strings.TrimSpace(string(b)); got != wantHello {
				t.Fatalf("HELLO contents = %q, want %q", got, wantHello)
			}
			return // success: agent booted with its prompt and wrote to the host via the mount
		}
		if time.Now().After(deadline) {
			t.Fatalf("HELLO not created within deadline (manager agent did not complete the task)")
		}
		time.Sleep(5 * time.Second)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
