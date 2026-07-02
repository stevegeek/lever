// Package egress generates the jail's network egress allowlist as iptables/
// ip6tables OUTPUT-chain argv. Policy: ACCEPT allowlisted tool ports on the
// host alias, DROP everything else TO the alias, DROP private/LAN ranges
// explicitly (defence-in-depth beyond OrbStack routing), leave the public
// internet open. Rule ORDER matters: allows precede drops.
package egress

import (
	"sort"
	"strconv"
	"strings"
)

// Chain is the dedicated iptables chain lever's egress rules live in. OUTPUT
// jumps to it; ApplyEgress flushes ONLY this chain before re-populating, so a
// re-apply is idempotent (no rule accumulation) and — because flushing removes
// the catch-all DROP — DNS works again for the host-alias re-resolve.
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

// BuildRules returns ordered OUTPUT rules. aliasV4/aliasV6 are the resolved
// host.orb.internal addresses; allowedPorts are host-loopback tool ports.
// Assumes the OUTPUT chain default policy is ACCEPT: the public internet stays
// open; only the host alias and the listed private/special-use ranges are dropped.
// When closedInternet is true a catch-all OUTPUT DROP is appended AFTER all
// per-port ACCEPTs, so the jail can reach ONLY the already-ACCEPTed destinations
// (the broker port on the host alias). This is the api-key mode posture.
// When closedInternet is false behaviour is byte-identical to the pre-existing open
// posture (no catch-all DROP; public internet remains reachable).
func BuildRules(aliasV4, aliasV6 string, allowedPorts []int, closedInternet bool) []Rule {
	ports := append([]int(nil), allowedPorts...)
	sort.Ints(ports)
	var rules []Rule

	// 0) When closing the internet, ACCEPT loopback FIRST. The catch-all DROP
	// below matches locally-generated packets to 127.0.0.1/::1 too, so without
	// this every in-machine localhost service breaks — the scion hub on
	// 127.0.0.1:8080 (its readiness check) and host-loopback tools. Only added in
	// the closed posture so the open posture stays byte-identical to pre-existing
	// (where the default-ACCEPT policy already permits loopback).
	if closedInternet {
		rules = append(rules,
			Rule{Family: IPv4, Args: []string{"-A", Chain, "-o", "lo", "-j", "ACCEPT"}},
			Rule{Family: IPv6, Args: []string{"-A", Chain, "-o", "lo", "-j", "ACCEPT"}},
		)
	}

	// 1) ACCEPT allowlisted ports to each alias family (BEFORE any drop).
	for _, p := range ports {
		if aliasV4 != "" {
			rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
		if aliasV6 != "" {
			rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
	}
	// 1b) ACCEPT established replies TO THE HOST ALIAS, before the alias DROP
	// below. On some backends lever's own control channel into the jail is a
	// connection the HOST initiates to the guest — Lima drives the guest over SSH
	// from the host, which the guest sees as the alias IP — and the guest's replies
	// flow OUTPUT→alias, so the blanket alias DROP below would sever that channel.
	// Accepting only ESTABLISHED traffic TO THE ALIAS lets the guest answer
	// host-initiated connections WITHOUT letting it initiate any new outbound: a new
	// connection is state NEW and still falls through to the per-port allow / DROP /
	// catch-all, so the egress containment assertions are unchanged (a NEW dial to a
	// non-allowlisted alias port, or to the internet under closed egress, is still
	// dropped). Scoped to ESTABLISHED only, not RELATED: Lima's host→guest SSH reply
	// traffic is a single ESTABLISHED TCP flow, and RELATED would additionally admit
	// conntrack-helper-spawned expectations (the FTP/SIP ALG bypass class) that
	// nothing here needs — least privilege, defence in depth. Scoped to the alias so
	// it never broadens internet egress. Inert on backends whose transport isn't a
	// host→guest connection (e.g. OrbStack's `orb`).
	if aliasV4 != "" {
		rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT")})
	}
	if aliasV6 != "" {
		rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT")})
	}
	// 2) DROP everything else to the alias (closes non-allowlisted host loopback).
	if aliasV4 != "" {
		rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-j", "DROP")})
	}
	if aliasV6 != "" {
		rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-j", "DROP")})
	}
	// 3) DROP private/LAN ranges explicitly (don't rely on OrbStack routing alone).
	for _, c := range privateV4 {
		rules = append(rules, Rule{IPv4, out("-d", c, "-j", "DROP")})
	}
	for _, c := range ipv6Local {
		rules = append(rules, Rule{IPv6, out("-d", c, "-j", "DROP")})
	}
	if closedInternet {
		// Final catch-all: drop everything not already ACCEPTed above (the broker
		// port to the host alias). The jail can then reach ONLY the broker — LLM
		// traffic must flow broker→Anthropic. Order matters: this follows the
		// per-port ACCEPTs.
		rules = append(rules,
			Rule{Family: IPv4, Args: []string{"-A", Chain, "-j", "DROP"}},
			Rule{Family: IPv6, Args: []string{"-A", Chain, "-j", "DROP"}},
		)
	}
	return rules
}

func out(a ...string) []string { return append([]string{"-A", Chain}, a...) }

// Binary returns the iptables binary name for a family.
func (f Family) Binary() string {
	if f == IPv6 {
		return "ip6tables"
	}
	return "iptables"
}

// Render is a debug helper: "iptables -A OUTPUT ...".
func (r Rule) Render() string { return r.Family.Binary() + " " + strings.Join(r.Args, " ") }
