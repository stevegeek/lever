package backendtest

import (
	"context"
	"errors"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

var orb = Guest{Machine: "m", User: "orb -m m", Root: "orb -u root -m m", Alias: "host.orb.internal"}

func TestClosedChainRunnerInterceptsOnlyItsHost(t *testing.T) {
	r := &ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "orb"}
	r.Script("orb", proc.Result{})
	r.Script("limactl", proc.Result{Stdout: "other\n"})
	res, err := r.Run(context.Background(), nil, "orb", "-u", "root", "-m", "m", "iptables", "-S", "LEVER_EGRESS")
	if err != nil || res.Stdout != ClosedChain {
		t.Fatalf("closed chain not served: %q %v", res.Stdout, err)
	}
	if res, _ := r.Run(context.Background(), nil, "limactl", "shell", "m", "sudo", "iptables", "-S", "LEVER_EGRESS"); res.Stdout != "other\n" {
		t.Fatalf("another host binary must fall through: %q", res.Stdout)
	}
	_, _ = r.Run(context.Background(), nil, "orb", "-u", "root", "-m", "m", "iptables", "-F", "LEVER_EGRESS")
	_, _ = r.Run(context.Background(), nil, "orb", "-m", "m", "getent", "ahosts", "host.orb.internal")
	if !r.Flushed || !r.Resolved {
		t.Fatalf("flushed=%v resolved=%v", r.Flushed, r.Resolved)
	}
	// The served chain probe is answered without reaching the FakeRunner; the
	// rest are recorded as usual.
	if len(r.Calls) != 3 {
		t.Fatalf("pass-through calls must still be recorded: %d", len(r.Calls))
	}
}

func TestClosedChainRunnerOpenFallsThrough(t *testing.T) {
	r := &ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "orb", Open: true}
	_, err := r.Run(context.Background(), nil, "orb", "iptables", "-S", "LEVER_EGRESS")
	if !errors.Is(err, proc.ErrUnscripted) {
		t.Fatalf("open posture must consult the FakeRunner: %v", err)
	}
}

func TestGuestScriptRunUser(t *testing.T) {
	f := proc.NewFakeRunner()
	orb.ScriptRunUser(f, "stephen", "501")
	res, _ := f.Run(context.Background(), nil, "orb", "-m", "m", "id", "-u")
	if res.Stdout != "501\n" {
		t.Fatalf("uid = %q", res.Stdout)
	}
}

func TestGuestScriptProvisionCoversEveryProbe(t *testing.T) {
	f := proc.NewFakeRunner()
	orb.ScriptProvision(f, "501", AhostsDual)
	ctx := context.Background()
	for _, argv := range [][]string{
		{"orb", "-m", "m", "whoami"},
		{"orb", "-m", "m", "id", "-u"},
		{"orb", "-m", "m", "bash", "-c", "x"},
		{"orb", "-u", "root", "-m", "m", "bash", "-c", "x"},
		{"orb", "-m", "m", "uname", "-m"},
		{"orb", "-m", "m", "getent", "ahosts", "host.orb.internal"},
		{"orb", "-u", "root", "-m", "m", "iptables", "-S", "LEVER_EGRESS"},
		{"orb", "-u", "root", "-m", "m", "ip6tables", "-F", "LEVER_EGRESS"},
	} {
		if _, err := f.Run(ctx, nil, argv[0], argv[1:]...); err != nil {
			t.Errorf("%v: %v", argv, err)
		}
	}
	if res, _ := f.Run(ctx, nil, "orb", "-m", "m", "getent", "ahosts", "host.orb.internal"); res.Stdout != AhostsDual {
		t.Fatalf("ahosts = %q", res.Stdout)
	}
}

func TestGuestClosedChain(t *testing.T) {
	r := orb.ClosedChain(AhostsV4, false)
	if r.Host != "orb" {
		t.Fatalf("Host = %q", r.Host)
	}
	res, err := r.Run(context.Background(), nil, "orb", "-u", "root", "-m", "m", "iptables", "-S", "LEVER_EGRESS")
	if err != nil || res.Stdout != ClosedChain {
		t.Fatalf("closed chain not served: %q %v", res.Stdout, err)
	}
	if _, err := r.Run(context.Background(), nil, "orb", "-u", "root", "-m", "m", "ip6tables", "-F", "LEVER_EGRESS"); err != nil {
		t.Fatalf("firewall not scripted: %v", err)
	}
}

func TestAssertHelpers(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb", proc.Result{})
	ctx := context.Background()
	_, _ = f.Run(ctx, nil, "orb", "-u", "root", "-m", "m", "iptables", "-F", "LEVER_EGRESS")
	_, _ = f.Run(ctx, nil, "orb", "-m", "m", "getent", "ahosts", "host.orb.internal")
	_, _ = f.Run(ctx, nil, "orb", "-u", "root", "-m", "m", "iptables", "-A", "LEVER_EGRESS", "-d", HostAliasV4+"/32", "-p", "tcp", "-m", "tcp", "--dport", "8443", "-j", "ACCEPT")
	_, _ = f.Run(ctx, nil, "orb", "-u", "root", "-m", "m", "iptables", "-A", "LEVER_EGRESS", "-d", HostAliasV4, "-j", "DROP")
	AssertNoSubcommand(t, f, "orb", "create", "start")
	AssertEgressRules(t, f, "8443")
	AssertFlushPrecedesResolve(t, f, "host.orb.internal")
	AssertNoNodeTooling(t, f)
}
