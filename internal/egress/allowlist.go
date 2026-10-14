// Package egress generates the jail's network egress allowlist as iptables/
// ip6tables argv. Policy: ACCEPT allowlisted tool ports on the host alias,
// DROP everything else to the alias, DROP private/LAN ranges explicitly
// (defence in depth beyond backend routing), leave the public internet open
// unless the closed posture is asked for. Rule order matters: allows precede
// drops.
package egress

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

// Chain is the dedicated iptables chain lever's egress rules live in. OUTPUT
// jumps to it; ApplyEgress replaces only this chain, atomically (one
// iptables-restore --noflush commit per family), so a re-apply never
// accumulates rules and never leaves it empty.
const Chain = "LEVER_EGRESS"

type Family int

const (
	IPv4 Family = iota
	IPv6
)

type Rule struct {
	Family Family
	Args   []string // argv after the binary, e.g. ["-A","OUTPUT","-d","...","-j","DROP"]
}

var (
	// private/special-use IPv4: RFC1918 + link-local/metadata (169.254/16) + CGNAT/Tailscale (100.64/10)
	privateV4 = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}
	ipv6Local = []string{"fe80::/10", "fc00::/7"} // link-local + ULA (the host alias ULA is a /128 inside fc00::/7, so allow-before-drop ordering is essential)
)

// MetadataV4 is the cloud instance-metadata address. It lies inside
// 169.254/16 but is the one address REJECTed rather than dropped: the scion
// hub looks up a GCP project ID there at every start, and a dropped dial
// costs it about 13 s of retried timeouts, while EHOSTUNREACH ends the
// lookup at once. A REJECT still delivers nothing to the address, and every
// other link-local address stays dropped.
const MetadataV4 = "169.254.169.254"

func metadataReject() []string {
	return []string{"-d", MetadataV4 + "/32", "-j", "REJECT", "--reject-with", "icmp-host-prohibited"}
}

// rejectReplyAccept lets the REJECT's ICMP answer reach the dialling socket.
// The kernel sends it from and to the guest's own address through lo and
// OUTPUT; on Lima that address is in 192.168/16, whose DROP would swallow it
// and hang the dial. (The closed posture's lo ACCEPT already covers it.)
// Only host-prohibited, only on lo (never leaving the guest), and only
// RELATED to a connection conntrack saw.
func rejectReplyAccept() []string {
	return []string{"-o", "lo", "-p", "icmp", "--icmp-type", "host-prohibited", "-m", "conntrack", "--ctstate", "RELATED", "-j", "ACCEPT"}
}

// BuildRules returns ordered OUTPUT rules. aliasV4/aliasV6 are the resolved
// host alias addresses; allowedPorts are host-loopback tool ports. It
// assumes OUTPUT's default policy is ACCEPT.
//
// When closedInternet is true a catch-all DROP follows all per-port ACCEPTs,
// so the jail reaches only the ACCEPTed destinations (the broker port on the
// host alias): the api-key mode posture. Both postures carry rule 1b (an
// alias-scoped ESTABLISHED ACCEPT for host-initiated control channels) and
// rule 2b (the metadata REJECT, MetadataV4).
//
// On config-driven paths allowedPorts is config.App.EffectiveAllowedPorts():
// the broker's jail port, manager.allow_ports, and, while remote access is
// on, the local OIDC provider's port that the guest's login forwarder dials.
// This allowlist is the one place that grant is enforced, so a missing one
// shows up as a hung discovery fetch, not as anything mentioning firewalls.
func BuildRules(aliasV4, aliasV6 string, allowedPorts []int, closedInternet bool) []Rule {
	return BuildRulesDNS(aliasV4, aliasV6, allowedPorts, closedInternet, nil)
}

// DNSForward is one destination the guest's DNS traffic is rewritten to on
// the host alias, read from a backend's nat-table DNAT (ParseDNATTargets).
// On Lima the resolver address is DNATed to <host alias>:<per-boot port>
// before the filter OUTPUT chain sees it, so without an ACCEPT every lookup
// hits the alias DROP.
type DNSForward struct {
	Proto string // "udp" | "tcp"
	Port  int
}

// ParseDNATTargets extracts the DNSForward targets from `iptables -t nat -S
// <chain>` output whose --to-destination host is aliasV4 (Lima's LIMADNS
// chain: `-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT
// --to-destination 192.168.5.2:41234`). Only tcp/udp DNAT rules to the alias
// count, so the caller never ACCEPTs a port the resolver does not use.
func ParseDNATTargets(out, aliasV4 string) []DNSForward {
	var targets []DNSForward
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "-A" {
			continue
		}
		var proto, dest string
		var dnat bool
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "-p":
				proto = f[i+1]
			case "-j":
				dnat = f[i+1] == "DNAT"
			case "--to-destination":
				dest = f[i+1]
			}
		}
		if !dnat || (proto != "udp" && proto != "tcp") {
			continue
		}
		host, portStr, err := net.SplitHostPort(dest)
		if err != nil || host != aliasV4 {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		targets = append(targets, DNSForward{proto, port})
	}
	return targets
}

// ParseResolverUpstreams returns udp and tcp port-53 DNSForward targets when
// a resolv.conf listing (systemd-resolved's upstream file) names aliasV4 as a
// nameserver: a Lima vz guest talks to <alias>:53 directly, with an empty
// LIMADNS chain.
func ParseResolverUpstreams(resolvConf, aliasV4 string) []DNSForward {
	if aliasV4 == "" {
		return nil
	}
	for _, line := range strings.Split(resolvConf, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" && f[1] == aliasV4 {
			return []DNSForward{{"udp", 53}, {"tcp", 53}}
		}
	}
	return nil
}

// BuildRulesDNS is BuildRules plus, in the open posture only, an ACCEPT to
// the v4 alias for each DNS target, before the alias DROP. In the closed
// posture dns is ignored: DNS stays dropped by design (agents dial the
// broker by IP) and opening the resolver would widen containment.
func BuildRulesDNS(aliasV4, aliasV6 string, allowedPorts []int, closedInternet bool, dns []DNSForward) []Rule {
	ports := append([]int(nil), allowedPorts...)
	sort.Ints(ports)
	var rules []Rule

	// 0) Closed posture: ACCEPT loopback first, or the catch-all DROP also
	// breaks in-machine localhost services (the scion hub on 127.0.0.1:8080,
	// host-loopback tools). The open posture's default ACCEPT covers it.
	if closedInternet {
		rules = append(rules,
			Rule{IPv4, out("-o", "lo", "-j", "ACCEPT")},
			Rule{IPv6, out("-o", "lo", "-j", "ACCEPT")},
		)
	}

	// 1) ACCEPT allowlisted ports to each alias family, before any drop.
	for _, p := range ports {
		if aliasV4 != "" {
			rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
		if aliasV6 != "" {
			rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
	}
	// 1a) Open posture only: ACCEPT the guest resolver's DNAT targets on the
	// alias.
	if !closedInternet && aliasV4 != "" {
		for _, d := range dns {
			rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-p", d.Proto, "--dport", strconv.Itoa(d.Port), "-j", "ACCEPT")})
		}
	}
	// 1b) ACCEPT ESTABLISHED traffic to the host alias, before the alias DROP.
	// Some backends' control channel is a connection the host initiates (Lima's
	// SSH, seen by the guest as the alias IP), and the guest's replies go
	// OUTPUT to the alias. A new dial is state NEW and still meets the per-port
	// allow / DROP / catch-all, so containment is unchanged. Not RELATED: that
	// would also admit conntrack-helper expectations (the FTP/SIP ALG bypass
	// class) that nothing needs. Scoped to the alias so it never broadens
	// internet egress.
	if aliasV4 != "" {
		rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT")})
	}
	if aliasV6 != "" {
		rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT")})
	}
	// 2) DROP everything else to the alias.
	if aliasV4 != "" {
		rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-j", "DROP")})
	}
	if aliasV6 != "" {
		rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-j", "DROP")})
	}
	// 2b) REJECT the metadata address just before the 169.254/16 DROP (see
	// MetadataV4), in both postures. The only REJECT in the chain; a /32
	// that admits nothing. Its ICMP answer needs rejectReplyAccept first.
	rules = append(rules,
		Rule{IPv4, out(rejectReplyAccept()...)},
		Rule{IPv4, out(metadataReject()...)},
	)
	// 3) DROP private/LAN ranges explicitly; do not rely on backend routing.
	for _, c := range privateV4 {
		rules = append(rules, Rule{IPv4, out("-d", c, "-j", "DROP")})
	}
	for _, c := range ipv6Local {
		rules = append(rules, Rule{IPv6, out("-d", c, "-j", "DROP")})
	}
	if closedInternet {
		// Catch-all: the jail reaches only what was ACCEPTed above (the
		// broker port), so LLM traffic must flow through the broker. It must
		// follow the per-port ACCEPTs.
		rules = append(rules,
			Rule{IPv4, out("-j", "DROP")},
			Rule{IPv6, out("-j", "DROP")},
		)
	}
	return rules
}

func out(a ...string) []string { return append([]string{"-A", Chain}, a...) }

func (f Family) Binary() string {
	if f == IPv6 {
		return "ip6tables"
	}
	return "iptables"
}

func (f Family) RestoreBinary() string { return f.Binary() + "-restore" }

// RestoreInput renders the rules of family f as `iptables-restore --noflush`
// input that replaces Chain in one commit: the `:Chain` declaration empties
// the chain inside the transaction and COMMIT applies all or nothing.
// --noflush leaves every other chain alone. Rule args contain no whitespace
// or quotes, so joining them with spaces is the exact line.
func RestoreInput(rules []Rule, f Family) string {
	var b strings.Builder
	b.WriteString("*filter\n:" + Chain + " - [0:0]\n")
	for _, r := range rules {
		if r.Family == f {
			b.WriteString(strings.Join(r.Args, " ") + "\n")
		}
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// Render is a debug helper.
func (r Rule) Render() string { return r.Family.Binary() + " " + strings.Join(r.Args, " ") }

// DroppedForJail reports whether ip lies in a range the jail's egress rules
// refuse in every posture (the private ranges of rule 3; MetadataV4 is
// REJECTed instead), so no jailed process can connect to it on any port.
// config.validateRemote accepts a non-loopback remote.bind only on such an
// address. Loopback is not in the set: the jail reaches host loopback
// through the alias, governed per port.
func DroppedForJail(ip net.IP) bool {
	ranges := privateV4
	if ip.To4() == nil {
		ranges = ipv6Local
	}
	for _, c := range ranges {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
