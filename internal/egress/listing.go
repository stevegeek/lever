package egress

import (
	"net"
	"strings"
)

// Listing renders the rules of family f the way `iptables -S Chain` prints
// them once loaded: iptables adds the full prefix length to a bare address,
// the implicit protocol match before a port or ICMP type, and prints an ICMP
// type as type/code.
//
// A re-apply verifies the live chain against it (ListingMatches). Only the
// expected side is transformed, so a rule shape this rendering does not know
// can only cause a mismatch (a rebuild), never a false match.
func Listing(rules []Rule, f Family) []string {
	out := []string{"-N " + Chain}
	for _, r := range rules {
		if r.Family == f {
			out = append(out, listingLine(r.Args, f))
		}
	}
	return out
}

func listingLine(args []string, f Family) string {
	var b []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-d" || a == "-s") && i+1 < len(args):
			b = append(b, a, listingAddr(args[i+1], f))
			i++
		case a == "-p" && i+2 < len(args) && (args[i+2] == "--dport" || args[i+2] == "--sport"):
			b = append(b, a, args[i+1], "-m", args[i+1])
			i++
		case a == "-p" && i+2 < len(args) && args[i+1] == "icmp" && args[i+2] == "--icmp-type":
			b = append(b, a, "icmp", "-m", "icmp")
			i++
		case a == "--icmp-type" && i+1 < len(args):
			b = append(b, a, listingICMPType(args[i+1]))
			i++
		default:
			b = append(b, a)
		}
	}
	return strings.Join(b, " ")
}

// listingAddr renders a CIDR as its network and a bare address with the
// family's full prefix. A non-address is left as is and will not match.
func listingAddr(s string, f Family) string {
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n.String()
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if f == IPv4 && ip.To4() != nil {
		return ip.To4().String() + "/32"
	}
	return ip.String() + "/128"
}

// icmpTypeCodes maps the ICMP type names the ruleset uses to iptables -S
// type/code form. A missing name cannot match: a rebuild, never a false match.
var icmpTypeCodes = map[string]string{
	"host-prohibited": "3/10",
}

func listingICMPType(s string) string {
	if tc, ok := icmpTypeCodes[s]; ok {
		return tc
	}
	return s
}

// ListingMatches reports whether live (`iptables -S Chain` for family f) is
// exactly the chain RestoreInput(rules, f) would commit. Blank lines are
// ignored; any other difference is a mismatch.
func ListingMatches(rules []Rule, f Family, live string) bool {
	var got []string
	for _, line := range strings.Split(live, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got = append(got, line)
		}
	}
	want := Listing(rules, f)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
