package lima

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/webassets"
)

// TestProfileIsSingleSourced guards against re-hardcoding the profile: Lima's
// runtime Profile() must be the exported Profile value the registry publishes.
func TestProfileIsSingleSourced(t *testing.T) {
	want := Profile
	if got := New(proc.NewFakeRunner(), "lever-x", common.Options{}).Profile(); got != want {
		t.Errorf("Profile() = %+v, want declared %+v", got, want)
	}
}

func TestProfileDeclaresSeparateKernel(t *testing.T) {
	p := New(proc.NewFakeRunner(), "lever-x", common.Options{}).Profile()
	if !p.SeparateKernel {
		t.Fatalf("lima profile should declare SeparateKernel=true (own VM kernel); got %+v", p)
	}
}

// The VM every test drives, and its guest transport prefixes.
const (
	vm   = "lever-x"
	tree = "/Users/x/tree"
)

var limaGuest = backendtest.Guest{
	Machine: vm,
	User:    "limactl shell " + vm,
	Root:    "limactl shell " + vm + " sudo",
	Alias:   "host.lima.internal",
}

// `limactl list --format '{{.Name}} {{.Status}}'` answers for the VM states.
const (
	listRunning = vm + " Running\n"
	listStopped = vm + " Stopped\n"
	listAbsent  = ""
)

// limaVersionScript scripts a successful `limactl --version` response for the
// installed dev version (verified live: `limactl version 2.1.3`).
func limaVersionScript(f *proc.FakeRunner) {
	f.Script("limactl --version", proc.Result{Stdout: "limactl version 2.1.3\n"})
}

// scriptList scripts the VM listing EnsureUp/Stop/Teardown consult first.
func scriptList(f *proc.FakeRunner, out string) {
	f.Script("limactl list --format", proc.Result{Stdout: out})
}

// scriptLifecycle scripts the VM lifecycle verbs (create/start) and the
// realized-config verify that follows create.
func scriptLifecycle(f *proc.FakeRunner) {
	f.Script("limactl create --name="+vm+" --tty=false", proc.Result{Stdout: "created\n"})
	scriptRealizedConfig(f, vm, matchingRealizedConfigJSON(vm, tree))
	f.Script("limactl start --tty=false "+vm, proc.Result{Stdout: "started\n"})
}

// scriptedVM scripts a fully up (Running) VM: version, list, whoami/id -u,
// runtimes, egress. Used by tests that only care about post-EnsureUp state.
func scriptedVM(f *proc.FakeRunner) {
	limaVersionScript(f)
	scriptList(f, listRunning)
	scriptRealizedConfig(f, vm, matchingRealizedConfigJSON(vm, tree))
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsDual)
}

// callIndex returns the index of the first "limactl" call whose leading args
// exactly match want, or -1.
func callIndex(f *proc.FakeRunner, want ...string) int {
	return f.CallIndex(proc.ArgvPrefix("limactl", want...))
}

// --- Test 1: fresh host — version, list, create (template tmpfile), start,
// whoami/id -u, runtimes, egress, in that order. ---

func TestEnsureUpFreshHostFullSequence(t *testing.T) {
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listAbsent)
	scriptLifecycle(f)
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsDual)
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree, AllowedPorts: []int{3305},
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	versionIdx := callIndex(f, "--version")
	listIdx := callIndex(f, "list", "--format", "{{.Name}} {{.Status}}")
	createIdx := callIndex(f, "create", "--name="+vm, "--tty=false")
	startIdx := callIndex(f, "start", "--tty=false", vm)
	whoamiIdx := callIndex(f, "shell", vm, "whoami")
	idUIdx := callIndex(f, "shell", vm, "id", "-u")

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
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected tmpfile %q to be removed after EnsureUp, stat err=%v", tmpPath, err)
	}
}

// --- Test 2: idempotency — Running VM → no create, no start. ---

func TestEnsureUpIsIdempotentWhenRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedVM(f) // "lever-x Running"
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	// Neither create nor start: the VM is already Running.
	backendtest.AssertNoSubcommand(t, f, "limactl", "create", "start")
}

// --- Test 3: Stopped VM → start but no create. ---

func TestEnsureUpStartsStoppedVMWithoutCreate(t *testing.T) {
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listStopped)
	scriptLifecycle(f)
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsV4)
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if !f.Called(proc.Subcommand("limactl", "start")) {
		t.Fatal("expected `limactl start` for a Stopped VM")
	}
	// create must NOT be called for an already-existing (Stopped) VM.
	backendtest.AssertNoSubcommand(t, f, "limactl", "create")
}

// --- Test 4: version preflight — Lima >= 2.0.0 required. ---

func TestEnsureUpRejectsOldLima(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl --version", proc.Result{Stdout: "limactl version 0.23.0\n"})
	l := New(f, vm, common.Options{})

	err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
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
			f := proc.NewFakeRunner()
			f.Script("limactl --version", proc.Result{Stdout: tc.stdout})
			ok, got, err := common.VersionAtLeast(context.Background(), f, []string{"limactl", "--version"}, limaVersionRe, 2, 0, 0)
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

// --- ResolveRunUser: passive attach must resolve without provisioning. ---

func TestResolveRunUserResolvesWhenVMRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	limaGuest.ScriptRunUser(f, "leveruser", "501")
	l := New(f, vm, common.Options{})

	if err := l.ResolveRunUser(context.Background()); err != nil {
		t.Fatalf("ResolveRunUser: %v", err)
	}
	if l.RunUser() != "leveruser" || l.RunUID() != "501" {
		t.Fatalf("run user/uid not resolved: user=%q uid=%q", l.RunUser(), l.RunUID())
	}
	// No provisioning calls: only the read-only list + whoami/id -u probes.
	backendtest.AssertNoSubcommand(t, f, "limactl", "create", "start")
}

func TestResolveRunUserErrorsWhenVMAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	l := New(f, vm, common.Options{})

	err := l.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the VM does not exist")
	}
	if !strings.Contains(err.Error(), vm) {
		t.Fatalf("error should name the VM; got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "limactl", "create")
}

func TestResolveRunUserErrorsWhenVMStopped(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	l := New(f, vm, common.Options{})

	err := l.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the VM is not running")
	}
	if !strings.Contains(err.Error(), vm) {
		t.Fatalf("error should name the VM; got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "limactl", "start")
}

// --- Test 5: teardown. ---

func TestTeardownDeletesVMWhenPresent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script("limactl delete --force "+vm, proc.Result{})
	if err := New(f, vm, common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if last := f.Calls[len(f.Calls)-1]; !last.Subcommand("limactl", "delete") {
		t.Fatalf("expected last call limactl delete --force; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	if err := New(f, vm, common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "limactl", "delete")
}

// --- Stop: power off, keep disk (distinct from Teardown). ---

func TestStopStopsVMWhenListed(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script("limactl stop "+vm, proc.Result{})
	if err := New(f, vm, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if last := f.Calls[len(f.Calls)-1]; !last.Subcommand("limactl", "stop") {
		t.Fatalf("expected last call limactl stop lever-x; got %+v", f.Calls)
	}
}

func TestStopIsNoopWhenVMAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	if err := New(f, vm, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop should be a no-op, got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "limactl", "stop")
}

func TestStopOnAlreadyStoppedVMIsHarmless(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	f.Script("limactl stop "+vm, proc.Result{})
	if err := New(f, vm, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop on an already-stopped VM should be harmless, got: %v", err)
	}
}

// --- Test 6: DockerHost — default before EnsureUp, resolved uid after. ---

func TestDockerHostDefaultBeforeEnsureUp(t *testing.T) {
	l := New(proc.NewFakeRunner(), "lever-x", common.Options{})
	if got := l.DockerHost(); got != "unix:///run/user/501/docker.sock" {
		t.Fatalf("DockerHost() before EnsureUp = %q", got)
	}
}

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listRunning)
	scriptRealizedConfig(f, vm, matchingRealizedConfigJSON(vm, tree))
	limaGuest.ScriptProvision(f, "1000", backendtest.AhostsV4) // non-default uid
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := l.DockerHost(); !strings.Contains(got, "/run/user/1000/") {
		t.Fatalf("DockerHost should reflect resolved uid 1000; got %q", got)
	}
}

// --- Test 7: HostToolAlias + JailPrefix. ---

func TestHostToolAliasAndJailPrefix(t *testing.T) {
	if got := New(proc.NewFakeRunner(), "lever-x", common.Options{}).HostToolAlias(); got != "host.lima.internal" {
		t.Fatalf("HostToolAlias() = %q", got)
	}
	if got := JailPrefix("v"); !reflect.DeepEqual(got, []string{"limactl", "shell", "v"}) {
		t.Fatalf("JailPrefix(v) = %v", got)
	}
}

// resolvedLima returns a backend whose run user is resolved (leveruser/501)
// through the same probes EnsureUp issues, without provisioning anything.
func resolvedLima(t *testing.T, f *proc.FakeRunner) *Lima {
	t.Helper()
	limaGuest.ScriptRunUser(f, "leveruser", "501")
	l := New(f, vm, common.Options{})
	if err := l.ReadRunUser(context.Background()); err != nil {
		t.Fatalf("ReadRunUser: %v", err)
	}
	f.Calls = nil
	return l
}

func TestJailTransportMethods(t *testing.T) {
	l := resolvedLima(t, proc.NewFakeRunner())

	if l.JailRunner() == nil {
		t.Fatal("JailRunner() = nil")
	}
	attach := l.AttachArgv([]string{"scion", "attach"})
	if attach[0] != "limactl" || attach[len(attach)-1] != "attach" {
		t.Fatalf("AttachArgv = %v", attach)
	}
}

// --- F1 guard: per-backend exact-argv assertions for the image-op / jail-
// transport forwarders. Before F1 these had no per-backend argv assertion
// (TestJailTransportMethods only checked non-nil), so a prefix mis-wiring in
// the shared base — lima's jail prefix is the static ["limactl","shell",vm] —
// would only fail at runtime. LoadImage/ImageLoaded/PruneJailImages exec docker
// directly (unobservable offline) but share jailPrefix() with JailRunner and
// AttachArgv, so pinning those two transitively guards them; InstallGuestBinary
// is pinned directly through the root transport. ---

func TestInstallGuestBinaryArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl", proc.Result{})
	local := filepath.Join(t.TempDir(), "lever-agent")
	if err := os.WriteFile(local, []byte("agent-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := New(f, "lever-x", common.Options{})
	if err := l.InstallGuestBinary(context.Background(), local, "/usr/local/bin/lever-agent"); err != nil {
		t.Fatalf("InstallGuestBinary: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 host call, got %d", len(f.Calls))
	}
	c := f.Calls[0]
	// The root transport prefix must be wired through verbatim, with the
	// binary on stdin of the guest-side script.
	if c.Name != "limactl" || !reflect.DeepEqual(c.Args[:5], []string{"shell", "lever-x", "sudo", "bash", "-c"}) {
		t.Fatalf("root prefix mis-wired: %s %v", c.Name, c.Args)
	}
	if c.Stdin != "agent-bytes" {
		t.Fatalf("binary not streamed on stdin: %q", c.Stdin)
	}
}

func TestJailRunnerArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	l := resolvedLima(t, f)
	f.Script("limactl", proc.Result{Stdout: "ok\n"})
	if _, err := l.JailRunner().Run(context.Background(), nil, "true"); err != nil {
		t.Fatalf("JailRunner run: %v", err)
	}
	c := f.Calls[len(f.Calls)-1]
	// jail.Runner uses prefix[0] as the host command and prefix[1:]+env as args:
	// `limactl shell lever-x env <jailenv...> true`.
	if c.Name != "limactl" {
		t.Fatalf("host command = %q, want limactl", c.Name)
	}
	if wantPrefix := []string{"shell", "lever-x", "env"}; !reflect.DeepEqual(c.Args[:3], wantPrefix) {
		t.Fatalf("jail prefix mis-wired: %v", c.Args[:3])
	}
	if c.Args[len(c.Args)-1] != "true" {
		t.Fatalf("inner command not last: %v", c.Args)
	}
}

func TestAttachArgvFullPrefix(t *testing.T) {
	l := resolvedLima(t, proc.NewFakeRunner())
	attach := l.AttachArgv([]string{"scion", "attach"})
	if wantPrefix := []string{"limactl", "shell", "lever-x", "env"}; !reflect.DeepEqual(attach[:4], wantPrefix) {
		t.Fatalf("attach prefix mis-wired: %v", attach[:4])
	}
	if last2 := attach[len(attach)-2:]; !reflect.DeepEqual(last2, []string{"scion", "attach"}) {
		t.Fatalf("inner command not trailing: %v", last2)
	}
}

// --- Additional coverage mirroring orbstack's suite. ---

func TestEnsureUpRequiresProjectTree(t *testing.T) {
	f := proc.NewFakeRunner()
	// No `limactl --version` needed: the ProjectTree guard fires before the preflight.
	l := New(f, "lever-x", common.Options{})

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
	f := proc.NewFakeRunner()
	f.Script(limaGuest.User+" getent ahosts "+limaGuest.Alias, proc.Result{Stdout: "" +
		"fd07:b51a:cc66:f0::fe STREAM host.lima.internal\n" +
		"0.250.250.254   STREAM \n"})
	l := New(f, vm, common.Options{})

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
	f := proc.NewFakeRunner()
	limaGuest.ScriptEgress(f, backendtest.AhostsDual)
	l := New(f, vm, common.Options{})

	if err := l.ApplyEgress(context.Background(), []int{3305}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	backendtest.AssertEgressRules(t, f, "3305")
	if l.HostAliasV4() != backendtest.HostAliasV4 {
		t.Fatalf("HostAliasV4() = %q", l.HostAliasV4())
	}
}

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := limaGuest.ClosedChain(backendtest.AhostsDual, true)
	l := New(r, vm, common.Options{})
	// A prior apply closed the chain; the re-apply below must hit the skip path.
	if err := l.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("first ApplyEgress: %v", err)
	}
	r.Open, r.Flushed, r.Resolved = false, false, false

	if err := l.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	// I2: an already-closed chain must NOT be flushed or re-resolved — that
	// would briefly open egress for a running agent.
	if r.Flushed {
		t.Fatal("must not flush LEVER_EGRESS when the closed posture is already active (would open egress)")
	}
	if r.Resolved {
		t.Fatal("must not re-resolve the alias (DNS) when already closed — read it from the chain")
	}
	if l.HostAliasV4() != backendtest.HostAliasV4 {
		t.Fatalf("alias should be read from the existing chain, got %q", l.HostAliasV4())
	}
}

// EnsureUp must thread Config.ScionWebUI into the guest's ScionSpec. The
// literal now lives once in common.Base.Provision, but each backend still pins
// it end to end (and asserts the negative) so a backend that stops calling
// Provision, or calls it with the wrong config, is caught here.
func TestEnsureUpBuildsWebAssetsWhenAsked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	f := proc.NewFakeRunner()
	scriptedVM(f)
	limaGuest.ScriptScionInstall(t, f)
	l := New(f, vm, common.Options{})

	err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
		ScionSource: backendtest.FakeScionCheckout(t), ScionWebUI: true,
	})
	// node is deliberately not scripted: the web build stops at its toolchain
	// probe, which is only reachable if ScionWebUI was threaded through.
	if !errors.Is(err, webassets.ErrNodeToolchain) {
		t.Fatalf("want the web-asset build attempted; got %v", err)
	}
}

func TestEnsureUpSkipsWebAssetsByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	f := proc.NewFakeRunner()
	scriptedVM(f)
	limaGuest.ScriptScionInstall(t, f)
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
		ScionSource: backendtest.FakeScionCheckout(t),
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	backendtest.AssertNoNodeTooling(t, f)
}
