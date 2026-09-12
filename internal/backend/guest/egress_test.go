package guest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
)

// orbGuest returns a Guest shaped like orbstack's, for argv-identical assertions.
func orbGuest(host proc.Runner, machine string) Guest {
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
	r := &backendtest.ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "orb"}
	r.Script("orb -u root -m lever-jail iptables", proc.Result{})
	r.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(r, "lever-jail")

	v4, _, rebuilt, err := g.ApplyEgress(context.Background(), noopResolve(t), nil, []int{8443}, true)
	if err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	// I2: an already-closed chain must NOT be flushed or re-resolved — that would
	// briefly open egress for a running agent.
	if r.Flushed {
		t.Fatal("must not flush LEVER_EGRESS when the closed posture is already active (would open egress)")
	}
	if r.Resolved {
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
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) {
		f.Run(context.Background(), nil, "orb", "-m", "lever-jail", "getent", "ahosts", "host.orb.internal")
		return "0.250.250.254", "fd07::fe", nil
	}
	if _, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, nil, []int{8443}, true); err != nil {
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
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "0.250.250.254", "fd07::fe", nil }
	if _, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, nil, []int{3305}, false); err != nil {
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

// --- lever#34: resolver DNAT targets ACCEPTed in the open posture only ---

func TestApplyEgressAcceptsResolverForwardTargetsBeforeAliasDrop(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "192.168.5.2", "", nil }
	var askedFor string
	dns := func(_ context.Context, alias string) ([]egress.DNSForward, error) {
		askedFor = alias
		return []egress.DNSForward{{Proto: "udp", Port: 41234}, {Proto: "tcp", Port: 41235}}, nil
	}
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, dns, []int{8443}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	// The hook is asked for the RESOLVED alias, so it can filter DNAT targets
	// to the alias host.
	if askedFor != "192.168.5.2" {
		t.Fatalf("dns hook must receive the resolved v4 alias, got %q", askedFor)
	}
	udp := f.CallIndex(proc.ArgvContains("iptables -A LEVER_EGRESS -d 192.168.5.2 -p udp --dport 41234 -j ACCEPT"))
	tcp := f.CallIndex(proc.ArgvContains("iptables -A LEVER_EGRESS -d 192.168.5.2 -p tcp --dport 41235 -j ACCEPT"))
	drop := f.CallIndex(proc.ArgvContains("iptables -A LEVER_EGRESS -d 192.168.5.2 -j DROP"))
	if udp < 0 || tcp < 0 || drop < 0 {
		t.Fatalf("expected udp/tcp forward ACCEPTs and the alias DROP: udp=%d tcp=%d drop=%d", udp, tcp, drop)
	}
	if udp > drop || tcp > drop {
		t.Fatalf("forward ACCEPTs (udp=%d tcp=%d) must precede the alias DROP (%d)", udp, tcp, drop)
	}
}

func TestApplyEgressNeverAsksResolverForwardWhenClosed(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "192.168.5.2", "", nil }
	dns := func(context.Context, string) ([]egress.DNSForward, error) {
		t.Fatal("the closed posture keeps DNS dropped by design: the forward hook must not run")
		return nil, nil
	}
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, dns, []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	if f.Called(proc.ArgvContains("--dport 41234")) {
		t.Fatal("closed posture must not ACCEPT a resolver forward port")
	}
}

func TestApplyEgressResolverForwardErrorFails(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "192.168.5.2", "", nil }
	dns := func(context.Context, string) ([]egress.DNSForward, error) { return nil, errors.New("boom") }
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, dns, []int{8443}, false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the hook error to surface, got %v", err)
	}
}
