package guest

import (
	"net"
	"strings"
)

// ParseAhosts parses `getent ahosts <alias>` output into one IPv4 and one
// IPv6 address: the LAST of each family listed. Chain building and
// Snapshot.VerifyEgress must share this parse so a live chain and its
// verification name the same address; changing the pick would make every
// existing chain mismatch once and be rebuilt.
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
