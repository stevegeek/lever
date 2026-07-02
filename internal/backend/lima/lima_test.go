package lima

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

// TestProfileIsSingleSourced guards against re-hardcoding the profile: Lima's
// runtime Profile() must be the same value the guarantee matrix declares.
func TestProfileIsSingleSourced(t *testing.T) {
	want, ok := backend.ProfileFor("lima")
	if !ok {
		t.Fatal("backend.Candidates is missing lima")
	}
	if got := New(exec.NewFakeRunner(), "lever-x").Profile(); got != want {
		t.Errorf("Profile() = %+v, want declared %+v", got, want)
	}
}

func TestProfileDeclaresSeparateKernel(t *testing.T) {
	p := New(exec.NewFakeRunner(), "lever-x").Profile()
	if !p.SeparateKernel {
		t.Fatalf("lima profile should declare SeparateKernel=true (own VM kernel); got %+v", p)
	}
}

// limaVersionScript scripts a successful `limactl --version` response for the
// installed dev version (verified live: `limactl version 2.1.3`).
func limaVersionScript(f *exec.FakeRunner) {
	f.Script("limactl --version", exec.Result{Stdout: "limactl version 2.1.3\n"})
}

// scriptedVM scripts a fully up (Running) VM: version, list, whoami/id -u,
// runtimes, egress. Used by tests that only care about post-EnsureUp state.
func scriptedVM(f *exec.FakeRunner) {
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: "lever-x Running\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl shell lever-x whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", exec.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x uname -m", exec.Result{Stdout: "arm64\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
}

// callIndex returns the index of the first "limactl" call whose leading args
// exactly match want, or -1.
func callIndex(calls []exec.Call, want []string) int {
	for i, c := range calls {
		if c.Name != "limactl" || len(c.Args) < len(want) {
			continue
		}
		match := true
		for j, w := range want {
			if c.Args[j] != w {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// --- Test 1: fresh host — version, list, create (template tmpfile), start,
// whoami/id -u, runtimes, egress, in that order. ---

func TestEnsureUpFreshHostFullSequence(t *testing.T) {
	f := exec.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: ""}) // no VMs yet
	f.Script("limactl create --name=lever-x --tty=false", exec.Result{Stdout: "created\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl start --tty=false lever-x", exec.Result{Stdout: "started\n"})
	f.Script("limactl shell lever-x whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", exec.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree", AllowedPorts: []int{3305},
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	versionIdx := callIndex(f.Calls, []string{"--version"})
	listIdx := callIndex(f.Calls, []string{"list", "--format", "{{.Name}} {{.Status}}"})
	createIdx := callIndex(f.Calls, []string{"create", "--name=lever-x", "--tty=false"})
	startIdx := callIndex(f.Calls, []string{"start", "--tty=false", "lever-x"})
	whoamiIdx := callIndex(f.Calls, []string{"shell", "lever-x", "whoami"})
	idUIdx := callIndex(f.Calls, []string{"shell", "lever-x", "id", "-u"})

	for name, i := range map[string]int{
		"--version": versionIdx, "list": listIdx, "create": createIdx,
		"start": startIdx, "whoami": whoamiIdx, "id -u": idUIdx,
	} {
		if i < 0 {
			t.Fatalf("expected a %s call; calls=%+v", name, f.Calls)
		}
	}
	if !(versionIdx < listIdx && listIdx < createIdx && createIdx < startIdx && startIdx < whoamiIdx && whoamiIdx < idUIdx) {
		t.Fatalf("argv sequence out of order: version=%d list=%d create=%d start=%d whoami=%d id-u=%d",
			versionIdx, listIdx, createIdx, startIdx, whoamiIdx, idUIdx)
	}

	// The create call's tmpfile argument must be removed after EnsureUp.
	createCall := f.Calls[createIdx]
	tmpPath := createCall.Args[len(createCall.Args)-1]
	if !strings.Contains(tmpPath, "lever-lima-") {
		t.Fatalf("create tmpfile path should be under a lever-lima- prefix, got %q", tmpPath)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected tmpfile %q to be removed after EnsureUp, stat err=%v", tmpPath, err)
	}
}

// --- Test 2: idempotency — Running VM → no create, no start. ---

func TestEnsureUpIsIdempotentWhenRunning(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedVM(f) // "lever-x Running"
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree",
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name != "limactl" || len(c.Args) == 0 {
			continue
		}
		if c.Args[0] == "create" {
			t.Fatalf("create called though VM is Running: %+v", c)
		}
		if c.Args[0] == "start" {
			t.Fatalf("start called though VM is already Running: %+v", c)
		}
	}
}

// --- Test 3: Stopped VM → start but no create. ---

func TestEnsureUpStartsStoppedVMWithoutCreate(t *testing.T) {
	f := exec.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: "lever-x Stopped\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl start --tty=false lever-x", exec.Result{})
	f.Script("limactl shell lever-x whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", exec.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree",
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	var sawStart, sawCreate bool
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 {
			if c.Args[0] == "start" {
				sawStart = true
			}
			if c.Args[0] == "create" {
				sawCreate = true
			}
		}
	}
	if !sawStart {
		t.Fatal("expected `limactl start` for a Stopped VM")
	}
	if sawCreate {
		t.Fatal("create must NOT be called for an already-existing (Stopped) VM")
	}
}

// --- Test 4: version preflight — Lima >= 2.0.0 required. ---

func TestEnsureUpRejectsOldLima(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("limactl --version", exec.Result{Stdout: "limactl version 0.23.0\n"})
	l := New(f, "lever-x")

	err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree",
	})
	if err == nil {
		t.Fatal("expected error for Lima 0.23.0, got nil")
	}
	if !strings.Contains(err.Error(), "Lima") || !strings.Contains(err.Error(), "2.0.0") {
		t.Fatalf("error should mention Lima >= 2.0.0; got: %v", err)
	}
	if !strings.Contains(err.Error(), "0.23.0") {
		t.Fatalf("error should show the found version; got: %v", err)
	}
}

func TestLimaVersionAtLeast(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantOK  bool
		wantErr bool
		wantGot string
	}{
		{name: "2.1.3 >= 2.0.0 → ok", stdout: "limactl version 2.1.3\n", wantOK: true, wantGot: "2.1.3"},
		{name: "2.0.0 >= 2.0.0 → ok (exact match)", stdout: "limactl version 2.0.0\n", wantOK: true, wantGot: "2.0.0"},
		{name: "1.9.9 >= 2.0.0 → too old", stdout: "limactl version 1.9.9\n", wantOK: false, wantGot: "1.9.9"},
		{name: "3.0.0 >= 2.0.0 → ok (major bump)", stdout: "limactl version 3.0.0\n", wantOK: true, wantGot: "3.0.0"},
		{name: "malformed output → error", stdout: "limactl: command not found\n", wantErr: true},
		{name: "empty output → error", stdout: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("limactl --version", exec.Result{Stdout: tc.stdout})
			ok, got, err := limaVersionAtLeast(context.Background(), f, 2, 0, 0)
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

// --- Test 5: teardown. ---

func TestTeardownDeletesVMWhenPresent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("limactl list --format", exec.Result{Stdout: "lever-x Running\n"})
	f.Script("limactl delete --force lever-x", exec.Result{})
	if err := New(f, "lever-x").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	last := f.Calls[len(f.Calls)-1]
	if last.Name != "limactl" || last.Args[0] != "delete" {
		t.Fatalf("expected last call limactl delete --force; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("limactl list --format", exec.Result{Stdout: ""})
	if err := New(f, "lever-x").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 && c.Args[0] == "delete" {
			t.Fatalf("delete must NOT be called when VM absent: %+v", f.Calls)
		}
	}
}

// --- Test 6: DockerHost — default before EnsureUp, resolved uid after. ---

func TestDockerHostDefaultBeforeEnsureUp(t *testing.T) {
	l := New(exec.NewFakeRunner(), "lever-x")
	if got := l.DockerHost(); got != "unix:///run/user/501/docker.sock" {
		t.Fatalf("DockerHost() before EnsureUp = %q", got)
	}
}

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := exec.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: "lever-x Running\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl shell lever-x whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", exec.Result{Stdout: "1000\n"}) // non-default uid
	f.Script("limactl shell lever-x bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: "lever-x", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := l.DockerHost(); !strings.Contains(got, "/run/user/1000/") {
		t.Fatalf("DockerHost should reflect resolved uid 1000; got %q", got)
	}
}

// --- Test 7: HostToolAlias + JailPrefix. ---

func TestHostToolAliasAndJailPrefix(t *testing.T) {
	if got := New(exec.NewFakeRunner(), "lever-x").HostToolAlias(); got != "host.lima.internal" {
		t.Fatalf("HostToolAlias() = %q", got)
	}
	if got := JailPrefix("v"); !reflect.DeepEqual(got, []string{"limactl", "shell", "v"}) {
		t.Fatalf("JailPrefix(v) = %v", got)
	}
}

func TestJailTransportMethods(t *testing.T) {
	l := New(exec.NewFakeRunner(), "lever-x")
	l.runUID = "501"

	if l.JailRunner() == nil {
		t.Fatal("JailRunner() = nil")
	}
	attach := l.AttachArgv([]string{"scion", "attach"})
	if attach[0] != "limactl" || attach[len(attach)-1] != "attach" {
		t.Fatalf("AttachArgv = %v", attach)
	}
}

// --- Additional coverage mirroring orbstack's suite. ---

func TestEnsureUpRequiresProjectTree(t *testing.T) {
	f := exec.NewFakeRunner()
	// No `limactl --version` needed: the ProjectTree guard fires before the preflight.
	l := New(f, "lever-x")

	err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x",
		ProjectTree: "", // empty
	})
	if err == nil {
		t.Fatal("expected error for empty ProjectTree, got nil")
	}
	if !strings.Contains(err.Error(), "ProjectTree") {
		t.Fatalf("error should mention ProjectTree; got: %v", err)
	}
}

func TestResolveHostAliasParsesBothFamilies(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "" +
		"fd07:b51a:cc66:f0::fe STREAM host.lima.internal\n" +
		"0.250.250.254   STREAM \n"})
	l := New(f, "lever-x")

	v4, v6, err := l.resolveHostAlias(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v4 != "0.250.250.254" {
		t.Fatalf("v4=%q", v4)
	}
	if v6 != "fd07:b51a:cc66:f0::fe" {
		t.Fatalf("v6=%q", v6)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(f, "lever-x")

	if err := l.ApplyEgress(context.Background(), []int{3305}, false); err != nil {
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
	if l.HostAliasV4() != "0.250.250.254" {
		t.Fatalf("HostAliasV4() = %q", l.HostAliasV4())
	}
}

// closedChainRunner returns an ACTIVE closed LEVER_EGRESS chain for
// `iptables -S` and records whether the chain was flushed or the alias
// re-resolved. It intercepts those substrings in a fixed switch order BEFORE
// falling through to the embedded FakeRunner, so results are deterministic —
// FakeRunner.Script matches by HasPrefix over its (randomized-iteration-order)
// map, so two overlapping keys like "...iptables -S LEVER_EGRESS" and the
// shorter generic "...iptables" are both valid prefixes of the same call, and
// which one "wins" is nondeterministic. Mirrors orbstack_test.go's
// closedChainRunner exactly (see orbstack_test.go:28-79).
type closedChainRunner struct {
	*exec.FakeRunner
	flushed, resolved bool
}

func (r *closedChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	argv := strings.Join(args, " ")
	if name == "limactl" {
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
	r.Script("limactl shell lever-x sudo iptables", exec.Result{})
	r.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(r, "lever-x")
	// A prior apply resolved a v6 alias; the skip path parses only v4 from the
	// live chain, so a re-apply that hits the skip must leave a prior
	// aliasV6 untouched rather than zeroing it.
	l.aliasV6 = "fd07::fe"

	if err := l.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	// I2: an already-closed chain must NOT be flushed or re-resolved — that
	// would briefly open egress for a running agent.
	if r.flushed {
		t.Fatal("must not flush LEVER_EGRESS when the closed posture is already active (would open egress)")
	}
	if r.resolved {
		t.Fatal("must not re-resolve the alias (DNS) when already closed — read it from the chain")
	}
	if l.HostAliasV4() != "0.250.250.254" {
		t.Fatalf("alias should be read from the existing chain, got %q", l.HostAliasV4())
	}
	if l.aliasV6 != "fd07::fe" {
		t.Fatalf("skip path must not clobber a prior aliasV6; got %q", l.aliasV6)
	}
}
