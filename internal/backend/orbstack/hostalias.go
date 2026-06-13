package orbstack

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/lever-to/lever/internal/exec"
)

// resolveHostAlias returns the IPv4 and IPv6 addresses host.orb.internal
// resolves to FROM INSIDE the machine (both forward to the host's 127.0.0.1).
func resolveHostAlias(ctx context.Context, r exec.Runner, machine string) (v4, v6 string, err error) {
	res, err := r.Run(ctx, nil, "orb", "-m", machine, "getent", "ahosts", "host.orb.internal")
	if err != nil {
		return "", "", fmt.Errorf("getent host.orb.internal: %w", err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			v4 = fields[0]
		} else {
			v6 = fields[0]
		}
	}
	if v4 == "" && v6 == "" {
		return "", "", fmt.Errorf("host.orb.internal resolved to no addresses")
	}
	return v4, v6, nil
}
