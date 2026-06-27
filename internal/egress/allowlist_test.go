package egress

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildRulesAllowsListedPortToBothAliasFamilies(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305}, false)
	v4 := familyArgs(rules, IPv4)
	v6 := familyArgs(rules, IPv6)

	if !containsRule(v4, "-d 0.250.250.254 -p tcp --dport 3305 -j ACCEPT") {
		t.Fatalf("missing v4 allow for 3305:\n%s", strings.Join(v4, "\n"))
	}
	if !containsRule(v6, "-d fd07:b51a:cc66:f0::fe -p tcp --dport 3305 -j ACCEPT") {
		t.Fatalf("missing v6 allow for 3305")
	}
}

func TestBuildRulesDropsRestToAliasAfterAllows(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305}, false)
	v4 := familyArgs(rules, IPv4)
	allowIdx := indexOfRule(v4, "--dport 3305 -j ACCEPT")
	dropIdx := indexOfRule(v4, "-d 0.250.250.254 -j DROP")
	if allowIdx < 0 || dropIdx < 0 || dropIdx < allowIdx {
		t.Fatalf("alias DROP must come AFTER the allow; allow=%d drop=%d", allowIdx, dropIdx)
	}
}

func TestBuildRulesDropsFC00AfterV6Allows(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305}, false)
	v6 := familyArgs(rules, IPv6)
	allowIdx := indexOfRule(v6, "--dport 3305 -j ACCEPT")
	dropIdx := indexOfRule(v6, "-d fc00::/7 -j DROP")
	if allowIdx < 0 || dropIdx < 0 || dropIdx < allowIdx {
		t.Fatalf("fc00::/7 DROP must come AFTER the v6 alias allow (alias is a /128 inside fc00::/7); allow=%d drop=%d", allowIdx, dropIdx)
	}
}

func TestBuildRulesDropsRFC1918AndIPv6(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", nil, false)
	v4 := familyArgs(rules, IPv4)
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !containsRule(v4, "-d "+cidr+" -j DROP") {
			t.Fatalf("missing RFC1918 drop for %s", cidr)
		}
	}
	v6 := familyArgs(rules, IPv6)
	if !containsRule(v6, "-d fe80::/10 -j DROP") || !containsRule(v6, "-d fc00::/7 -j DROP") {
		t.Fatalf("missing IPv6 link-local/ULA drops")
	}
}

func TestBuildRulesClosedInternetAppendsCatchAllDrop(t *testing.T) {
	open := BuildRules("10.0.0.1", "fd00::1", []int{8443}, false)
	closed := BuildRules("10.0.0.1", "fd00::1", []int{8443}, true)

	// Open posture is byte-identical to the pre-existing behavior: no catch-all DROP.
	if hasCatchAllDrop(open) {
		t.Fatal("open posture must NOT contain a catch-all OUTPUT DROP")
	}
	// Closed posture appends a catch-all DROP for BOTH families, after the ACCEPTs.
	if !hasCatchAllDropFamily(closed, IPv4) || !hasCatchAllDropFamily(closed, IPv6) {
		t.Fatal("closed posture must append a catch-all OUTPUT DROP for v4 and v6")
	}
	// The broker-port ACCEPT precedes the catch-all DROP.
	if acceptIdx(closed, "8443") > dropIdx(closed) {
		t.Fatal("broker-port ACCEPT must precede the catch-all DROP")
	}
}

// helpers
func familyArgs(rules []Rule, fam Family) []string {
	var out []string
	for _, r := range rules {
		if r.Family == fam {
			out = append(out, strings.Join(r.Args, " "))
		}
	}
	return out
}
func containsRule(lines []string, needle string) bool { return indexOfRule(lines, needle) >= 0 }
func indexOfRule(lines []string, needle string) int {
	for i, l := range lines {
		if strings.Contains(l, needle) {
			return i
		}
	}
	return -1
}

// helpers for closedInternet test
func hasCatchAllDropFamily(rules []Rule, fam Family) bool {
	catchAll := []string{"-A", "OUTPUT", "-j", "DROP"}
	for _, r := range rules {
		if r.Family == fam && slices.Equal(r.Args, catchAll) {
			return true
		}
	}
	return false
}

func hasCatchAllDrop(rules []Rule) bool {
	return hasCatchAllDropFamily(rules, IPv4) || hasCatchAllDropFamily(rules, IPv6)
}

func acceptIdx(rules []Rule, port string) int {
	for i, r := range rules {
		args := strings.Join(r.Args, " ")
		if strings.Contains(args, "--dport "+port) && strings.Contains(args, "-j ACCEPT") {
			return i
		}
	}
	return -1
}

func dropIdx(rules []Rule) int {
	catchAll := []string{"-A", "OUTPUT", "-j", "DROP"}
	for i, r := range rules {
		if slices.Equal(r.Args, catchAll) {
			return i
		}
	}
	return -1
}
