package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
)

// TestProfileIsSingleSourced guards against re-hardcoding the profile: OrbStack's
// runtime Profile() must be the exported Profile value the registry publishes.
func TestProfileIsSingleSourced(t *testing.T) {
	want := Profile
	if got := New(proc.RealRunner{}, "m", common.Options{}).Profile(); got != want {
		t.Errorf("Profile() = %+v, want declared %+v", got, want)
	}
}

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := &backendtest.ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "orb", Open: true}
	r.Script("orb -u root -m lever-jail iptables", proc.Result{})
	r.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	r.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	b := New(r, "lever-jail", common.Options{})
	// A prior apply closed the chain; the re-apply below must hit the skip path.
	if err := b.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("first ApplyEgress: %v", err)
	}
	r.Open, r.Flushed, r.Resolved = false, false, false
	if err := b.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
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
	if b.HostAliasV4() != "0.250.250.254" {
		t.Fatalf("alias should be read from the existing chain, got %q", b.HostAliasV4())
	}
}

func TestApplyEgressFlushesChainBeforeResolving(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	b := New(f, "lever-jail", common.Options{})
	if err := b.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
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

// orbVersionScript scripts a successful `orb version` response for >= 2.1.1.
func orbVersionScript(f *proc.FakeRunner) {
	f.Script("orb version", proc.Result{Stdout: "Version: 2.2.1 (2020100)\n"})
}

func scriptedMachine(f *proc.FakeRunner) {
	orbVersionScript(f)
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"}) // machine already exists
	f.Script("orb -m lever-jail whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", proc.Result{Stdout: "501\n"})
	f.Script("orb -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	// EnsureScion (when a test sets ScionSource/ScionVersion): guest arch detection.
	f.Script("orb -m lever-jail uname -m", proc.Result{Stdout: "arm64\n"})
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
}

func TestEnsureUpIsIdempotentWhenMachineExists(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail", common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/Users/x/tree", AllowedPorts: []int{3305},
	})
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	// Must NOT call `orb create` when the machine already exists.
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "create" {
			t.Fatalf("create called though machine exists: %+v", c)
		}
	}
}

func TestEnsureUpCreatesIsolatedMachineWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	orbVersionScript(f)
	f.Script("orb list", proc.Result{Stdout: "\n"}) // no machines
	f.Script("orb create --isolated --mount", proc.Result{Stdout: "created\n"})
	f.Script("orb -m lever-jail whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", proc.Result{Stdout: "501\n"})
	f.Script("orb -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: "lever-jail", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	var sawCreate bool
	for _, c := range f.Calls {
		if c.Name == "orb" && strings.Join(c.Args, " ") == "create --isolated --mount /Users/x/tree:/lever ubuntu lever-jail" {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Fatalf("expected `orb create --isolated --mount /Users/x/tree:/lever ubuntu lever-jail`; calls=%+v", f.Calls)
	}
}

// --- ResolveRunUser: passive attach must resolve without provisioning. ---

func TestResolveRunUserResolvesWhenMachineRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb -m lever-jail whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", proc.Result{Stdout: "501\n"})
	b := New(f, "lever-jail", common.Options{})

	if err := b.ResolveRunUser(context.Background()); err != nil {
		t.Fatalf("ResolveRunUser: %v", err)
	}
	if b.RunUser() != "leveruser" || b.RunUID() != "501" {
		t.Fatalf("run user/uid not resolved: user=%q uid=%q", b.RunUser(), b.RunUID())
	}
	// No provisioning calls: only the read-only list + whoami/id -u probes.
	for _, c := range f.Calls {
		argv := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(argv, "create") || strings.Contains(argv, "iptables") || strings.Contains(argv, "apt-get") {
			t.Fatalf("ResolveRunUser must not provision anything; saw call: %s", argv)
		}
	}
}

func TestResolveRunUserErrorsWhenMachineAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "\n"}) // no machines
	b := New(f, "lever-jail", common.Options{})

	err := b.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the machine does not exist")
	}
	if !strings.Contains(err.Error(), "lever-jail") {
		t.Fatalf("error should name the machine; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "create" {
			t.Fatalf("create must NOT be called by ResolveRunUser: %+v", c)
		}
	}
}

func TestResolveRunUserErrorsWhenMachineNotRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail stopped ubuntu\n"})
	b := New(f, "lever-jail", common.Options{})

	err := b.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the machine is not running")
	}
	if !strings.Contains(err.Error(), "lever-jail") {
		t.Fatalf("error should name the machine; got: %v", err)
	}
	for _, c := range f.Calls {
		argv := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(argv, "whoami") || strings.Contains(argv, "id -u") {
			t.Fatalf("must not attempt to resolve run user on a non-running machine: %s", argv)
		}
	}
}

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := proc.NewFakeRunner()
	orbVersionScript(f)
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb -m lever-jail whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", proc.Result{Stdout: "1000\n"}) // non-default uid
	f.Script("orb -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", proc.Result{Stdout: "ok\n"})
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: "lever-jail", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := b.DockerHost(); !strings.Contains(got, "/run/user/1000/") {
		t.Fatalf("DockerHost should reflect resolved uid 1000; got %q", got)
	}
}

func TestProfileDeclaresSharedKernelAndFragile(t *testing.T) {
	p := New(proc.NewFakeRunner(), "lever-jail", common.Options{}).Profile()
	if p.SeparateKernel || !p.VersionFragile {
		t.Fatalf("orbstack profile wrong: %+v", p)
	}
}

func TestProfileFSBoundedByIsHonest(t *testing.T) {
	p := New(proc.NewFakeRunner(), "lever-jail", common.Options{}).Profile()
	if !strings.Contains(p.FSBoundedBy, "/lever") {
		t.Fatalf("Profile.FSBoundedBy should mention /lever mount; got %q", p.FSBoundedBy)
	}
	if strings.Contains(p.FSBoundedBy, "NOT yet") {
		t.Fatalf("Profile.FSBoundedBy still contains stale 'NOT yet' wording; got %q", p.FSBoundedBy)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", proc.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", proc.Result{})
	b := New(f, "lever-jail", common.Options{})

	if err := b.ApplyEgress(context.Background(), []int{3305}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
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

// --- Stop: power off, keep disk (distinct from Teardown). ---

func TestStopStopsMachineWhenListed(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb stop lever-jail", proc.Result{})
	if err := New(f, "lever-jail", common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	last := f.Calls[len(f.Calls)-1]
	if last.Name != "orb" || len(last.Args) == 0 || last.Args[0] != "stop" {
		t.Fatalf("expected last call orb stop lever-jail; got %+v", f.Calls)
	}
}

func TestStopIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "\n"}) // no machines
	if err := New(f, "lever-jail", common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "stop" {
			t.Fatalf("stop must NOT be called when machine absent: %+v", f.Calls)
		}
	}
}

func TestStopOnAlreadyStoppedMachineIsHarmless(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail stopped ubuntu\n"})
	f.Script("orb stop lever-jail", proc.Result{})
	if err := New(f, "lever-jail", common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop on an already-stopped machine should be harmless, got: %v", err)
	}
}

// --- ensureMachine: a stopped machine is STARTED (so `up` resumes it), not
// treated as a no-op. ---

func TestEnsureMachineStartsStoppedMachine(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail stopped ubuntu\n"})
	f.Script("orb start lever-jail", proc.Result{})
	f.Script("orb -m lever-jail true", proc.Result{})
	b := New(f, "lever-jail", common.Options{})

	if err := b.ensureMachine(context.Background(), "/Users/x/tree"); err != nil {
		t.Fatalf("ensureMachine: %v", err)
	}
	var sawStart, sawCreate bool
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 {
			if c.Args[0] == "start" {
				sawStart = true
			}
			if c.Args[0] == "create" {
				sawCreate = true
			}
		}
	}
	if !sawStart {
		t.Fatal("expected `orb start` for a stopped machine")
	}
	if sawCreate {
		t.Fatal("create must NOT be called for an already-existing (stopped) machine")
	}
}

func TestEnsureMachineRunningIsNoop(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"})
	b := New(f, "lever-jail", common.Options{})

	if err := b.ensureMachine(context.Background(), "/Users/x/tree"); err != nil {
		t.Fatalf("ensureMachine: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && (c.Args[0] == "start" || c.Args[0] == "create") {
			t.Fatalf("neither start nor create should be called for an already-running machine: %+v", f.Calls)
		}
	}
}

func TestEnsureMachineStartTimesOutWhenUnreachable(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail stopped ubuntu\n"})
	f.Script("orb start lever-jail", proc.Result{})
	// "orb -m lever-jail true" is deliberately unscripted: FakeRunner errors on
	// every unscripted call, simulating a machine that never becomes reachable.
	b := New(f, "lever-jail", common.Options{})
	b.probeAttempts, b.probeInterval = 2, time.Millisecond

	err := b.ensureMachine(context.Background(), "/Users/x/tree")
	if err == nil {
		t.Fatal("expected an error when the machine never becomes reachable after start")
	}
	if !strings.Contains(err.Error(), "lever-jail") {
		t.Fatalf("error should name the machine; got: %v", err)
	}
}

func TestTeardownDeletesMachineWhenPresent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb delete lever-jail", proc.Result{})
	if err := New(f, "lever-jail", common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if f.Calls[len(f.Calls)-1].Name != "orb" || f.Calls[len(f.Calls)-1].Args[0] != "delete" {
		t.Fatalf("expected last call orb delete; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb list", proc.Result{Stdout: "\n"}) // no machines
	if err := New(f, "lever-jail", common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "orb" && len(c.Args) > 0 && c.Args[0] == "delete" {
			t.Fatalf("delete must NOT be called when machine absent: %+v", f.Calls)
		}
	}
}

// --- OrbStack version preflight tests ---

func TestOrbVersionAtLeast(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantOK  bool
		wantErr bool
		wantGot string
	}{
		{
			name:    "2.2.1 >= 2.1.1 → ok",
			stdout:  "Version: 2.2.1 (2020100)\n",
			wantOK:  true,
			wantGot: "2.2.1",
		},
		{
			name:    "2.1.1 >= 2.1.1 → ok (exact match)",
			stdout:  "Version: 2.1.1 (2000000)\n",
			wantOK:  true,
			wantGot: "2.1.1",
		},
		{
			name:    "2.1.0 >= 2.1.1 → too old",
			stdout:  "Version: 2.1.0 (1990000)\n",
			wantOK:  false,
			wantGot: "2.1.0",
		},
		{
			name:    "2.0.9 >= 2.1.1 → too old (minor mismatch)",
			stdout:  "Version: 2.0.9 (1900000)\n",
			wantOK:  false,
			wantGot: "2.0.9",
		},
		{
			name:    "3.0.0 >= 2.1.1 → ok (major bump)",
			stdout:  "Version: 3.0.0 (9999999)\n",
			wantOK:  true,
			wantGot: "3.0.0",
		},
		{
			name:    "1.9.9 >= 2.1.1 → too old (major too low)",
			stdout:  "Version: 1.9.9 (1000000)\n",
			wantOK:  false,
			wantGot: "1.9.9",
		},
		{
			name:    "malformed output → error",
			stdout:  "orb: command not found\n",
			wantErr: true,
		},
		{
			name:    "empty output → error",
			stdout:  "",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script("orb version", proc.Result{Stdout: tc.stdout})
			ok, got, err := common.VersionAtLeast(context.Background(), f, []string{"orb", "version"}, orbVersionRe, 2, 1, 1)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (ok=%t got=%q)", ok, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("ok: want %t got %t", tc.wantOK, ok)
			}
			if got != tc.wantGot {
				t.Errorf("got version string: want %q got %q", tc.wantGot, got)
			}
		})
	}
}

func TestEnsureUpRejectsOldOrb(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb version", proc.Result{Stdout: "Version: 2.1.0 (1990000)\n"})
	b := New(f, "lever-jail", common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail",
		ProjectTree: "/Users/x/tree",
	})
	if err == nil {
		t.Fatal("expected error for OrbStack 2.1.0, got nil")
	}
	if !strings.Contains(err.Error(), "OrbStack") || !strings.Contains(err.Error(), "2.1.1") {
		t.Fatalf("error should mention OrbStack >= 2.1.1; got: %v", err)
	}
	if !strings.Contains(err.Error(), "2.1.0") {
		t.Fatalf("error should show the found version; got: %v", err)
	}
}

func TestEnsureUpInstallsPodman(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail", common.Options{})
	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: "lever-jail", ProjectTree: "/t", AllowedPorts: nil}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	var sawPodman bool
	for _, c := range f.Calls {
		j := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(j, "apt-get install") && strings.Contains(j, "podman") {
			sawPodman = true
		}
	}
	if !sawPodman {
		t.Fatalf("expected podman install; calls=%+v", f.Calls)
	}
}

func TestEnsureScionBuildsAndInstalls(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	f.Script("go build", proc.Result{})
	f.Script("bash -c", proc.Result{})
	// The install now reads a digest marker first and records one after.
	f.Script("orb -m lever-jail sh -c", proc.Result{Code: 1})
	f.Script("orb -u root -m lever-jail sh -c", proc.Result{})
	src := t.TempDir() // must exist for the stat check
	backendtest.StageFakeBuildOutput(t, "lever-jail")
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/t", ScionSource: src,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	var sawBuild, sawInstall bool
	for _, c := range f.Calls {
		if c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build" {
			if c.Dir != src {
				t.Errorf("build Dir: want %q got %q", src, c.Dir)
			}
			if c.Env["GOOS"] != "linux" || c.Env["GOARCH"] != "arm64" {
				t.Errorf("build env: want linux/arm64 got %+v", c.Env)
			}
			var sawCmd bool
			var binArg string
			for i, a := range c.Args {
				if a == "./cmd/scion" {
					sawCmd = true
				}
				if a == "-o" && i+1 < len(c.Args) {
					binArg = c.Args[i+1]
				}
			}
			if !sawCmd {
				t.Errorf("build args should contain ./cmd/scion; got %+v", c.Args)
			}
			if !strings.Contains(binArg, "lever-scion-lever-jail") {
				t.Errorf("build output path should include per-machine name lever-scion-lever-jail; got %q", binArg)
			}
			sawBuild = true
		}
		// The install: root prefix, guest-side atomic script, binary on stdin.
		if c.Name == "orb" && len(c.Args) >= 6 && reflect.DeepEqual(c.Args[:6], []string{"-u", "root", "-m", "lever-jail", "bash", "-c"}) {
			script := c.Args[len(c.Args)-1]
			if strings.Contains(script, "scion.tmp") &&
				strings.Contains(script, "mv") &&
				strings.Contains(script, "/usr/local/bin/scion") &&
				c.Stdin == "fake-scion-lever-jail" {
				sawInstall = true
			}
		}
	}
	if !sawBuild {
		t.Fatalf("expected go build for ./cmd/scion in %q; calls=%+v", src, f.Calls)
	}
	if !sawInstall {
		t.Fatalf("expected atomic scion install into jail via the root prefix; calls=%+v", f.Calls)
	}
}

func TestEnsureScionSkippedWhenEmpty(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/t", ScionSource: "",
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build" {
			t.Fatalf("go build must NOT be called when ScionSource empty: %+v", c)
		}
		if c.Name == "bash" && len(c.Args) >= 2 && strings.Contains(c.Args[1], "/usr/local/bin/scion") {
			t.Fatalf("scion install must NOT be called when ScionSource empty: %+v", c)
		}
	}
}

func TestEnsureScionSourceMissing(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	b := New(f, "lever-jail", common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/t", ScionSource: missing,
	})
	if err == nil {
		t.Fatal("expected error for missing scion source, got nil")
	}
	if !strings.Contains(err.Error(), "scion source") {
		t.Fatalf("error should mention scion source; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build" {
			t.Fatalf("go build must NOT be called when source missing (stat short-circuits): %+v", c)
		}
	}
}

func TestEnsureUpRequiresProjectTree(t *testing.T) {
	f := proc.NewFakeRunner()
	// No `orb version` needed: ProjectTree guard fires before the preflight.
	b := New(f, "lever-jail", common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail",
		ProjectTree: "", // empty
	})
	if err == nil {
		t.Fatal("expected error for empty ProjectTree, got nil")
	}
	if !strings.Contains(err.Error(), "ProjectTree") {
		t.Fatalf("error should mention ProjectTree; got: %v", err)
	}
}

// resolvedOrb returns a backend whose run user is resolved (stephen/501)
// through the same probes EnsureUp issues, without provisioning anything.
func resolvedOrb(t *testing.T, f *proc.FakeRunner, machine string) *OrbStack {
	t.Helper()
	backendtest.ScriptRunUser(f, "orb -m "+machine, "stephen", "501")
	o := New(f, machine, common.Options{})
	if err := o.ReadRunUser(context.Background()); err != nil {
		t.Fatalf("ReadRunUser: %v", err)
	}
	f.Calls = nil
	return o
}

func TestJailTransportMethods(t *testing.T) {
	o := resolvedOrb(t, proc.NewFakeRunner(), "lever-x")

	if got := JailPrefix("lever-x", "stephen"); !reflect.DeepEqual(got, []string{"orb", "-m", "lever-x", "-u", "stephen"}) {
		t.Fatalf("JailPrefix = %v", got)
	}
	if o.JailRunner() == nil {
		t.Fatal("JailRunner() = nil")
	}
	attach := o.AttachArgv([]string{"scion", "attach"})
	if attach[0] != "orb" || attach[len(attach)-1] != "attach" {
		t.Fatalf("AttachArgv = %v", attach)
	}
}

// --- F1 guard: per-backend exact-argv assertions for the image-op / jail-
// transport forwarders. Before F1 these forwarders had no per-backend argv
// assertion (TestJailTransportMethods only checked non-nil), so a prefix mis-
// wiring in the shared base — the orbstack jail prefix is run-user-dependent —
// would only fail at runtime. LoadImage/ImageLoaded/PruneJailImages exec docker
// directly (unobservable offline) but share jailPrefix() with JailRunner and
// AttachArgv, so pinning those two transitively guards them; InstallGuestBinary
// is pinned directly through the root transport. ---

func TestInstallGuestBinaryArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb", proc.Result{})
	local := filepath.Join(t.TempDir(), "lever-agent")
	if err := os.WriteFile(local, []byte("agent-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := New(f, "lever-x", common.Options{})
	if err := o.InstallGuestBinary(context.Background(), local, "/usr/local/bin/lever-agent"); err != nil {
		t.Fatalf("InstallGuestBinary: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 host call, got %d", len(f.Calls))
	}
	c := f.Calls[0]
	// The root transport prefix must be wired through verbatim, with the
	// binary on stdin of the guest-side script.
	if c.Name != "orb" || !reflect.DeepEqual(c.Args[:6], []string{"-u", "root", "-m", "lever-x", "bash", "-c"}) {
		t.Fatalf("root prefix mis-wired: %s %v", c.Name, c.Args)
	}
	if c.Stdin != "agent-bytes" {
		t.Fatalf("binary not streamed on stdin: %q", c.Stdin)
	}
}

func TestJailRunnerArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	o := resolvedOrb(t, f, "lever-x")
	f.Script("orb", proc.Result{Stdout: "ok\n"})
	if _, err := o.JailRunner().Run(context.Background(), nil, "true"); err != nil {
		t.Fatalf("JailRunner run: %v", err)
	}
	c := f.Calls[len(f.Calls)-1]
	// jail.Runner uses prefix[0] as the host command and prefix[1:]+env as args:
	// `orb -m lever-x -u stephen env <jailenv...> true`.
	if c.Name != "orb" {
		t.Fatalf("host command = %q, want orb", c.Name)
	}
	if wantPrefix := []string{"-m", "lever-x", "-u", "stephen", "env"}; !reflect.DeepEqual(c.Args[:5], wantPrefix) {
		t.Fatalf("jail prefix mis-wired: %v", c.Args[:5])
	}
	if c.Args[len(c.Args)-1] != "true" {
		t.Fatalf("inner command not last: %v", c.Args)
	}
}

func TestAttachArgvFullPrefix(t *testing.T) {
	o := resolvedOrb(t, proc.NewFakeRunner(), "lever-x")
	attach := o.AttachArgv([]string{"scion", "attach"})
	if wantPrefix := []string{"orb", "-m", "lever-x", "-u", "stephen", "env"}; !reflect.DeepEqual(attach[:6], wantPrefix) {
		t.Fatalf("attach prefix mis-wired: %v", attach[:6])
	}
	if last2 := attach[len(attach)-2:]; !reflect.DeepEqual(last2, []string{"scion", "attach"}) {
		t.Fatalf("inner command not trailing: %v", last2)
	}
}

func TestRunUserUIDAfterEnsureUp(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f) // scripts whoami→leveruser, id -u→501
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/Users/x/tree",
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := b.RunUser(); got != "leveruser" {
		t.Errorf("RunUser: want %q got %q", "leveruser", got)
	}
	if got := b.RunUID(); got != "501" {
		t.Errorf("RunUID: want %q got %q", "501", got)
	}
}

// Binary mode must reach the guest THROUGH EnsureUp. It did not: ScionBinary
// was plumbed into the ScionSpec literal while the guard around it still asked
// only about ScionSource/ScionVersion, so `lever up` installed nothing and
// reported success. Every other binary-mode test calls EnsureScion directly and
// so cannot see it — this one drives the real entry point.
func TestEnsureUpInstallsScionInBinaryMode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "scion")
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F'})
	h[4], h[5], h[6] = 2, 1, 1 // 64-bit, little-endian, v1
	h[16] = 2                  // ET_EXEC
	h[18] = 183                // EM_AARCH64
	h[20] = 1                  // e_version
	h[52] = 64                 // e_ehsize
	if err := os.WriteFile(bin, h, 0o755); err != nil {
		t.Fatal(err)
	}

	f := proc.NewFakeRunner()
	scriptedMachine(f)
	f.Script("orb -m lever-jail uname -m", proc.Result{Stdout: "aarch64\n"})
	f.Script("orb -m lever-jail /usr/bin/sha256sum", proc.Result{Code: 1}) // not installed yet
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/t", ScionBinary: bin,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	var installed bool
	for _, c := range f.Calls {
		if c.Name == "go" {
			t.Fatalf("binary mode must never invoke go through EnsureUp; got %+v", c)
		}
		if len(c.Args) > 1 && c.Args[len(c.Args)-2] == "-c" && strings.Contains(c.Args[len(c.Args)-1], "cat > ") {
			installed = true
		}
	}
	if !installed {
		t.Fatal("EnsureUp with only ScionBinary set installed nothing")
	}
}

// EnsureUp must thread Config.ScionWebUI into the guest's ScionSpec. Each
// backend keeps its OWN copy of that struct literal, and this pair has drifted
// before — ScionBinary was added to both literals while the guard around them
// was updated in neither (see backend.Config.HasScion) — so both backends pin
// it, and both assert the negative too.
func TestEnsureUpBuildsWebAssetsWhenAsked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	f := proc.NewFakeRunner()
	scriptedMachine(f)
	backendtest.ScriptScionInstall(t, f, "orb -m lever-jail", "lever-jail")
	b := New(f, "lever-jail", common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/Users/x/tree",
		ScionSource: backendtest.FakeScionCheckout(t), ScionWebUI: true,
	})
	// node is deliberately not scripted: the web build stops at its toolchain
	// probe, which is only reachable if ScionWebUI was threaded through.
	if err == nil || !strings.Contains(err.Error(), "node/npm toolchain not usable") {
		t.Fatalf("want the web-asset build attempted; got %v", err)
	}
}

func TestEnsureUpSkipsWebAssetsByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	f := proc.NewFakeRunner()
	scriptedMachine(f)
	backendtest.ScriptScionInstall(t, f, "orb -m lever-jail", "lever-jail")
	b := New(f, "lever-jail", common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-jail", ProjectTree: "/Users/x/tree",
		ScionSource: backendtest.FakeScionCheckout(t),
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "npm" || c.Name == "node" {
			t.Fatalf("an instance that serves no UI must not need node: %v %v", c.Name, c.Args)
		}
	}
}
