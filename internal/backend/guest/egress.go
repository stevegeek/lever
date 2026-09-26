package guest

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/stevegeek/lever/internal/egress"
)

// ApplyEgress applies the LEVER_EGRESS ruleset inside the guest via RootPrefix,
// preserving the I2 no-reopen property: if closedInternet and the closed chain
// is already live, it returns the alias parsed from the live chain and does NOT
// touch it. resolve returns the (v4, v6) host-alias addresses as seen from the
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
// that window left it that way until the next good apply. Now the old chain
// stays in force until the commit, and a failed commit leaves it in force.
//
// That is also why the alias is resolved WITH the old chain in force. The old
// code flushed first so that a closed chain's catch-all DROP would not block
// the DNS lookup; here, when resolve fails, the alias is read back from the
// live chain instead (liveAliases) — every chain lever writes names it in its
// per-port ACCEPTs. Only when neither works does ApplyEgress fail, leaving the
// live chain untouched: failing closed, never reopening to find out.
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
	// I2 — never touch a running closed instance's chain. If the closed
	// posture is ALREADY active (LEVER_EGRESS has the catch-all DROP), a running
	// jailed agent depends on it. The ruleset is a pure function of (alias,
	// ports, closed) and is unchanged on a normal re-apply, so leave the
	// working chain intact and skip the rebuild. (The commit below is atomic
	// now, so a rebuild would no longer open a window; the skip stays because
	// it also spares a closed chain the DNS lookup its DROP would block.)
	//
	// NOTE what the skip does NOT do: it tests only that the chain is closed and
	// carries a parseable alias. It never compares the LIVE ruleset against the
	// desired one, so a genuine egress-config change — a new manager.allow_ports
	// entry, or the remote-access login port (config.App.EffectiveAllowedPorts) —
	// is NOT applied to a live closed instance. That needs `lever down` + `up`,
	// which is the documented requirement; it is not, as an earlier version of
	// this comment claimed, something the skip detects and falls through for.
	if closedInternet {
		if alias, ok := g.existingClosedAlias(ctx); ok {
			return alias, "", false, nil
		}
	}
	// The chain must exist and OUTPUT must jump to it. Neither step changes
	// what is enforced: an existing chain is left as it is, and a new one is
	// empty until the commit fills it.
	if err := g.ensureEgressChain(ctx); err != nil {
		return "", "", false, err
	}
	v4, v6, err = resolve(ctx)
	if err != nil {
		lv4, lv6 := g.liveAliases(ctx)
		if lv4 == "" && lv6 == "" {
			return "", "", false, fmt.Errorf("%w (and the live %s chain names no host alias to fall back to; it is left as it is)", err, egress.Chain)
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
	for _, fam := range []egress.Family{egress.IPv4, egress.IPv6} {
		if err := g.commitEgressChain(ctx, fam, rules); err != nil {
			return "", "", false, err
		}
	}
	return v4, v6, true, nil
}

// commitEgressChain replaces LEVER_EGRESS for one family in a single commit:
// `iptables-restore --noflush` with the chain declared (which empties an
// existing user chain in the same transaction) and every rule appended, all
// applied or none. --noflush leaves every other chain and table as it is.
// A failure leaves the previous chain in force.
func (g Guest) commitEgressChain(ctx context.Context, fam egress.Family, rules []egress.Rule) error {
	in := egress.RestoreInput(rules, fam)
	if err := g.pipeInto(ctx, g.RootPrefix, strings.NewReader(in), "exec "+fam.RestoreBinary()+" --noflush"); err != nil {
		return fmt.Errorf("egress: commit %s (%s); the previous chain stays in force: %w", egress.Chain, fam.RestoreBinary(), err)
	}
	return nil
}

// liveAliases reads the host-alias addresses back out of the live chain's
// per-port ACCEPTs (`-d <alias>/32|/128 … --dport … -j ACCEPT`), for when
// resolve cannot run under the live chain (a closed chain's DROP blocks DNS).
// Empty when there is no chain or it names no alias.
func (g Guest) liveAliases(ctx context.Context) (v4, v6 string) {
	read := func(bin string, wantV4 bool) string {
		res, err := g.RootRun(ctx, bin, "-S", egress.Chain)
		if err != nil {
			return ""
		}
		alias := aliasFromChain(res.Stdout + res.Stderr)
		// Only an address of the chain's own family: a v4 address in the
		// ip6tables ruleset (or the reverse) would be a rule that never loads.
		if ip := net.ParseIP(alias); ip == nil || (ip.To4() != nil) != wantV4 {
			return ""
		}
		return alias
	}
	return read("iptables", true), read("ip6tables", false)
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

// existingClosedAlias returns the v4 host alias already encoded in an ACTIVE
// closed LEVER_EGRESS chain (i.e. the chain contains the catch-all DROP), so
// ApplyEgress can leave it untouched (I2).
// Returns ("", false) when the chain is absent, open (no catch-all DROP), or
// unreadable — the caller then (re)builds the chain normally. The alias is read
// from the per-port ACCEPT rule (`-d <ip>/32 … --dport … -j ACCEPT`), so we never
// need DNS (which the active DROP blocks anyway).
func (g Guest) existingClosedAlias(ctx context.Context) (string, bool) {
	res, err := g.RootRun(ctx, "iptables", "-S", egress.Chain)
	if err != nil {
		return "", false // chain absent (fresh machine) or unreadable
	}
	out := res.Stdout + res.Stderr
	if !strings.Contains(out, "-A "+egress.Chain+" -j DROP") {
		return "", false // not in the closed posture (no catch-all DROP)
	}
	if alias := aliasFromChain(out); alias != "" {
		return alias, true
	}
	return "", false // closed but no parseable alias — rebuild to be safe
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
