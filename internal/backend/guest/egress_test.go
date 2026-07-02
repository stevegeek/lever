package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

// closedChainRunner returns an ACTIVE closed LEVER_EGRESS chain for `iptables -S`
// and records whether the chain was flushed or the alias re-resolved.
type closedChainRunner struct {
	*exec.FakeRunner
	flushed, resolved bool
}

func (r *closedChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	argv := strings.Join(args, " ")
	if name == "orb" {
		switch {
		case strings.Contains(argv, "iptables -S LEVER_EGRESS"):
			return exec.Result{Stdout: "-N LEVER_EGRESS\n-A LEVER_EGRESS -o lo -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -p tcp -m tcp --dport 8443 -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -j DROP\n-A LEVER_EGRESS -j DROP\n"}, nil
		case strings.Contains(argv, "-F LEVER_EGRESS"):
			r.flushed = true
		case strings.Contains(argv, "getent ahosts"):
			r.resolved = true
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *closedChainRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// orbGuest returns a Guest shaped like orbstack's, for argv-identical assertions.
func orbGuest(host exec.Runner, machine string) Guest {
	return Guest{
		Host:       host,
		UserPrefix: []string{"orb", "-m", machine},
		RootPrefix: []string{"orb", "-u", "root", "-m", machine},
		Machine:    machine,
	}
}

// noopResolve fails the test if called — used by tests that must skip the
// resolve+rebuild path entirely (I2).
func noopResolve(t *testing.T) func(context.Context) (string, string, error) {
	return func(context.Context) (string, string, error) {
		t.Fatal("resolve must not be called when the closed posture is already active")
		return "", "", nil
	}
}

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := &closedChainRunner{FakeRunner: exec.NewFakeRunner()}
	r.Script("orb -u root -m lever-jail iptables", exec.Result{})
	r.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	g := orbGuest(r, "lever-jail")

	v4, _, rebuilt, err := g.ApplyEgress(context.Background(), noopResolve(t), []int{8443}, true)
	if err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	// I2: an already-closed chain must NOT be flushed or re-resolved — that would
	// briefly open egress for a running agent.
	if r.flushed {
		t.Fatal("must not flush LEVER_EGRESS when the closed posture is already active (would open egress)")
	}
	if r.resolved {
		t.Fatal("must not re-resolve the alias (DNS) when already closed — read it from the chain")
	}
	if v4 != "0.250.250.254" {
		t.Fatalf("alias should be read from the existing chain, got %q", v4)
	}
	// rebuilt=false tells the caller v6 is NOT authoritative here (the skip path
	// parses only the v4 ACCEPT rule from the live chain) — it must not overwrite
	// a previously-resolved v6 alias.
	if rebuilt {
		t.Fatal("rebuilt should be false on the I2 skip path")
	}
}

func TestApplyEgressFlushesChainBeforeResolving(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) {
		f.Run(context.Background(), nil, "orb", "-m", "lever-jail", "getent", "ahosts", "host.orb.internal")
		return "0.250.250.254", "fd07::fe", nil
	}
	if _, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	} else if !rebuilt {
		t.Fatal("rebuilt should be true when the chain is not already closed")
	}
	flushIdx, getentIdx := -1, -1
	for i, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if strings.Contains(argv, "iptables -F LEVER_EGRESS") {
			flushIdx = i
		}
		if strings.Contains(argv, "getent ahosts host.orb.internal") {
			getentIdx = i
		}
	}
	if flushIdx < 0 {
		t.Fatal("ApplyEgress must flush LEVER_EGRESS (idempotent re-apply, no rule accumulation)")
	}
	// Flush BEFORE resolve: under a prior closed posture the catch-all DROP blocks
	// DNS/53; flushing the chain first restores it so the re-resolve succeeds.
	if getentIdx < 0 || flushIdx > getentIdx {
		t.Fatalf("flush (idx %d) must precede the host-alias resolve (idx %d)", flushIdx, getentIdx)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "0.250.250.254", "fd07::fe", nil }
	if _, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, []int{3305}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	} else if !rebuilt {
		t.Fatal("rebuilt should be true on a normal (open-posture) apply")
	}
	var sawAccept, sawDrop bool
	for _, c := range f.Calls {
		j := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(j, "iptables") && strings.Contains(j, "--dport 3305") && strings.Contains(j, "ACCEPT") {
			sawAccept = true
		}
		if strings.Contains(j, "iptables") && strings.Contains(j, "0.250.250.254 -j DROP") {
			sawDrop = true
		}
	}
	if !sawAccept || !sawDrop {
		t.Fatalf("accept=%t drop=%t", sawAccept, sawDrop)
	}
}
