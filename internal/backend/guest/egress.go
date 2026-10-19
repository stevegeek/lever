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
// from the live chain and does NOT touch it. resolve returns the (v4, v6)
// host-alias addresses as seen from the guest. dns (nil = none) returns the
// guest resolver's DNAT targets on the alias to ACCEPT in the OPEN posture; it
// is never called in the closed posture, where DNS stays dropped by design.
//
// # The swap is atomic
//
// The new ruleset replaces the old one in ONE kernel commit per address family
// (iptables-restore --noflush), never by flushing and appending: in the open
// posture an empty chain is OUTPUT's default ACCEPT, so every running agent
// could reach the host alias on any port (the broker's unauthenticated admin
// port, the remote proxy) and the private ranges for as long as the window
// lasted. Each family's old chain stays in force until its commit, and a
// failed commit leaves it in force.
//
// So the alias is resolved WITH the old chain in force. When resolve fails
// under a CLOSED live IPv4 chain, the alias is read back from the live chains
// (liveAliases) with a warning. Otherwise ApplyEgress fails, leaving the live
// chain untouched: failing closed, never reopening to find out.
//
// The rules are not persisted in the guest: after a guest reboot there is no
// LEVER_EGRESS chain until the next apply or up, which always applies egress
// before it starts agents.
//
// rebuilt reports whether a new ruleset was committed. On the I2 skip path v6
// is always "" (only the v4 alias is parsed from the live chain); callers MUST
// NOT treat it as authoritative. Only trust v6 when rebuilt is true.
func (g Guest) ApplyEgress(ctx context.Context, resolve func(context.Context) (v4, v6 string, err error), dns func(ctx context.Context, aliasV4 string) ([]egress.DNSForward, error), allowedPorts []int, closedInternet bool) (v4, v6 string, rebuilt bool, err error) {
	// First, so the skip below can never leave a closed chain that OUTPUT no
	// longer jumps to. It changes nothing that is enforced.
	if err := g.ensureEgressChain(ctx); err != nil {
		return "", "", false, err
	}
	// I2: leave a closed chain alone when both families are already, rule for
	// rule, the closed ruleset for the alias they name and these ports.
	// Anything else is rebuilt below; the commit is atomic, so a rebuild
	// opens no window.
	if closedInternet {
		if alias, ok := g.existingClosedAlias(ctx, allowedPorts); ok {
			return alias, "", false, nil
		}
	}
	v4, v6, err = resolve(ctx)
	if err != nil {
		// Fall back to the aliases the live chains name ONLY when the IPv4
		// chain is closed: its DROP is then the expected reason the lookup
		// failed. Otherwise nothing lever wrote blocks DNS, so a failed lookup
		// is a real fault, and an old address would hide it: fail, leaving
		// the chains alone.
		lv4, lv6, closed4 := g.liveAliases(ctx)
		if !closed4 || (lv4 == "" && lv6 == "") {
			return "", "", false, fmt.Errorf("%w (the live %s chains are left as they are; the alias is read back from them only when the IPv4 chain is closed and names one)", err, egress.Chain)
		}
		msg := fmt.Sprintf("egress: could not resolve the host alias (%v) — the live closed IPv4 %s chain blocks DNS; "+
			"using the alias it names instead: v4 %q, v6 %q", err, egress.Chain, lv4, lv6)
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
	// IPv6 first, IPv4 last. The pair is not atomic; committing v4 last means
	// a v4 closed marker is only written after v6 is final, so the I2 skip
	// never mistakes a half-applied closed posture for a finished one.
	if err := g.commitEgressChain(ctx, egress.IPv6, rules); err != nil {
		return "", "", false, fmt.Errorf("%w — nothing was committed: IPv4 and IPv6 both keep their previous %s chains", err, egress.Chain)
	}
	if err := g.commitEgressChain(ctx, egress.IPv4, rules); err != nil {
		return "", "", false, fmt.Errorf("%w — the new IPv6 %s chain IS committed; IPv4 keeps its previous chain until the next apply", err, egress.Chain)
	}
	return v4, v6, true, nil
}

// commitEgressChain replaces LEVER_EGRESS for one family in a single commit:
// declaring the chain empties it in the same transaction, so all rules apply
// or none. A failure leaves that family's previous chain in force.
func (g Guest) commitEgressChain(ctx context.Context, fam egress.Family, rules []egress.Rule) error {
	in := egress.RestoreInput(rules, fam)
	if err := g.pipeInto(ctx, g.RootPrefix, strings.NewReader(in), "exec "+fam.RestoreBinary()+" --noflush"); err != nil {
		return fmt.Errorf("egress: commit %s (%s) failed, so that family's previous chain stays in force: %w", egress.Chain, fam.RestoreBinary(), err)
	}
	return nil
}

// liveAliases reads the host-alias addresses back out of the live chains'
// per-port ACCEPTs and reports whether the IPv4 chain is closed.
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

func closedChain(listing string) bool {
	return strings.Contains(listing, "-A "+egress.Chain+" -j DROP")
}

// aliasOfFamily counts an address of the wrong family as none: it would be a
// rule that never loads.
func aliasOfFamily(listing string, wantV4 bool) string {
	alias := aliasFromChain(listing)
	if ip := net.ParseIP(alias); ip == nil || (ip.To4() != nil) != wantV4 {
		return ""
	}
	return alias
}

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
// aliases the chains name and allowedPorts (I2). Anything absent, open,
// unreadable or different returns false. Both families are required so a
// closed apply that failed between its commits is finished, not skipped.
// The aliases come from the chains, so no DNS is needed.
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

// ensureEgressChain makes sure the LEVER_EGRESS chain exists and that the
// jump to it is OUTPUT's FIRST rule, exactly once, for both families,
// WITHOUT flushing the chain.
//
// First, not merely present: a rule ahead of the jump would decide packets
// before the lever chain sees them. The repair deletes every copy and
// inserts one at position 1 in ONE commit, so OUTPUT is never without it.
// This is the state Snapshot.VerifyEgress requires.
func (g Guest) ensureEgressChain(ctx context.Context) error {
	for _, fam := range []egress.Family{egress.IPv4, egress.IPv6} {
		bin := fam.Binary()
		// -N errors if the chain already exists.
		_, _ = g.RootRun(ctx, bin, "-N", egress.Chain)
		res, err := g.RootRun(ctx, bin, "-S", "OUTPUT")
		if err != nil {
			return fmt.Errorf("egress: list OUTPUT (%s): %w", bin, err)
		}
		if egress.JumpFirst(res.Stdout) {
			continue
		}
		in := egress.JumpRestoreInput(res.Stdout)
		if err := g.pipeInto(ctx, g.RootPrefix, strings.NewReader(in), "exec "+fam.RestoreBinary()+" --noflush"); err != nil {
			return fmt.Errorf("egress: make the %s jump OUTPUT's first rule (%s): %w", egress.Chain, fam.RestoreBinary(), err)
		}
		// Read it back, so a listing this parse misread fails here rather
		// than at every later warm check.
		after, err := g.RootRun(ctx, bin, "-S", "OUTPUT")
		if err != nil {
			return fmt.Errorf("egress: list OUTPUT (%s): %w", bin, err)
		}
		if !egress.JumpFirst(after.Stdout) {
			return fmt.Errorf("egress: after the repair, the %s jump is still not OUTPUT's first and only rule (%s); OUTPUT lists:\n%s", egress.Chain, bin, safeListing(after.Stdout))
		}
	}
	return nil
}

// safeListing is a guest rule listing fit to quote in an error: the guest
// writes it, and it can be as long as the output cap.
func safeListing(out string) string {
	out = strings.TrimSpace(out)
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		lines = append(lines, SafeLine(l, reportLineMax))
		if len(lines) > reportLinesMax+1 {
			break
		}
	}
	n := strings.Count(out, "\n") + 1
	if len(lines) > reportLinesMax {
		lines = append(lines[:reportLinesMax], fmt.Sprintf("and %d more guest rules not shown", n-reportLinesMax))
	}
	return strings.Join(lines, "\n")
}
