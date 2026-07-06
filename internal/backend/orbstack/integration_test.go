//go:build integration

package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/exec"
)

// Run with: go test -tags integration -run TestRealOrbStack ./internal/backend/orbstack/ -v
// Requires OrbStack running. Creates and DELETES a throwaway jail.
func TestRealOrbStack(t *testing.T) {
	machine := "lever-it-" + os.Getenv("USER")
	b := New(exec.RealRunner{}, machine)
	ctx := context.Background()
	t.Cleanup(func() { _ = b.Teardown(ctx) })

	// Create a temp dir with a sentinel file to verify the project-tree mount.
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

	r := exec.RealRunner{}

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

	// 3. ~/.ssh must NOT be accessible (belt-and-suspenders check).
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

	// 5. Rootless docker must run a container.
	res, err = r.Run(ctx, map[string]string{"DOCKER_HOST": b.DockerHost()}, "orb", "-m", machine, "bash", "-lc",
		`export XDG_RUNTIME_DIR=/run/user/$(id -u); docker run --rm alpine echo OK`)
	if err != nil || !strings.Contains(res.Stdout, "OK") {
		t.Fatalf("rootless docker run failed: %v %q %q", err, res.Stdout, res.Stderr)
	}
	t.Logf("rootless docker: %q", strings.TrimSpace(res.Stdout))
}
