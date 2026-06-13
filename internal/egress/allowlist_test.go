package egress

import (
	"strings"
	"testing"
)

func TestBuildRulesAllowsListedPortToBothAliasFamilies(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305})
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
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305})
	v4 := familyArgs(rules, IPv4)
	allowIdx := indexOfRule(v4, "--dport 3305 -j ACCEPT")
	dropIdx := indexOfRule(v4, "-d 0.250.250.254 -j DROP")
	if allowIdx < 0 || dropIdx < 0 || dropIdx < allowIdx {
		t.Fatalf("alias DROP must come AFTER the allow; allow=%d drop=%d", allowIdx, dropIdx)
	}
}

func TestBuildRulesDropsFC00AfterV6Allows(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305})
	v6 := familyArgs(rules, IPv6)
	allowIdx := indexOfRule(v6, "--dport 3305 -j ACCEPT")
	dropIdx := indexOfRule(v6, "-d fc00::/7 -j DROP")
	if allowIdx < 0 || dropIdx < 0 || dropIdx < allowIdx {
		t.Fatalf("fc00::/7 DROP must come AFTER the v6 alias allow (alias is a /128 inside fc00::/7); allow=%d drop=%d", allowIdx, dropIdx)
	}
}

func TestBuildRulesDropsRFC1918AndIPv6(t *testing.T) {
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", nil)
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
