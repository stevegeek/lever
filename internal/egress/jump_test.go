package egress

import (
	"os/exec"
	"strings"
	"testing"
)

func TestJumpFirst(t *testing.T) {
	for listing, want := range map[string]bool{
		"-P OUTPUT ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n":                                    true,
		"-P OUTPUT DROP\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -j ACCEPT\n":                 true,
		"-P OUTPUT ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -o eth0 -j LEVER_EGRESS\n": true,
		"":                   false,
		"-P OUTPUT ACCEPT\n": false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -j ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n":       false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -j RETURN\n-A OUTPUT -j LEVER_EGRESS\n":       false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -g OTHER\n-A OUTPUT -j LEVER_EGRESS\n":        false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -o lo -j LEVER_EGRESS\n":                      false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -j LEVER_EGRESS\n": false,
		"-P OUTPUT ACCEPT\n-A OUTPUT -j LEVER_EGRESS -m comment --comment x\n":     false,
		"-N LEVER_EGRESS\n-A LEVER_EGRESS -j DROP\n":                               false,
	} {
		if got := JumpFirst(listing); got != want {
			t.Errorf("JumpFirst(%q) = %v, want %v", listing, got, want)
		}
	}
}

func TestJumpRestoreInput(t *testing.T) {
	got := JumpRestoreInput("-P OUTPUT ACCEPT\n-A OUTPUT -j ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -o lo -j LEVER_EGRESS\n-A OUTPUT -j LEVER_EGRESS\n")
	want := "*filter\n-D OUTPUT -j LEVER_EGRESS\n-D OUTPUT -j LEVER_EGRESS\n-I OUTPUT 1 -j LEVER_EGRESS\nCOMMIT\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := JumpRestoreInput("-P OUTPUT ACCEPT\n"); got != "*filter\n-I OUTPUT 1 -j LEVER_EGRESS\nCOMMIT\n" {
		t.Fatalf("no jump yet: got\n%s", got)
	}
}

// TestJumpRestoreAgainstRealIptables builds an OUTPUT that bypasses the lever
// chain (an ACCEPT ahead of the jump, a second jump) in a throwaway user and
// network namespace, and checks that JumpRestoreInput makes the jump first
// and unique, keeps the other rules' order, is idempotent, and that input
// from a stale listing is refused whole. Skips where unprivileged namespaces
// or iptables are unavailable.
func TestJumpRestoreAgainstRealIptables(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	for _, fam := range []Family{IPv4, IPv6} {
		bin, rbin := fam.Binary(), fam.RestoreBinary()
		setup := bin + " -N " + Chain + " && " + bin + " -N OTHER && " +
			bin + " -A OUTPUT -j ACCEPT && " + bin + " -A OUTPUT -j " + Chain + " && " +
			bin + " -A OUTPUT -o lo -j " + Chain + " && " + bin + " -A OUTPUT -g OTHER && " +
			bin + " -A OUTPUT -j " + Chain
		before := "-P OUTPUT ACCEPT\n-A OUTPUT -j ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -o lo -j LEVER_EGRESS\n-A OUTPUT -g OTHER\n-A OUTPUT -j LEVER_EGRESS\n"
		in := JumpRestoreInput(before)
		stale := strings.Replace(in, "*filter\n", "*filter\n-D OUTPUT -j "+Chain+"\n", 1)
		script := setup + ` && ` + bin + ` -S OUTPUT && echo @@ &&
{ printf '%s' "$2" | ` + rbin + ` --noflush 2>/dev/null && echo stale-applied || echo stale-refused; } && ` + bin + ` -S OUTPUT && echo @@ &&
printf '%s' "$1" | ` + rbin + ` --noflush && ` + bin + ` -S OUTPUT`
		out, err := exec.Command("unshare", "-rn", "sh", "-c", script, "_", in, stale).CombinedOutput()
		if err != nil {
			t.Skipf("%s in an unprivileged namespace is unavailable here: %v: %s", bin, err, out)
		}
		parts := strings.Split(string(out), "@@\n")
		if len(parts) != 3 {
			t.Fatalf("%s: output:\n%s", bin, out)
		}
		if parts[0] != before {
			t.Fatalf("%s: the setup lists\n%s\nnot\n%s", bin, parts[0], before)
		}
		if !strings.HasPrefix(parts[1], "stale-refused\n") || strings.TrimPrefix(parts[1], "stale-refused\n") != before {
			t.Fatalf("%s: a stale input must be refused as a whole, leaving OUTPUT as it was:\n%s", bin, parts[1])
		}
		after := parts[2]
		want := "-P OUTPUT ACCEPT\n-A OUTPUT -j LEVER_EGRESS\n-A OUTPUT -j ACCEPT\n-A OUTPUT -o lo -j LEVER_EGRESS\n-A OUTPUT -g OTHER\n"
		if after != want {
			t.Fatalf("%s: after the commit\n%s\nwant\n%s", bin, after, want)
		}
		if !JumpFirst(after) {
			t.Fatalf("%s: JumpFirst refuses what the commit left:\n%s", bin, after)
		}
	}
}
