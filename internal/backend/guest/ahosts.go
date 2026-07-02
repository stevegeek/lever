package guest

import (
	"net"
	"strings"
)

// ParseAhosts parses `getent ahosts <alias>` output into the first IPv4 and
// first IPv6 address found. Every backend that resolves a host-alias domain
// (host.orb.internal, host.lima.internal, ...) runs getent through its own
// argv prefix but hits the identical output format, so the parse itself lives
// here once rather than being copied per backend.
func ParseAhosts(stdout string) (v4, v6 string) {
	for _, line := range strings.Split(stdout, "\n") {
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
	return v4, v6
}
