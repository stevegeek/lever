package guest

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/exec"
)

// rootRun executes args inside the guest as root via RootPrefix, e.g.
// RootPrefix ["orb","-u","root","-m",m] + args ["iptables","-S",chain] runs
// `orb -u root -m <m> iptables -S <chain>`. Defensively copies RootPrefix[1:]
// before appending so concurrent callers can't alias/corrupt each other's argv
// (append may reuse the underlying array when capacity allows).
func (g Guest) rootRun(ctx context.Context, args ...string) (exec.Result, error) {
	argv := append(append([]string{}, g.RootPrefix[1:]...), args...)
	return g.Host.Run(ctx, nil, g.RootPrefix[0], argv...)
}

// ApplyEgress applies the LEVER_EGRESS ruleset inside the guest via RootPrefix,
// preserving the I2 no-reopen property: if closedInternet and the closed chain
// is already live, it returns the alias parsed from the live chain and does NOT
// flush. resolve returns the (v4, v6) host-alias addresses as seen from the
// guest; it is only called when a rebuild happens (DNS is available then).
//
// rebuilt reports whether a full reset+resolve+apply ran. On the I2 skip path
// (rebuilt=false) v6 is always "" — existingClosedAlias parses only the v4
// ACCEPT rule from the live chain, so it cannot recover v6 — and callers MUST
// NOT treat that empty v6 as authoritative (e.g. must not use it to overwrite
// a previously-resolved v6 alias). Only trust v6 when rebuilt is true.
func (g Guest) ApplyEgress(ctx context.Context, resolve func(context.Context) (v4, v6 string, err error), allowedPorts []int, closedInternet bool) (v4, v6 string, rebuilt bool, err error) {
	// I2 — never briefly open egress under a running closed instance. If the closed
	// posture is ALREADY active (LEVER_EGRESS has the catch-all DROP), a running
	// jailed agent depends on it; flushing+rebuilding would leave the chain empty
	// (→ OUTPUT default ACCEPT) for the resolve+rebuild window. The ruleset is a
	// pure function of (alias, ports, closed) and is unchanged on a normal
	// re-apply, so leave the working chain intact and skip the rebuild. (A fresh
	// machine, the open/subscription posture, or a genuine egress-config change all
	// have no active DROP and fall through to a full rebuild — and a fresh apply has
	// no running container to leak. Changing egress config on a live closed instance
	// requires `lever down` + `up`, not a re-apply.)
	if closedInternet {
		if alias, ok := g.existingClosedAlias(ctx); ok {
			return alias, "", false, nil
		}
	}
	// Reset the dedicated chain FIRST: this makes re-apply idempotent (no rule
	// accumulation) and — critically — flushing the chain removes any prior
	// catch-all DROP, restoring DNS so the host-alias re-resolve below works even
	// when a previous closed-egress posture is still in place.
	if err := g.resetEgressChain(ctx); err != nil {
		return "", "", false, err
	}
	v4, v6, err = resolve(ctx)
	if err != nil {
		return "", "", false, err
	}
	for _, rule := range egress.BuildRules(v4, v6, allowedPorts, closedInternet) {
		if _, err := g.rootRun(ctx, append([]string{rule.Family.Binary()}, rule.Args...)...); err != nil {
			return "", "", false, fmt.Errorf("apply %s: %w", rule.Render(), err)
		}
	}
	return v4, v6, true, nil
}

// existingClosedAlias returns the v4 host alias already encoded in an ACTIVE
// closed LEVER_EGRESS chain (i.e. the chain contains the catch-all DROP), so
// ApplyEgress can skip the flush+rebuild that would briefly open egress (I2).
// Returns ("", false) when the chain is absent, open (no catch-all DROP), or
// unreadable — the caller then (re)builds the chain normally. The alias is read
// from the per-port ACCEPT rule (`-d <ip>/32 … --dport … -j ACCEPT`), so we never
// need DNS (which the active DROP blocks anyway).
func (g Guest) existingClosedAlias(ctx context.Context) (string, bool) {
	res, err := g.rootRun(ctx, "iptables", "-S", egress.Chain)
	if err != nil {
		return "", false // chain absent (fresh machine) or unreadable
	}
	out := res.Stdout + res.Stderr
	if !strings.Contains(out, "-A "+egress.Chain+" -j DROP") {
		return "", false // not in the closed posture (no catch-all DROP)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "--dport") || !strings.Contains(line, "-j ACCEPT") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-d" && i+1 < len(fields) {
				ip := strings.TrimSuffix(fields[i+1], "/32")
				if net.ParseIP(ip) != nil {
					return ip, true
				}
			}
		}
	}
	return "", false // closed but no parseable alias — rebuild to be safe
}

// resetEgressChain ensures the LEVER_EGRESS chain exists and OUTPUT jumps to it
// exactly once, then flushes it, for both address families. Idempotent: the chain
// holds ALL of lever's egress rules, so flushing wipes the prior apply's rules
// (no accumulation) without touching any non-lever OUTPUT rules, and removes the
// catch-all DROP so DNS works for the subsequent host-alias resolve.
func (g Guest) resetEgressChain(ctx context.Context) error {
	for _, bin := range []string{"iptables", "ip6tables"} {
		run := func(args ...string) error {
			_, err := g.rootRun(ctx, append([]string{bin}, args...)...)
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
		// Flush our chain (idempotent re-apply; restores DNS for the re-resolve).
		if err := run("-F", egress.Chain); err != nil {
			return fmt.Errorf("egress: flush %s (%s): %w", egress.Chain, bin, err)
		}
	}
	return nil
}
