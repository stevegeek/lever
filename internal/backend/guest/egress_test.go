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

// scriptFirewall answers every root iptables call and the restore commit.
func scriptFirewall(f *proc.FakeRunner) {
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	f.Script("orb -u root -m lever-jail bash -c exec ip", proc.Result{})
}

// committedV4 is the IPv4 ruleset ApplyEgress committed.
func committedV4(t *testing.T, f *proc.FakeRunner) string {
	t.Helper()
	bins, inputs := backendtest.RestoreCommits(f)
	for i, b := range bins {
		if b == "iptables-restore" {
			return inputs[i]
		}
	}
	t.Fatalf("no iptables-restore commit; calls=%+v", f.Calls)
	return ""
}

// The chain is replaced in one commit per family AFTER the resolve, never
// flushed: a flush would leave OUTPUT's default ACCEPT in force until the
// rules were back.
func TestApplyEgressCommitsAtomicallyAfterResolving(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
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
	backendtest.AssertAtomicCommit(t, f, "host.orb.internal")
	// The chain and the jump are ensured without touching live rules.
	if !f.Called(proc.ArgvContains("iptables -N LEVER_EGRESS")) || !f.Called(proc.ArgvContains("iptables -C OUTPUT -j LEVER_EGRESS")) {
		t.Fatal("ApplyEgress must ensure the chain and the OUTPUT jump")
	}
	// The whole v4 ruleset, in order, is in the one commit.
	want := egress.RestoreInput(egress.BuildRules("0.250.250.254", "fd07::fe", []int{8443}, true), egress.IPv4)
	if got := committedV4(t, f); got != want {
		t.Fatalf("committed v4 ruleset:\n%s\nwant:\n%s", got, want)
	}
}

// A failed commit leaves the previous chain in force: nothing flushes it
// first, and the error says so.
func TestApplyEgressFailedCommitKeepsTheOldChain(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	// No script for the restore: the commit fails.
	g := orbGuest(f, "lever-jail")
	resolve := func(context.Context) (string, string, error) { return "0.250.250.254", "", nil }
	_, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, nil, []int{8443}, false)
	if err == nil || !strings.Contains(err.Error(), "previous chain stays in force") {
		t.Fatalf("want the commit failure, got %v", err)
	}
	if rebuilt {
		t.Fatal("rebuilt must be false when nothing was committed")
	}
	for _, c := range f.Calls {
		argv := c.Argv()
		if strings.Contains(argv, "-F LEVER_EGRESS") || strings.Contains(argv, "-X LEVER_EGRESS") || strings.Contains(argv, "-D OUTPUT") {
			t.Fatalf("the live chain was touched before the commit: %s", argv)
		}
	}
}

// When the resolve cannot run under the live chain (a closed chain's DROP
// blocks DNS, e.g. switching closed -> open), the alias is read back from
// the live chain instead of flushing it to let DNS through.
func TestApplyEgressFallsBackToTheLiveChainAlias(t *testing.T) {
	r := &backendtest.ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "orb"}
	scriptFirewall(r.FakeRunner)
	g := orbGuest(r, "lever-jail")
	resolve := func(context.Context) (string, string, error) { return "", "", errors.New("dns blocked") }
	v4, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, nil, []int{8443}, false)
	if err != nil || !rebuilt {
		t.Fatalf("ApplyEgress: rebuilt=%v err=%v", rebuilt, err)
	}
	if v4 != backendtest.HostAliasV4 {
		t.Fatalf("alias = %q, want the one the live chain names", v4)
	}
	if r.Flushed {
		t.Fatal("must not flush the live chain to get DNS back")
	}
	if !strings.Contains(committedV4(t, r.FakeRunner), "-d "+backendtest.HostAliasV4+" -j DROP") {
		t.Fatal("the committed ruleset must use the live chain's alias")
	}
}

// With neither a resolve nor a live alias, ApplyEgress fails and leaves the
// chain alone rather than reopening egress to find out.
func TestApplyEgressFailsClosedWithoutAnyAlias(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
	g := orbGuest(f, "lever-jail")
	resolve := func(context.Context) (string, string, error) { return "", "", errors.New("dns blocked") }
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, nil, []int{8443}, false); err == nil || !strings.Contains(err.Error(), "left as it is") {
		t.Fatalf("want a fail-closed error, got %v", err)
	}
	if bins, _ := backendtest.RestoreCommits(f); len(bins) != 0 {
		t.Fatalf("nothing may be committed without an alias, got %v", bins)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "0.250.250.254", "fd07::fe", nil }
	if _, _, rebuilt, err := g.ApplyEgress(context.Background(), resolve, nil, []int{3305}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	} else if !rebuilt {
		t.Fatal("rebuilt should be true on a normal (open-posture) apply")
	}
	v4 := committedV4(t, f)
	if !strings.Contains(v4, "--dport 3305 -j ACCEPT") || !strings.Contains(v4, "-d 0.250.250.254 -j DROP") {
		t.Fatalf("committed ruleset:\n%s", v4)
	}
}

// --- lever#34: resolver DNAT targets ACCEPTed in the open posture only ---

func TestApplyEgressAcceptsResolverForwardTargetsBeforeAliasDrop(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
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
	v4 := committedV4(t, f)
	udp := strings.Index(v4, "-A LEVER_EGRESS -d 192.168.5.2 -p udp --dport 41234 -j ACCEPT")
	tcp := strings.Index(v4, "-A LEVER_EGRESS -d 192.168.5.2 -p tcp --dport 41235 -j ACCEPT")
	drop := strings.Index(v4, "-A LEVER_EGRESS -d 192.168.5.2 -j DROP")
	if udp < 0 || tcp < 0 || drop < 0 {
		t.Fatalf("expected udp/tcp forward ACCEPTs and the alias DROP: udp=%d tcp=%d drop=%d", udp, tcp, drop)
	}
	if udp > drop || tcp > drop {
		t.Fatalf("forward ACCEPTs (udp=%d tcp=%d) must precede the alias DROP (%d)", udp, tcp, drop)
	}
}

func TestApplyEgressNeverAsksResolverForwardWhenClosed(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "192.168.5.2", "", nil }
	dns := func(context.Context, string) ([]egress.DNSForward, error) {
		t.Fatal("the closed posture keeps DNS dropped by design: the forward hook must not run")
		return nil, nil
	}
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, dns, []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	if strings.Contains(committedV4(t, f), "--dport 41234") {
		t.Fatal("closed posture must not ACCEPT a resolver forward port")
	}
}

func TestApplyEgressResolverForwardErrorFails(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptFirewall(f)
	g := orbGuest(f, "lever-jail")

	resolve := func(context.Context) (string, string, error) { return "192.168.5.2", "", nil }
	dns := func(context.Context, string) ([]egress.DNSForward, error) { return nil, errors.New("boom") }
	if _, _, _, err := g.ApplyEgress(context.Background(), resolve, dns, []int{8443}, false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the hook error to surface, got %v", err)
	}
}
