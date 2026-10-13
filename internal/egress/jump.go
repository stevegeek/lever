package egress

import "strings"

// The OUTPUT jump. LEVER_EGRESS enforces nothing unless OUTPUT sends every
// packet into it, and a rule placed before the jump (an ACCEPT, a RETURN, a
// goto) decides the packet first. So the jump must be OUTPUT's first rule in
// both families. Rules after it cannot weaken it, so they are left alone:
// lever owns its chain and the head of OUTPUT, nothing else.

// JumpRule is the exact OUTPUT rule, as `iptables -S OUTPUT` prints it, that
// sends every packet into the lever chain.
const JumpRule = "-A OUTPUT -j " + Chain

// JumpFirst reports whether listing (`iptables -S OUTPUT`) has JumpRule as
// OUTPUT's first rule and nowhere else. An unreadable listing is false: the
// caller then places the jump.
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

// JumpRestoreInput is the `iptables-restore --noflush` input that deletes
// each existing copy of JumpRule and inserts it at position 1 in one commit.
// Two commands would leave OUTPUT without the jump in between, bypassing the
// lever chain. A stale listing makes a delete fail and the whole commit is
// refused, leaving OUTPUT as it was.
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
