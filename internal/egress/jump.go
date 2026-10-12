package egress

import "strings"

// The OUTPUT jump. LEVER_EGRESS enforces nothing unless OUTPUT sends every
// packet into it, and a jump that is merely present is not enough: a rule
// placed in OUTPUT BEFORE it (an ACCEPT, a RETURN, a goto to another chain)
// decides the packet before it ever reaches the lever chain. So the jump
// must be OUTPUT's first rule, in both families. Rules after it cannot
// weaken it (a packet the lever chain DROPs or REJECTs never reaches them,
// and one it lets through was allowed anyway), so they are left as they are:
// lever owns its chain and the head of OUTPUT, nothing else.

// JumpRule is the exact OUTPUT rule, as `iptables -S OUTPUT` prints it, that
// sends every packet into the lever chain.
const JumpRule = "-A OUTPUT -j " + Chain

// JumpFirst reports whether listing, the output of `iptables -S OUTPUT` (or
// ip6tables), has JumpRule as OUTPUT's first rule and nowhere else. The
// policy line (-P) is not a rule. A listing that cannot be read this way
// (empty, another chain) is false: the caller then (re)places the jump.
func JumpFirst(listing string) bool {
	first := true
	ok := false
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		if !strings.HasPrefix(line, "-A OUTPUT ") {
			return false // not an OUTPUT listing
		}
		switch {
		case first:
			ok = line == JumpRule
		case line == JumpRule:
			return false // a second jump: the full path leaves exactly one
		}
		first = false
	}
	return ok
}

// JumpRestoreInput is the `iptables-restore --noflush` input that makes
// JumpRule OUTPUT's first and only copy, given the live OUTPUT listing: it
// deletes each existing copy of the rule and inserts it at position 1, in
// ONE commit. One commit matters: deleting first and inserting in a second
// command would leave OUTPUT without the jump in between, and the lever
// chain bypassed for every packet sent in that window. If the listing went
// stale (a copy was removed meanwhile) a delete fails and the whole commit
// is refused, leaving OUTPUT as it was. Other OUTPUT rules are left alone.
func JumpRestoreInput(listing string) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, line := range strings.Split(listing, "\n") {
		if strings.TrimSpace(line) == JumpRule {
			b.WriteString("-D OUTPUT -j " + Chain + "\n")
		}
	}
	b.WriteString("-I OUTPUT 1 -j " + Chain + "\nCOMMIT\n")
	return b.String()
}
