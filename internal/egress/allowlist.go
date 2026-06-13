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
	rfc1918   = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	ipv6Local = []string{"fe80::/10", "fc00::/7"} // link-local + ULA (the host alias ULA is a /128 inside fc00::/7, so allow-before-drop ordering is essential)
)

// BuildRules returns ordered OUTPUT rules. aliasV4/aliasV6 are the resolved
// host.orb.internal addresses; allowedPorts are host-loopback tool ports.
func BuildRules(aliasV4, aliasV6 string, allowedPorts []int) []Rule {
	ports := append([]int(nil), allowedPorts...)
	sort.Ints(ports)
	var rules []Rule

	// 1) ACCEPT allowlisted ports to each alias family (BEFORE any drop).
	for _, p := range ports {
		if aliasV4 != "" {
			rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
		if aliasV6 != "" {
			rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "ACCEPT")})
		}
	}
	// 2) DROP everything else to the alias (closes non-allowlisted host loopback).
	if aliasV4 != "" {
		rules = append(rules, Rule{IPv4, out("-d", aliasV4, "-j", "DROP")})
	}
	if aliasV6 != "" {
		rules = append(rules, Rule{IPv6, out("-d", aliasV6, "-j", "DROP")})
	}
	// 3) DROP private/LAN ranges explicitly (don't rely on OrbStack routing alone).
	for _, c := range rfc1918 {
		rules = append(rules, Rule{IPv4, out("-d", c, "-j", "DROP")})
	}
	for _, c := range ipv6Local {
		rules = append(rules, Rule{IPv6, out("-d", c, "-j", "DROP")})
	}
	return rules
}

func out(a ...string) []string { return append([]string{"-A", "OUTPUT"}, a...) }

// Binary returns the iptables binary name for a family.
func (f Family) Binary() string {
	if f == IPv6 {
		return "ip6tables"
	}
	return "iptables"
}

// Render is a debug helper: "iptables -A OUTPUT ...".
func (r Rule) Render() string { return r.Family.Binary() + " " + strings.Join(r.Args, " ") }
