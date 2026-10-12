package guest

import (
	"net"
	"strings"
)

// ParseAhosts parses `getent ahosts <alias>` output into one IPv4 and one
// IPv6 address: the LAST of each family listed (each address appears once
// per socket type, so for an alias with one address per family, the usual
// case, first and last are the same). Both callers that build chains from it
// (the backends' ResolveHostAlias and Snapshot.VerifyEgress) use this same
// parse, so a live chain and its verification always name the same address;
// changing the pick would make every existing chain mismatch once and be
// rebuilt. Every backend that resolves a host-alias domain
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
