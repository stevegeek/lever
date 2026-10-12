package egress

import (
	"net"
	"strings"
)

// Listing renders the rules of family f the way `iptables -S Chain` (or
// ip6tables) prints the chain once RestoreInput has loaded them: the `-N`
// line, then one `-A` line per rule, in order. iptables prints a bare
// destination address with its full prefix length (`-d 192.168.5.2/32`,
// `-d fd07::fe/128`) and adds the implicit protocol match before a port
// (`-p tcp -m tcp --dport 8443`), and likewise before an ICMP type, which it
// prints as type/code (`-p icmp -m icmp --icmp-type 3/10` for
// host-prohibited); everything else prints as written.
//
// It exists so a re-apply can VERIFY the live chain against the ruleset it
// would commit (ListingMatches) instead of assuming it. The comparison is
// exact, and only the expected side is transformed, so a rule shape this
// rendering does not know can only make the comparison fail, which costs a
// rebuild. It can never make a different live chain compare equal.
func Listing(rules []Rule, f Family) []string {
	out := []string{"-N " + Chain}
	for _, r := range rules {
		if r.Family == f {
			out = append(out, listingLine(r.Args, f))
		}
	}
	return out
}

// listingLine is one rule's args in iptables -S form.
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

// listingAddr is an address as iptables prints it: a CIDR as its network,
// a bare address with the family's full prefix. Anything that is not an
// address is left as it is (and will then not match a live listing).
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

// icmpTypeCodes are the ICMP type names the ruleset uses, as iptables -S
// prints them (type/code). A name missing here is left as written, which
// cannot match a live listing: it costs a rebuild, never a false match.
var icmpTypeCodes = map[string]string{
	"host-prohibited": "3/10",
}

// listingICMPType is an ICMP type as iptables prints it.
func listingICMPType(s string) string {
	if tc, ok := icmpTypeCodes[s]; ok {
		return tc
	}
	return s
}

// ListingMatches reports whether live, the output of `iptables -S Chain` (or
// ip6tables) for family f, is exactly the chain RestoreInput(rules, f) would
// commit: the same rules in the same order, and nothing else. Blank lines are
// ignored; any other extra, missing or different line is a mismatch.
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
