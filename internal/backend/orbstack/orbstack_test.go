package orbstack

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
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

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := &closedChainRunner{FakeRunner: exec.NewFakeRunner()}
	r.Script("orb -u root -m lever-jail iptables", exec.Result{})
	r.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(r, "lever-jail")
	if err := b.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
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
	if b.HostAliasV4() != "0.250.250.254" {
		t.Fatalf("alias should be read from the existing chain, got %q", b.HostAliasV4())
	}
}

func TestApplyEgressFlushesChainBeforeResolving(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(f, "lever-jail")
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
func orbVersionScript(f *exec.FakeRunner) {
	f.Script("orb version", exec.Result{Stdout: "Version: 2.2.1 (2020100)\n"})
}

func scriptedMachine(f *exec.FakeRunner) {
	orbVersionScript(f)
	f.Script("orb list", exec.Result{Stdout: "lever-jail running ubuntu\n"}) // machine already exists
	f.Script("orb -m lever-jail whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", exec.Result{Stdout: "501\n"})
	f.Script("orb -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
}

func TestEnsureUpIsIdempotentWhenMachineExists(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail")

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
	f := exec.NewFakeRunner()
	orbVersionScript(f)
	f.Script("orb list", exec.Result{Stdout: "\n"}) // no machines
	f.Script("orb create --isolated --mount", exec.Result{Stdout: "created\n"})
	f.Script("orb -m lever-jail whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", exec.Result{Stdout: "501\n"})
	f.Script("orb -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	// ApplyEgress (called by EnsureUp): resolve alias + iptables rules
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(f, "lever-jail")

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

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := exec.NewFakeRunner()
	orbVersionScript(f)
	f.Script("orb list", exec.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb -m lever-jail whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("orb -m lever-jail id -u", exec.Result{Stdout: "1000\n"}) // non-default uid
	f.Script("orb -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -u root -m lever-jail bash", exec.Result{Stdout: "ok\n"})
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(f, "lever-jail")

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: "lever-jail", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := b.DockerHost(); !strings.Contains(got, "/run/user/1000/") {
		t.Fatalf("DockerHost should reflect resolved uid 1000; got %q", got)
	}
}

func TestProfileDeclaresSharedKernelAndFragile(t *testing.T) {
	p := New(exec.NewFakeRunner(), "lever-jail").Profile()
	if p.SeparateKernel || !p.VersionFragile {
		t.Fatalf("orbstack profile wrong: %+v", p)
	}
}

func TestProfileFSBoundedByIsHonest(t *testing.T) {
	p := New(exec.NewFakeRunner(), "lever-jail").Profile()
	if !strings.Contains(p.FSBoundedBy, "/lever") {
		t.Fatalf("Profile.FSBoundedBy should mention /lever mount; got %q", p.FSBoundedBy)
	}
	if strings.Contains(p.FSBoundedBy, "NOT yet") {
		t.Fatalf("Profile.FSBoundedBy still contains stale 'NOT yet' wording; got %q", p.FSBoundedBy)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("orb -u root -m lever-jail iptables", exec.Result{})
	f.Script("orb -u root -m lever-jail ip6tables", exec.Result{})
	b := New(f, "lever-jail")

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

func TestTeardownDeletesMachineWhenPresent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb list", exec.Result{Stdout: "lever-jail running ubuntu\n"})
	f.Script("orb delete lever-jail", exec.Result{})
	if err := New(f, "lever-jail").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if f.Calls[len(f.Calls)-1].Name != "orb" || f.Calls[len(f.Calls)-1].Args[0] != "delete" {
		t.Fatalf("expected last call orb delete; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb list", exec.Result{Stdout: "\n"}) // no machines
	if err := New(f, "lever-jail").Teardown(context.Background()); err != nil {
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
			f := exec.NewFakeRunner()
			f.Script("orb version", exec.Result{Stdout: tc.stdout})
			ok, got, err := orbVersionAtLeast(context.Background(), f, 2, 1, 1)
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
	f := exec.NewFakeRunner()
	f.Script("orb version", exec.Result{Stdout: "Version: 2.1.0 (1990000)\n"})
	b := New(f, "lever-jail")

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
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail")
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
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	f.Script("go build", exec.Result{})
	f.Script("bash -c", exec.Result{})
	src := t.TempDir() // must exist for the stat check
	b := New(f, "lever-jail")

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
		if c.Name == "bash" && len(c.Args) >= 2 && c.Args[0] == "-c" {
			script := c.Args[1]
			if strings.Contains(script, "set -o pipefail") &&
				strings.Contains(script, "scion.tmp") &&
				strings.Contains(script, "mv") &&
				strings.Contains(script, "'lever-jail'") &&
				strings.Contains(script, "/usr/local/bin/scion") {
				sawInstall = true
			}
		}
	}
	if !sawBuild {
		t.Fatalf("expected go build for ./cmd/scion in %q; calls=%+v", src, f.Calls)
	}
	if !sawInstall {
		t.Fatalf("expected bash -c atomic scion install into jail; calls=%+v", f.Calls)
	}
}

func TestEnsureScionSkippedWhenEmpty(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, "lever-jail")

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
	f := exec.NewFakeRunner()
	scriptedMachine(f)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	b := New(f, "lever-jail")

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

func TestShellSingleQuote(t *testing.T) {
	if got := shellSingleQuote("ab"); got != "'ab'" {
		t.Errorf("shellSingleQuote(ab): want 'ab' got %q", got)
	}
	if got := shellSingleQuote("a'b"); got != `'a'\''b'` {
		t.Errorf(`shellSingleQuote(a'b): want 'a'\''b' got %q`, got)
	}
}

func TestEnsureUpRequiresProjectTree(t *testing.T) {
	f := exec.NewFakeRunner()
	// No `orb version` needed: ProjectTree guard fires before the preflight.
	b := New(f, "lever-jail")

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

func TestRunUserUIDAfterEnsureUp(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedMachine(f) // scripts whoami→leveruser, id -u→501
	b := New(f, "lever-jail")

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
