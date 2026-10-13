package egress

import (
	"os/exec"
	"strings"
	"testing"
)

// listingFixture is what `iptables -S LEVER_EGRESS` printed (iptables 1.8.11,
// nf_tables) for the open ruleset (the lever#182 ICMP ACCEPT and metadata
// REJECT lines as TestListingAgainstRealIptables sees them printed) of BuildRulesDNS("192.168.5.2",
// "fd07::fe", {3305}, false, udp/53).
const listingFixture4 = `-N LEVER_EGRESS
-A LEVER_EGRESS -d 192.168.5.2/32 -p tcp -m tcp --dport 3305 -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -p udp -m udp --dport 53 -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -m conntrack --ctstate ESTABLISHED -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -j DROP
-A LEVER_EGRESS -o lo -p icmp -m icmp --icmp-type 3/10 -m conntrack --ctstate RELATED -j ACCEPT
-A LEVER_EGRESS -d 169.254.169.254/32 -j REJECT --reject-with icmp-host-prohibited
-A LEVER_EGRESS -d 10.0.0.0/8 -j DROP
-A LEVER_EGRESS -d 172.16.0.0/12 -j DROP
-A LEVER_EGRESS -d 192.168.0.0/16 -j DROP
-A LEVER_EGRESS -d 169.254.0.0/16 -j DROP
-A LEVER_EGRESS -d 100.64.0.0/10 -j DROP
`

const listingFixture6 = `-N LEVER_EGRESS
-A LEVER_EGRESS -d fd07::fe/128 -p tcp -m tcp --dport 3305 -j ACCEPT
-A LEVER_EGRESS -d fd07::fe/128 -m conntrack --ctstate ESTABLISHED -j ACCEPT
-A LEVER_EGRESS -d fd07::fe/128 -j DROP
-A LEVER_EGRESS -d fe80::/10 -j DROP
-A LEVER_EGRESS -d fc00::/7 -j DROP
`

// The closed ruleset BuildRulesDNS("192.168.5.2", "fd07::fe", {8443},
// true, nil), as iptables -S prints it, written out by hand.
const closedFixture4 = `-N LEVER_EGRESS
-A LEVER_EGRESS -o lo -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -p tcp -m tcp --dport 8443 -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -m conntrack --ctstate ESTABLISHED -j ACCEPT
-A LEVER_EGRESS -d 192.168.5.2/32 -j DROP
-A LEVER_EGRESS -o lo -p icmp -m icmp --icmp-type 3/10 -m conntrack --ctstate RELATED -j ACCEPT
-A LEVER_EGRESS -d 169.254.169.254/32 -j REJECT --reject-with icmp-host-prohibited
-A LEVER_EGRESS -d 10.0.0.0/8 -j DROP
-A LEVER_EGRESS -d 172.16.0.0/12 -j DROP
-A LEVER_EGRESS -d 192.168.0.0/16 -j DROP
-A LEVER_EGRESS -d 169.254.0.0/16 -j DROP
-A LEVER_EGRESS -d 100.64.0.0/10 -j DROP
-A LEVER_EGRESS -j DROP
`

const closedFixture6 = `-N LEVER_EGRESS
-A LEVER_EGRESS -o lo -j ACCEPT
-A LEVER_EGRESS -d fd07::fe/128 -p tcp -m tcp --dport 8443 -j ACCEPT
-A LEVER_EGRESS -d fd07::fe/128 -m conntrack --ctstate ESTABLISHED -j ACCEPT
-A LEVER_EGRESS -d fd07::fe/128 -j DROP
-A LEVER_EGRESS -d fe80::/10 -j DROP
-A LEVER_EGRESS -d fc00::/7 -j DROP
-A LEVER_EGRESS -j DROP
`

func TestListingMatchesClosedPrintedForm(t *testing.T) {
	rules := BuildRulesDNS("192.168.5.2", "fd07::fe", []int{8443}, true, nil)
	if !ListingMatches(rules, IPv4, closedFixture4) || !ListingMatches(rules, IPv6, closedFixture6) {
		t.Fatalf("closed listing does not match:\n%s\n%s", strings.Join(Listing(rules, IPv4), "\n"), strings.Join(Listing(rules, IPv6), "\n"))
	}
}

func fixtureRules() []Rule {
	return BuildRulesDNS("192.168.5.2", "fd07:0:0::00fe", []int{3305}, false, []DNSForward{{"udp", 53}})
}

func TestListingMatchesIptablesPrintedForm(t *testing.T) {
	rules := fixtureRules()
	if !ListingMatches(rules, IPv4, listingFixture4) {
		t.Errorf("v4 listing does not match:\nwant %q", Listing(rules, IPv4))
	}
	if !ListingMatches(rules, IPv6, listingFixture6) {
		t.Errorf("v6 listing does not match:\nwant %q", Listing(rules, IPv6))
	}
}

// Every way a live chain can differ from the ruleset must be a mismatch: a
// rule missing, added, reordered or changed, another alias, the chain absent.
func TestListingMatchesRejectsAnyDifference(t *testing.T) {
	rules := fixtureRules()
	lines := strings.Split(strings.TrimSpace(listingFixture4), "\n")
	cases := map[string]string{
		"empty (no chain)":   "",
		"chain only (empty)": "-N LEVER_EGRESS\n",
		"rule missing":       strings.Join(append(append([]string{}, lines[:4]...), lines[5:]...), "\n"),
		"rule added":         listingFixture4 + "-A LEVER_EGRESS -j ACCEPT\n",
		"accept prepended":   strings.Replace(listingFixture4, "-N LEVER_EGRESS\n", "-N LEVER_EGRESS\n-A LEVER_EGRESS -j ACCEPT\n", 1),
		"reordered":          strings.Join(append([]string{lines[0], lines[4], lines[1], lines[2], lines[3]}, lines[5:]...), "\n"),
		"other port":         strings.Replace(listingFixture4, "--dport 3305", "--dport 3306", 1),
		"other alias":        strings.ReplaceAll(listingFixture4, "192.168.5.2/32", "192.168.5.9/32"),
		"drop became accept": strings.Replace(listingFixture4, "-d 10.0.0.0/8 -j DROP", "-d 10.0.0.0/8 -j ACCEPT", 1),
		"error text":         "iptables: No chain/target/match by that name.\n",
		// The lever#182 pair: the ICMP ACCEPT widened, the REJECT opened.
		"icmp any type":        strings.Replace(listingFixture4, " -m icmp --icmp-type 3/10", "", 1),
		"icmp other type":      strings.Replace(listingFixture4, "--icmp-type 3/10", "--icmp-type 3/3", 1),
		"icmp named type":      strings.Replace(listingFixture4, "--icmp-type 3/10", "--icmp-type host-prohibited", 1),
		"icmp not lo":          strings.Replace(listingFixture4, "-o lo -p icmp", "-p icmp", 1),
		"icmp not related":     strings.Replace(listingFixture4, "--ctstate RELATED -j ACCEPT", "--ctstate RELATED,NEW -j ACCEPT", 1),
		"reject became accept": strings.Replace(listingFixture4, "-d 169.254.169.254/32 -j REJECT --reject-with icmp-host-prohibited", "-d 169.254.169.254/32 -j ACCEPT", 1),
		"reject wider":         strings.Replace(listingFixture4, "169.254.169.254/32 -j REJECT", "169.254.0.0/16 -j REJECT", 1),
	}
	for name, live := range cases {
		if ListingMatches(rules, IPv4, live) {
			t.Errorf("%s: matched, want a mismatch", name)
		}
	}
	// The closed posture's catch-all DROP is part of the comparison.
	closed := BuildRulesDNS("192.168.5.2", "", []int{3305}, true, nil)
	if ListingMatches(closed, IPv4, listingFixture4) {
		t.Error("an open chain matched the closed ruleset")
	}
}

// TestListingAgainstRealIptables loads RestoreInput into real iptables in a
// throwaway user and network namespace and compares the printed chain, so
// the rendering is pinned to what iptables really prints. It skips where
// unprivileged namespaces or iptables are unavailable.
func TestListingAgainstRealIptables(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	for _, closed := range []bool{false, true} {
		rules := BuildRulesDNS("192.168.5.2", "fd07:b51a:cc66:f0::fe", []int{3305, 8443}, closed, []DNSForward{{"udp", 41234}, {"tcp", 41235}})
		// The metadata REJECT shape, so a rule of that kind is pinned too.
		rules = append(rules,
			Rule{IPv4, out("-d", "169.254.169.254", "-j", "REJECT", "--reject-with", "icmp-host-prohibited")},
			Rule{IPv6, out("-d", "fd00::1", "-p", "udp", "--dport", "53", "-j", "ACCEPT")},
		)
		for _, fam := range []Family{IPv4, IPv6} {
			script := "printf '%s' \"$1\" | " + fam.RestoreBinary() + " --noflush && " + fam.Binary() + " -S " + Chain
			cmd := exec.Command("unshare", "-rn", "sh", "-c", script, "_", RestoreInput(rules, fam))
			outb, err := cmd.CombinedOutput()
			if err != nil {
				t.Skipf("iptables in an unprivileged namespace is unavailable here: %v: %s", err, outb)
			}
			if !ListingMatches(rules, fam, string(outb)) {
				t.Errorf("closed=%v %s: real listing\n%s\ndiffers from\n%s", closed, fam.Binary(), outb, strings.Join(Listing(rules, fam), "\n"))
			}
		}
	}
}
