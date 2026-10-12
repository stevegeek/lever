package guest

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/stevegeek/lever/internal/daemon"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/progress"
)

// ApplyEgress applies the LEVER_EGRESS ruleset inside the guest via RootPrefix,
// preserving the I2 no-reopen property: if closedInternet and the closed chain
// is already live and exactly the closed ruleset, it returns the alias parsed
// from the live chain and does NOT touch it. resolve returns the (v4, v6) host-alias addresses as seen from the
// guest. dns (nil = none) returns the guest resolver's DNAT targets on the alias
// (egress.DNSForward, lever#34) to ACCEPT in the OPEN posture; it is never
// called in the closed posture, where DNS stays dropped by design.
//
// # The swap is atomic
//
// The new ruleset replaces the old one in ONE kernel commit per address family
// (commitEgressChain: iptables-restore --noflush), never by flushing the chain
// and appending rule by rule. The old way left the chain empty, or holding
// only its first ACCEPTs, for the whole resolve+append window — one DNS
// lookup plus a dozen guest round trips, seconds on Lima, where each is a
// `limactl shell` — and in the open posture an empty chain is OUTPUT's
// default ACCEPT: every running agent could reach the host alias on any port
// (the broker's unauthenticated admin port, the remote proxy) and the private
// ranges (a private remote.bind). An apply that failed or was interrupted in
// that window left it that way until the next good apply. Now each family's
// old chain stays in force until its commit, and a failed commit leaves it in
// force. The two commits are ordered (IPv6, then IPv4) and the I2 skip needs
// both families closed; see the commit site.
//
// That is also why the alias is resolved WITH the old chain in force. The old
// code flushed first so that a closed chain's catch-all DROP would not block
// the DNS lookup; here, when resolve fails under a CLOSED live IPv4 chain,
// the alias is read back from the live chains instead (liveAliases), with a warning — every
// chain lever writes names it in its per-port ACCEPTs. Otherwise ApplyEgress
// fails, leaving the live chain untouched: failing closed, never reopening
// to find out.
//
// What remains: the rules are not persisted in the guest. After a guest
// reboot there is no LEVER_EGRESS chain until the next `lever apply`/`up`
// (pre-existing; lever always applies egress before it starts agents).
//
// rebuilt reports whether a new ruleset was committed. On the I2 skip path
// (rebuilt=false) v6 is always "" — existingClosedAlias parses only the v4
// ACCEPT rule from the live chain, so it cannot recover v6 — and callers MUST
// NOT treat that empty v6 as authoritative (e.g. must not use it to overwrite
// a previously-resolved v6 alias). Only trust v6 when rebuilt is true.
func (g Guest) ApplyEgress(ctx context.Context, resolve func(context.Context) (v4, v6 string, err error), dns func(ctx context.Context, aliasV4 string) ([]egress.DNSForward, error), allowedPorts []int, closedInternet bool) (v4, v6 string, rebuilt bool, err error) {
	// The chain must exist and OUTPUT must jump to it. Neither step changes
	// what is enforced: an existing chain is left as it is, and a new one is
	// empty until the commit fills it. First, so that the skip below can never
	// leave a closed chain that OUTPUT no longer jumps to.
	if err := g.ensureEgressChain(ctx); err != nil {
		return "", "", false, err
	}
	// I2 — leave a running closed instance's chain alone when it is already
	// exactly right. If the closed posture is ALREADY active in both families,
	// and the live chains are, rule for rule, the closed ruleset for the
	// alias they name and these ports (existingClosedAlias), there is nothing
	// to commit, and the skip spares a closed chain the DNS lookup its DROP
	// would block. Anything else (a port added to manager.allow_ports or the
	// login port, a rule missing or added by hand, a half-applied commit) is
	// rebuilt below: the commit is atomic, so a rebuild opens no window, and
	// a failed lookup under the closed chain falls back to the alias it names.
	if closedInternet {
		if alias, ok := g.existingClosedAlias(ctx, allowedPorts); ok {
			return alias, "", false, nil
		}
	}
	v4, v6, err = resolve(ctx)
	if err != nil {
		// Fall back to the aliases the live chains name ONLY when the IPv4
		// chain is closed: the guest resolves over IPv4, so its catch-all
		// DROP is then the expected reason the lookup failed (DNS is dropped
		// by design), and the aliases are the ones lever resolved when it
		// wrote them. A closed IPv6 chain alone (a closed apply whose IPv4
		// commit failed) blocks no IPv4 DNS, and under an open or absent
		// chain nothing lever wrote blocks DNS: a failed lookup is then a
		// real fault (guest DNS broken, alias gone), and papering over it
		// with an old address would hide it — fail instead, leaving the
		// chains alone.
		lv4, lv6, closed4 := g.liveAliases(ctx)
		if !closed4 || (lv4 == "" && lv6 == "") {
			return "", "", false, fmt.Errorf("%w (the live %s chains are left as they are; the alias is read back from them only when the IPv4 chain is closed and names one)", err, egress.Chain)
		}
		msg := fmt.Sprintf("egress: could not resolve the host alias (%v) — the live closed IPv4 %s chain blocks DNS; "+
			"using the alias it names instead: v4 %q, v6 %q", err, egress.Chain, lv4, lv6)
		// Above the progress list's live line when a bring-up shows one.
		if rep := progress.From(ctx); rep != nil {
			rep.Log("jail: warning: " + msg)
		} else {
			daemon.Warnf("%s", msg)
		}
		v4, v6 = lv4, lv6
	}
	var forwards []egress.DNSForward
	if !closedInternet && dns != nil && v4 != "" {
		if forwards, err = dns(ctx, v4); err != nil {
			return "", "", false, fmt.Errorf("egress: resolver forward targets: %w", err)
		}
	}
	rules := egress.BuildRulesDNS(v4, v6, allowedPorts, closedInternet, forwards)
	// IPv6 first, IPv4 last. Each commit is atomic, but the pair is not: a
	// failure or an interrupt between them leaves one family new and one
	// old. The I2 skip reads the closed marker from BOTH families
	// (existingClosedAlias), and committing v4 last means a v4 closed marker
	// is only ever written after v6 is final — so a half-applied closed
	// posture is never mistaken for a finished one and skipped forever.
	if err := g.commitEgressChain(ctx, egress.IPv6, rules); err != nil {
		return "", "", false, fmt.Errorf("%w — nothing was committed: IPv4 and IPv6 both keep their previous %s chains", err, egress.Chain)
	}
	if err := g.commitEgressChain(ctx, egress.IPv4, rules); err != nil {
		return "", "", false, fmt.Errorf("%w — the new IPv6 %s chain IS committed; IPv4 keeps its previous chain until the next apply", err, egress.Chain)
	}
	return v4, v6, true, nil
}

// commitEgressChain replaces LEVER_EGRESS for one family in a single commit:
// `iptables-restore --noflush` with the chain declared (which empties an
// existing user chain in the same transaction) and every rule appended, all
// applied or none. --noflush leaves every other chain and table as it is.
// A failure leaves that FAMILY's previous chain in force; the caller says
// what happened to the other one.
func (g Guest) commitEgressChain(ctx context.Context, fam egress.Family, rules []egress.Rule) error {
	in := egress.RestoreInput(rules, fam)
	if err := g.pipeInto(ctx, g.RootPrefix, strings.NewReader(in), "exec "+fam.RestoreBinary()+" --noflush"); err != nil {
		return fmt.Errorf("egress: commit %s (%s) failed, so that family's previous chain stays in force: %w", egress.Chain, fam.RestoreBinary(), err)
	}
	return nil
}

// liveAliases reads the host-alias addresses back out of the live chains'
// per-port ACCEPTs (`-d <alias>/32|/128 … --dport … -j ACCEPT`), for when
// resolve cannot run under them, and reports whether the IPv4 chain is
// closed (carries the catch-all DROP). Each alias is "" when that family
// has no chain or names none of its own family.
func (g Guest) liveAliases(ctx context.Context) (v4, v6 string, closed4 bool) {
	read := func(bin string, wantV4 bool) string {
		res, err := g.RootRun(ctx, bin, "-S", egress.Chain)
		if err != nil {
			return ""
		}
		out := res.Stdout + res.Stderr
		if wantV4 && closedChain(out) {
			closed4 = true
		}
		return aliasOfFamily(out, wantV4)
	}
	v4 = read("iptables", true)
	v6 = read("ip6tables", false)
	return v4, v6, closed4
}

// closedChain reports whether an `iptables -S` listing of the chain carries
// the closed posture's catch-all DROP.
func closedChain(listing string) bool {
	return strings.Contains(listing, "-A "+egress.Chain+" -j DROP")
}

// aliasOfFamily is aliasFromChain restricted to the chain's own family: a
// v4 address in the ip6tables ruleset (or the reverse) would be a rule that
// never loads, so it counts as none.
func aliasOfFamily(listing string, wantV4 bool) string {
	alias := aliasFromChain(listing)
	if ip := net.ParseIP(alias); ip == nil || (ip.To4() != nil) != wantV4 {
		return ""
	}
	return alias
}

// aliasFromChain returns the destination of the first per-port ACCEPT in an
// `iptables -S` listing, without its prefix length, or "".
func aliasFromChain(listing string) string {
	for _, line := range strings.Split(listing, "\n") {
		if !strings.Contains(line, "--dport") || !strings.Contains(line, "-j ACCEPT") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-d" && i+1 < len(fields) {
				ip, _, _ := strings.Cut(fields[i+1], "/")
				if net.ParseIP(ip) != nil {
					return ip
				}
			}
		}
	}
	return ""
}

// existingClosedAlias returns the v4 host alias of a LEVER_EGRESS posture
// that is closed in BOTH families and is exactly the closed ruleset for the
// aliases the chains name and allowedPorts, so ApplyEgress can leave it
// untouched (I2). Returns ("", false) when either chain is absent, open (no
// catch-all DROP), unreadable, or differs from that ruleset in any rule —
// the caller then (re)builds both normally. Requiring both families matters:
// a closed apply that committed one family and failed before the other must
// be finished by the next apply, not skipped as done (ApplyEgress also
// commits v4 last, so v4 alone never looks closed first). The aliases are
// read from the per-port ACCEPT rules (`-d <ip>/32 … --dport … -j ACCEPT`),
// so no DNS is needed (which the active DROP blocks anyway). The comparison
// reads stdout only: iptables -S prints the chain there.
func (g Guest) existingClosedAlias(ctx context.Context, allowedPorts []int) (string, bool) {
	res, err := g.RootRun(ctx, "iptables", "-S", egress.Chain)
	if err != nil || !closedChain(res.Stdout) {
		return "", false // chain absent, unreadable, or not closed
	}
	res6, err := g.RootRun(ctx, "ip6tables", "-S", egress.Chain)
	if err != nil || !closedChain(res6.Stdout) {
		return "", false // IPv6 not closed (yet): rebuild both
	}
	v4 := aliasOfFamily(res.Stdout, true)
	if v4 == "" {
		return "", false // closed but no parseable alias — rebuild to be safe
	}
	rules := egress.BuildRulesDNS(v4, aliasOfFamily(res6.Stdout, false), allowedPorts, true, nil)
	if !egress.ListingMatches(rules, egress.IPv4, res.Stdout) || !egress.ListingMatches(rules, egress.IPv6, res6.Stdout) {
		return "", false // not exactly the closed ruleset: rebuild
	}
	return v4, true
}

// ensureEgressChain makes sure the LEVER_EGRESS chain exists and OUTPUT jumps
// to it exactly once, for both address families, WITHOUT flushing it: the
// live rules stay in force until commitEgressChain replaces them atomically.
// A freshly created chain is empty, which enforces nothing more than the
// absence of the chain did.
func (g Guest) ensureEgressChain(ctx context.Context) error {
	for _, bin := range []string{"iptables", "ip6tables"} {
		run := func(args ...string) error {
			_, err := g.RootRun(ctx, append([]string{bin}, args...)...)
			return err
		}
		// Create the chain if absent (-N errors if it already exists — tolerate).
		_ = run("-N", egress.Chain)
		// Ensure the OUTPUT jump exists exactly once: -C checks, -A adds if missing.
		if err := run("-C", "OUTPUT", "-j", egress.Chain); err != nil {
			if err := run("-A", "OUTPUT", "-j", egress.Chain); err != nil {
				return fmt.Errorf("egress: add OUTPUT jump (%s): %w", bin, err)
			}
		}
	}
	return nil
}
