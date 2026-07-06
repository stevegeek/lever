package orbstack

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/exec"
)

// resolveHostAlias returns the IPv4 and IPv6 addresses host.orb.internal
// resolves to FROM INSIDE the machine (both forward to the host's 127.0.0.1).
func resolveHostAlias(ctx context.Context, r exec.Runner, machine string) (v4, v6 string, err error) {
	res, err := r.Run(ctx, nil, "orb", "-m", machine, "getent", "ahosts", "host.orb.internal")
	if err != nil {
		return "", "", fmt.Errorf("getent host.orb.internal: %w", err)
	}
	v4, v6 = guest.ParseAhosts(res.Stdout)
	if v4 == "" && v6 == "" {
		return "", "", fmt.Errorf("host.orb.internal resolved to no addresses")
	}
	return v4, v6, nil
}
