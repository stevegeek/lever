package backendtest

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestClosedChainRunnerInterceptsOnlyItsHost(t *testing.T) {
	r := &ClosedChainRunner{FakeRunner: exec.NewFakeRunner(), Host: "orb"}
	r.Script("orb", exec.Result{})
	r.Script("limactl", exec.Result{Stdout: "other\n"})
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
	r := &ClosedChainRunner{FakeRunner: exec.NewFakeRunner(), Host: "orb", Open: true}
	_, err := r.Run(context.Background(), nil, "orb", "iptables", "-S", "LEVER_EGRESS")
	if err == nil || !strings.Contains(err.Error(), "unscripted") {
		t.Fatalf("open posture must consult the FakeRunner: %v", err)
	}
}

func TestScriptRunUser(t *testing.T) {
	f := exec.NewFakeRunner()
	ScriptRunUser(f, "orb -m m", "stephen", "501")
	res, _ := f.Run(context.Background(), nil, "orb", "-m", "m", "id", "-u")
	if res.Stdout != "501\n" {
		t.Fatalf("uid = %q", res.Stdout)
	}
}
