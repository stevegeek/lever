//go:build integration

package orbstack

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

// Run with: go test -tags integration -run TestRealOrbStack ./internal/backend/orbstack/ -v
// Requires OrbStack running. Creates and DELETES a throwaway jail.
func TestRealOrbStack(t *testing.T) {
	machine := "lever-it-" + os.Getenv("USER")
	b := New(exec.RealRunner{}, machine)
	ctx := context.Background()
	t.Cleanup(func() { _ = b.Teardown(ctx) })

	if err := b.EnsureUp(ctx, backend.Config{MachineName: machine, ProjectTree: os.TempDir(), AllowedPorts: nil}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	r := exec.RealRunner{}
	// LAN must be unreachable from inside the jail.
	res, _ := r.Run(ctx, nil, "orb", "-m", machine, "bash", "-lc",
		`ping -c1 -W2 192.168.0.1 >/dev/null 2>&1 && echo REACHABLE || echo BLOCKED`)
	if !strings.Contains(res.Stdout, "BLOCKED") {
		t.Fatalf("LAN reachable from jail (containment broken): %q", res.Stdout)
	}

	// Rootless docker must run a container.
	res, err := r.Run(ctx, map[string]string{"DOCKER_HOST": b.DockerHost()}, "orb", "-m", machine, "bash", "-lc",
		`export XDG_RUNTIME_DIR=/run/user/$(id -u); docker run --rm alpine echo OK`)
	if err != nil || !strings.Contains(res.Stdout, "OK") {
		t.Fatalf("rootless docker run failed: %v %q %q", err, res.Stdout, res.Stderr)
	}
}
