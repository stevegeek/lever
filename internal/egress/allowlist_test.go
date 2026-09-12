package egress

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildRulesTargetDedicatedChain(t *testing.T) {
	// Rules append to the dedicated LEVER_EGRESS chain (which OUTPUT jumps to),
	// NOT OUTPUT directly — so ApplyEgress can flush ONLY lever's rules on
	// re-apply (idempotent; no accumulation; restores DNS before re-resolving).
	rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305}, true)
	for i, r := range rules {
		if len(r.Args) < 2 || r.Args[0] != "-A" || r.Args[1] != Chain {
			t.Fatalf("rule %d %v must append to %s, not %q", i, r.Args, Chain, r.Args[1])
		}
	}
}

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

// TestBuildRulesAcceptsEstablishedRepliesToAliasBeforeDrop pins the stateful
// carve-out that keeps a host→guest control channel (Lima's SSH) alive: replies
// to the host alias on ESTABLISHED connections are ACCEPTed before the
// blanket alias DROP. Scoped to ESTABLISHED only (not RELATED): Lima's
// host→guest SSH replies are a single ESTABLISHED TCP flow, and RELATED would
// additionally admit conntrack-helper-spawned expectations (the FTP/SIP ALG
// bypass class) for no need here. It is scoped to the alias (never the
// internet) and present in BOTH postures (the alias DROP exists in both). A
// NEW dial is unaffected, so containment is unchanged.
func TestBuildRulesAcceptsEstablishedRepliesToAliasBeforeDrop(t *testing.T) {
	for _, closed := range []bool{false, true} {
		rules := BuildRules("0.250.250.254", "fd07:b51a:cc66:f0::fe", []int{3305}, closed)
		for _, fam := range []Family{IPv4, IPv6} {
			args := familyArgs(rules, fam)
			estIdx := indexOfRule(args, "-m conntrack --ctstate ESTABLISHED -j ACCEPT")
			var dropNeedle string
			if fam == IPv4 {
				dropNeedle = "-d 0.250.250.254 -j DROP"
			} else {
				dropNeedle = "-d fd07:b51a:cc66:f0::fe -j DROP"
			}
			dropIdx := indexOfRule(args, dropNeedle)
			if estIdx < 0 {
				t.Fatalf("closed=%v family=%v: missing ESTABLISHED accept to alias:\n%s", closed, fam, strings.Join(args, "\n"))
			}
			if dropIdx < 0 || estIdx > dropIdx {
				t.Fatalf("closed=%v family=%v: established accept (idx %d) must precede the alias DROP (idx %d)", closed, fam, estIdx, dropIdx)
			}
		}
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

func TestBuildRulesClosedInternetExemptsLoopback(t *testing.T) {
	open := BuildRules("10.0.0.1", "fd00::1", []int{8443}, false)
	closed := BuildRules("10.0.0.1", "fd00::1", []int{8443}, true)

	// Closed posture MUST allow loopback before the catch-all DROP, or in-machine
	// localhost traffic (the scion hub on 127.0.0.1:8080, host-loopback tools) is
	// dropped. Required for BOTH families.
	for _, fam := range []Family{IPv4, IPv6} {
		lo := loIdx(closed, fam)
		if lo < 0 {
			t.Fatalf("closed posture must ACCEPT loopback (-o lo) for family %v", fam)
		}
		if drop := dropIdxFamily(closed, fam); drop >= 0 && lo > drop {
			t.Fatalf("loopback ACCEPT (idx %d) must precede the catch-all DROP (idx %d) for family %v", lo, drop, fam)
		}
	}
	// Open posture stays byte-identical to pre-existing: no loopback rule added.
	if loIdx(open, IPv4) >= 0 || loIdx(open, IPv6) >= 0 {
		t.Fatal("open posture must NOT add a loopback rule (byte-identical to pre-existing)")
	}
}

// loIdx returns the index of the `-o lo -j ACCEPT` rule for a family, or -1.
func loIdx(rules []Rule, fam Family) int {
	for i, r := range rules {
		args := strings.Join(r.Args, " ")
		if r.Family == fam && strings.Contains(args, "-o lo") && strings.Contains(args, "-j ACCEPT") {
			return i
		}
	}
	return -1
}

// dropIdxFamily returns the index of the catch-all DROP for a family, or -1.
func dropIdxFamily(rules []Rule, fam Family) int {
	catchAll := []string{"-A", Chain, "-j", "DROP"}
	for i, r := range rules {
		if r.Family == fam && slices.Equal(r.Args, catchAll) {
			return i
		}
	}
	return -1
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
	catchAll := []string{"-A", Chain, "-j", "DROP"}
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
	catchAll := []string{"-A", Chain, "-j", "DROP"}
	for i, r := range rules {
		if slices.Equal(r.Args, catchAll) {
			return i
		}
	}
	return -1
}

// --- lever#34: Lima's LIMADNS DNAT targets (guest resolver → host alias:port) ---

const limaDNSChain = "-N LIMADNS\n" +
	"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT --to-destination 192.168.5.2:41234\n" +
	"-A LIMADNS -d 192.168.5.3/32 -p tcp -m tcp --dport 53 -j DNAT --to-destination 192.168.5.2:41235\n"

func TestParseDNATTargetsReadsLimaDNSChain(t *testing.T) {
	got := ParseDNATTargets(limaDNSChain, "192.168.5.2")
	want := []DNSForward{{"udp", 41234}, {"tcp", 41235}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseDNATTargetsIgnoresOtherHostsAndJunk(t *testing.T) {
	out := "-N LIMADNS\n" +
		"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT --to-destination 10.9.9.9:53\n" + // not the alias
		"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT --to-destination 192.168.5.2:notaport\n" +
		"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT --to-destination 192.168.5.2\n" + // no port
		"-A LIMADNS -d 192.168.5.3/32 -p sctp --dport 53 -j DNAT --to-destination 192.168.5.2:1\n" + // not tcp/udp
		"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j ACCEPT\n" + // not a DNAT
		"garbage line\n"
	if got := ParseDNATTargets(out, "192.168.5.2"); len(got) != 0 {
		t.Fatalf("expected no targets, got %v", got)
	}
	if got := ParseDNATTargets("", "192.168.5.2"); len(got) != 0 {
		t.Fatalf("empty input must yield no targets, got %v", got)
	}
}

// TestBuildRulesDNSAcceptsForwardTargetsBeforeAliasDropOpenOnly pins the
// lever#34 fix: in the OPEN posture the DNAT targets of the guest's resolver
// are ACCEPTed on the alias, BEFORE the alias DROP, for exactly the parsed
// proto/port pairs. In the CLOSED posture DNS stays dropped by design (agents
// dial the broker by IP), so no such rule is emitted.
func TestBuildRulesDNSAcceptsForwardTargetsBeforeAliasDropOpenOnly(t *testing.T) {
	dns := []DNSForward{{"udp", 41234}, {"tcp", 41235}}
	open := familyArgs(BuildRulesDNS("192.168.5.2", "", []int{8443}, false, dns), IPv4)
	udpIdx := indexOfRule(open, "-d 192.168.5.2 -p udp --dport 41234 -j ACCEPT")
	tcpIdx := indexOfRule(open, "-d 192.168.5.2 -p tcp --dport 41235 -j ACCEPT")
	dropIdx := indexOfRule(open, "-d 192.168.5.2 -j DROP")
	if udpIdx < 0 || tcpIdx < 0 {
		t.Fatalf("open posture must ACCEPT each DNS forward target:\n%s", strings.Join(open, "\n"))
	}
	if dropIdx < 0 || udpIdx > dropIdx || tcpIdx > dropIdx {
		t.Fatalf("DNS forward ACCEPTs (udp=%d tcp=%d) must precede the alias DROP (%d)", udpIdx, tcpIdx, dropIdx)
	}
	closed := familyArgs(BuildRulesDNS("192.168.5.2", "", []int{8443}, true, dns), IPv4)
	if indexOfRule(closed, "--dport 41234") >= 0 || indexOfRule(closed, "--dport 41235") >= 0 {
		t.Fatalf("closed posture must NOT open the DNS forward targets:\n%s", strings.Join(closed, "\n"))
	}
	// No targets → byte-identical to BuildRules.
	a := BuildRules("192.168.5.2", "fd07::fe", []int{8443}, false)
	b := BuildRulesDNS("192.168.5.2", "fd07::fe", []int{8443}, false, nil)
	if len(a) != len(b) {
		t.Fatalf("BuildRulesDNS with no targets must equal BuildRules: %d vs %d rules", len(a), len(b))
	}
	for i := range a {
		if a[i].Family != b[i].Family || !slices.Equal(a[i].Args, b[i].Args) {
			t.Fatalf("rule %d differs: %v vs %v", i, a[i], b[i])
		}
	}
}
