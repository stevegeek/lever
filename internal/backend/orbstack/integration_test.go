//go:build integration

package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
)

// Run with: go test -tags integration -run TestRealOrbStack ./internal/backend/orbstack/ -v
// Requires OrbStack running. Creates and DELETES a throwaway jail.
func TestRealOrbStack(t *testing.T) {
	machine := "lever-it-" + os.Getenv("USER")
	b := New(proc.RealRunner{}, machine, common.Options{})
	ctx := context.Background()
	t.Cleanup(func() { _ = b.Teardown(ctx) })

	projectTree := t.TempDir()
	sentinelName := "SENTINEL"
	sentinelPath := filepath.Join(projectTree, sentinelName)
	if err := os.WriteFile(sentinelPath, []byte("lever-mount-ok"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if err := b.EnsureUp(ctx, backend.Config{
		MachineName:  machine,
		ProjectTree:  projectTree,
		AllowedPorts: nil,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	r := proc.RealRunner{}

	// 1. Sentinel must be readable at /lever/<sentinelName> inside the jail.
	res, err := r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`cat /lever/`+sentinelName)
	if err != nil || !strings.Contains(res.Stdout, "lever-mount-ok") {
		t.Fatalf("sentinel not visible at /lever/%s: err=%v stdout=%q stderr=%q",
			sentinelName, err, res.Stdout, res.Stderr)
	}
	t.Logf("sentinel at /lever/%s: %q", sentinelName, strings.TrimSpace(res.Stdout))

	// 2. Host home (/Users) must NOT be accessible from inside the jail.
	res, _ = r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`ls /Users 2>/dev/null && echo VISIBLE || echo ABSENT`)
	if !strings.Contains(res.Stdout, "ABSENT") {
		t.Fatalf("host /Users is visible inside the jail (containment broken): %q", res.Stdout)
	}
	t.Logf("/Users inside jail: %q (expected ABSENT)", strings.TrimSpace(res.Stdout))

	// 3. ~/.ssh must NOT be accessible.
	res, _ = r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`ls ~/.ssh 2>/dev/null && echo VISIBLE || echo ABSENT`)
	if !strings.Contains(res.Stdout, "ABSENT") {
		t.Fatalf("~/.ssh is visible inside the jail (containment broken): %q", res.Stdout)
	}
	t.Logf("~/.ssh inside jail: %q (expected ABSENT)", strings.TrimSpace(res.Stdout))

	// 4. LAN must be unreachable from inside the jail.
	res, _ = r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`ping -c1 -W2 192.168.0.1 >/dev/null 2>&1 && echo REACHABLE || echo BLOCKED`)
	if !strings.Contains(res.Stdout, "BLOCKED") {
		t.Fatalf("LAN reachable from jail (containment broken): %q", res.Stdout)
	}
	t.Logf("LAN from jail: %q (expected BLOCKED)", strings.TrimSpace(res.Stdout))

	// 5. Rootless podman (scion's runtime) must run a container.
	res, err = r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`export XDG_RUNTIME_DIR=/run/user/$(id -u); podman run --rm docker.io/library/alpine echo OK`)
	if err != nil || !strings.Contains(res.Stdout, "OK") {
		t.Fatalf("rootless podman run failed: %v %q %q", err, res.Stdout, res.Stderr)
	}
	t.Logf("rootless podman: %q", strings.TrimSpace(res.Stdout))
}

// Run with: go test -tags integration -run TestRealOrbStackVerifiesMachine ./internal/backend/orbstack/ -v
// Creates and DELETES two throwaway machines. A machine lever made is reused
// (running, and after a stop); one made by hand without --isolated under a
// lever name is refused and not started.
func TestRealOrbStackVerifiesMachine(t *testing.T) {
	ctx := context.Background()
	r := proc.RealRunner{}
	tree := t.TempDir() // under /var, a symbolic link to /private/var on macOS

	own := New(r, "lever-it-verify-"+os.Getenv("USER"), common.Options{})
	t.Cleanup(func() { _ = own.Teardown(ctx) })
	if _, err := own.ensureMachine(ctx, tree); err != nil {
		t.Fatalf("create: %v", err)
	}
	if skip, err := own.ensureMachine(ctx, tree); err != nil || skip != "already running" {
		t.Fatalf("reuse running = %q, %v; want already running", skip, err)
	}
	if err := own.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := own.ensureMachine(ctx, tree); err != nil {
		t.Fatalf("reuse stopped: %v", err)
	}
	if _, err := own.ensureMachine(ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "mismatched containment config") {
		t.Fatalf("another project tree must be refused; got %v", err)
	}

	foreign := New(r, "lever-it-foreign-"+os.Getenv("USER"), common.Options{})
	t.Cleanup(func() { _ = foreign.Teardown(ctx) })
	if _, err := r.Run(ctx, nil, "orb", "create", "alpine", foreign.Machine()); err != nil {
		t.Fatalf("orb create (not isolated): %v", err)
	}
	_, err := foreign.ensureMachine(ctx, tree)
	if err == nil || !strings.Contains(err.Error(), "not isolated") {
		t.Fatalf("a non-isolated machine must be refused; got %v", err)
	}
	t.Logf("refused: %v", err)
	if err := foreign.Stop(ctx); err != nil {
		t.Fatalf("stop foreign: %v", err)
	}
	if _, err := foreign.ensureMachine(ctx, tree); err == nil {
		t.Fatal("a stopped non-isolated machine must be refused")
	}
	if stopped, err := foreign.MachineStopped(ctx); err != nil || !stopped {
		t.Fatalf("a refused stopped machine must stay stopped; stopped=%v err=%v", stopped, err)
	}
}
