package orbstack

import (
	"context"
	"errors"
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
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
)

// TestProfileIsSingleSourced guards against re-hardcoding the profile: OrbStack's
// runtime Profile() must be the exported Profile value the registry publishes.
func TestProfileIsSingleSourced(t *testing.T) {
	want := Profile
	if got := New(proc.RealRunner{}, "m", common.Options{}).Profile(); got != want {
		t.Errorf("Profile() = %+v, want declared %+v", got, want)
	}
}

// The machine every test drives, and its guest transport prefixes.
const machine = "lever-jail"

var orbGuest = backendtest.Guest{
	Machine: machine,
	User:    "orb -m " + machine,
	Root:    "orb -u root -m " + machine,
	Alias:   "host.orb.internal",
}

// `orb list` answers for the machine states.
const (
	listRunning = machine + " running ubuntu\n"
	listStopped = machine + " stopped ubuntu\n"
	listAbsent  = "\n"
)

// scriptList scripts the machine listing EnsureUp/Stop/Teardown consult first.
func scriptList(f *proc.FakeRunner, out string) {
	f.Script("orb list", proc.Result{Stdout: out})
}

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := orbGuest.ClosedChain(backendtest.AhostsDual, true)
	b := New(r, machine, common.Options{})
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
	backendtest.AssertClosedChainKept(t, r, b.HostAliasV4())
}

func TestApplyEgressFlushesChainBeforeResolving(t *testing.T) {
	f := proc.NewFakeRunner()
	orbGuest.ScriptEgress(f, backendtest.AhostsDual)
	b := New(f, machine, common.Options{})
	if err := b.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	backendtest.AssertFlushPrecedesResolve(t, f, orbGuest.Alias)
}

// orbVersionScript scripts a successful `orb version` response for >= 2.1.1.
func orbVersionScript(f *proc.FakeRunner) {
	f.Script("orb version", proc.Result{Stdout: "Version: 2.2.1 (2020100)\n"})
}

// scriptedMachine scripts a fully up (running) machine: version, list,
// whoami/id -u, runtimes, arch probe (EnsureScion) and egress.
func scriptedMachine(f *proc.FakeRunner) {
	orbVersionScript(f)
	scriptList(f, listRunning) // machine already exists
	orbGuest.ScriptProvision(f, "501", backendtest.AhostsDual)
}

func TestEnsureUpIsIdempotentWhenMachineExists(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, machine, common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/Users/x/tree", AllowedPorts: []int{3305},
	})
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	// Must NOT call `orb create` when the machine already exists.
	backendtest.AssertNoSubcommand(t, f, "orb", "create")
}

func TestEnsureUpCreatesIsolatedMachineWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	orbVersionScript(f)
	scriptList(f, listAbsent)
	f.Script("orb create --isolated --mount", proc.Result{Stdout: "created\n"})
	orbGuest.ScriptProvision(f, "501", backendtest.AhostsDual)
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: machine, ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	want := "orb create --isolated --mount /Users/x/tree:/lever ubuntu " + machine
	if !f.Called(func(c proc.Call) bool { return c.Argv() == want }) {
		t.Fatalf("expected `%s`; calls=%+v", want, f.Calls)
	}
}

// --- ResolveRunUser: passive attach must resolve without provisioning. ---

func TestResolveRunUserResolvesWhenMachineRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	orbGuest.ScriptRunUser(f, "leveruser", "501")
	b := New(f, machine, common.Options{})

	if err := b.ResolveRunUser(context.Background()); err != nil {
		t.Fatalf("ResolveRunUser: %v", err)
	}
	if b.RunUser() != "leveruser" || b.RunUID() != "501" {
		t.Fatalf("run user/uid not resolved: user=%q uid=%q", b.RunUser(), b.RunUID())
	}
	// No provisioning calls: only the read-only list + whoami/id -u probes.
	for _, sub := range []string{"create", "iptables", "apt-get"} {
		if i := f.CallIndex(proc.ArgvContains(sub)); i >= 0 {
			t.Fatalf("ResolveRunUser must not provision anything; saw call: %s", f.Calls[i].Argv())
		}
	}
}

func TestResolveRunUserErrorsWhenMachineAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	b := New(f, machine, common.Options{})

	err := b.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the machine does not exist")
	}
	if !strings.Contains(err.Error(), machine) {
		t.Fatalf("error should name the machine; got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "orb", "create")
}

func TestResolveRunUserErrorsWhenMachineNotRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	b := New(f, machine, common.Options{})

	err := b.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the machine is not running")
	}
	if !strings.Contains(err.Error(), machine) {
		t.Fatalf("error should name the machine; got: %v", err)
	}
	for _, probe := range []string{"whoami", "id -u"} {
		if i := f.CallIndex(proc.ArgvContains(probe)); i >= 0 {
			t.Fatalf("must not attempt to resolve run user on a non-running machine: %s", f.Calls[i].Argv())
		}
	}
}

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := proc.NewFakeRunner()
	orbVersionScript(f)
	scriptList(f, listRunning)
	orbGuest.ScriptProvision(f, "1000", backendtest.AhostsDual) // non-default uid
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: machine, ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if got := b.DockerHost(); !strings.Contains(got, "/run/user/1000/") {
		t.Fatalf("DockerHost should reflect resolved uid 1000; got %q", got)
	}
}

func TestProfileDeclaresSharedKernelAndFragile(t *testing.T) {
	p := New(proc.NewFakeRunner(), machine, common.Options{}).Profile()
	if p.SeparateKernel || !p.VersionFragile {
		t.Fatalf("orbstack profile wrong: %+v", p)
	}
}

func TestProfileFSBoundedByIsHonest(t *testing.T) {
	p := New(proc.NewFakeRunner(), machine, common.Options{}).Profile()
	if !strings.Contains(p.FSBoundedBy, "/lever") {
		t.Fatalf("Profile.FSBoundedBy should mention /lever mount; got %q", p.FSBoundedBy)
	}
	if strings.Contains(p.FSBoundedBy, "NOT yet") {
		t.Fatalf("Profile.FSBoundedBy still contains stale 'NOT yet' wording; got %q", p.FSBoundedBy)
	}
}

func TestApplyEgressResolvesAliasAndAppliesRules(t *testing.T) {
	f := proc.NewFakeRunner()
	orbGuest.ScriptEgress(f, backendtest.AhostsDual)
	b := New(f, machine, common.Options{})

	if err := b.ApplyEgress(context.Background(), []int{3305}, false); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	backendtest.AssertEgressRules(t, f, "3305")
}

// --- Stop: power off, keep disk (distinct from Teardown). ---

func TestStopStopsMachineWhenListed(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script("orb stop "+machine, proc.Result{})
	if err := New(f, machine, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	backendtest.AssertLastSubcommand(t, f, "orb", "stop")
}

func TestStopIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	if err := New(f, machine, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop should be a no-op, got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "orb", "stop")
}

func TestStopOnAlreadyStoppedMachineIsHarmless(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	f.Script("orb stop "+machine, proc.Result{})
	if err := New(f, machine, common.Options{}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop on an already-stopped machine should be harmless, got: %v", err)
	}
}

// --- ensureMachine: a stopped machine is STARTED (so `up` resumes it), not
// treated as a no-op. ---

func TestEnsureMachineStartsStoppedMachine(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	f.Script("orb start "+machine, proc.Result{})
	f.Script(orbGuest.User+" true", proc.Result{})
	b := New(f, machine, common.Options{})

	if err := b.ensureMachine(context.Background(), "/Users/x/tree"); err != nil {
		t.Fatalf("ensureMachine: %v", err)
	}
	if !f.Called(proc.Subcommand("orb", "start")) {
		t.Fatal("expected `orb start` for a stopped machine")
	}
	// create must NOT be called for an already-existing (stopped) machine.
	backendtest.AssertNoSubcommand(t, f, "orb", "create")
}

func TestEnsureMachineRunningIsNoop(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	b := New(f, machine, common.Options{})

	if err := b.ensureMachine(context.Background(), "/Users/x/tree"); err != nil {
		t.Fatalf("ensureMachine: %v", err)
	}
	// Neither start nor create for an already-running machine.
	backendtest.AssertNoSubcommand(t, f, "orb", "start", "create")
}

func TestEnsureMachineStartTimesOutWhenUnreachable(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	f.Script("orb start "+machine, proc.Result{})
	// "orb -m lever-jail true" is deliberately unscripted: FakeRunner errors on
	// every unscripted call, simulating a machine that never becomes reachable.
	b := New(f, machine, common.Options{})
	b.probeAttempts, b.probeInterval = 2, time.Millisecond

	err := b.ensureMachine(context.Background(), "/Users/x/tree")
	if err == nil {
		t.Fatal("expected an error when the machine never becomes reachable after start")
	}
	if !strings.Contains(err.Error(), machine) {
		t.Fatalf("error should name the machine; got: %v", err)
	}
}

func TestTeardownDeletesMachineWhenPresent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script("orb delete "+machine, proc.Result{})
	if err := New(f, machine, common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	backendtest.AssertLastSubcommand(t, f, "orb", "delete")
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listAbsent)
	if err := New(f, machine, common.Options{}).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "orb", "delete")
}

// --- OrbStack version preflight tests ---

func TestOrbVersionAtLeast(t *testing.T) {
	backendtest.RunVersionCases(t, []backendtest.VersionCase{
		{Name: "2.2.1 >= 2.1.1 → ok", Stdout: "Version: 2.2.1 (2020100)\n", WantOK: true, WantGot: "2.2.1"},
		{Name: "2.1.1 >= 2.1.1 → ok (exact match)", Stdout: "Version: 2.1.1 (2000000)\n", WantOK: true, WantGot: "2.1.1"},
		{Name: "2.1.0 >= 2.1.1 → too old", Stdout: "Version: 2.1.0 (1990000)\n", WantOK: false, WantGot: "2.1.0"},
		{Name: "2.0.9 >= 2.1.1 → too old (minor mismatch)", Stdout: "Version: 2.0.9 (1900000)\n", WantOK: false, WantGot: "2.0.9"},
		{Name: "3.0.0 >= 2.1.1 → ok (major bump)", Stdout: "Version: 3.0.0 (9999999)\n", WantOK: true, WantGot: "3.0.0"},
		{Name: "1.9.9 >= 2.1.1 → too old (major too low)", Stdout: "Version: 1.9.9 (1000000)\n", WantOK: false, WantGot: "1.9.9"},
		{Name: "malformed output → error", Stdout: "orb: command not found\n", WantErr: true},
		{Name: "empty output → error", Stdout: "", WantErr: true},
	}, func(f *proc.FakeRunner, stdout string) (bool, string, error) {
		f.Script("orb version", proc.Result{Stdout: stdout})
		return common.VersionAtLeast(context.Background(), f, []string{"orb", "version"}, orbVersionRe, 2, 1, 1)
	})
}

func TestEnsureUpRejectsOldOrb(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb version", proc.Result{Stdout: "Version: 2.1.0 (1990000)\n"})
	b := New(f, machine, common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine,
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
	b := New(f, machine, common.Options{})
	if err := b.EnsureUp(context.Background(), backend.Config{MachineName: machine, ProjectTree: "/t", AllowedPorts: nil}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if !f.Called(proc.ArgvContains("apt-get install", "podman")) {
		t.Fatalf("expected podman install; calls=%+v", f.Calls)
	}
}

func TestEnsureScionBuildsAndInstalls(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	f.Script("go build", proc.Result{})
	f.Script("bash -c", proc.Result{})
	// The install now reads a digest marker first and records one after.
	f.Script(orbGuest.User+" sh -c", proc.Result{Code: 1})
	f.Script(orbGuest.Root+" sh -c", proc.Result{})
	src := t.TempDir() // must exist for the stat check
	backendtest.StageFakeBuildOutput(t, machine)
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/t", ScionSource: src,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	backendtest.AssertScionBuild(t, f, src, machine)
	// The install: root prefix, guest-side atomic script, binary on stdin.
	sawInstall := f.Called(func(c proc.Call) bool {
		if !c.HasPrefix("orb", "-u", "root", "-m", machine, "bash", "-c") {
			return false
		}
		script := c.Args[len(c.Args)-1]
		return strings.Contains(script, "scion.tmp") &&
			strings.Contains(script, "mv") &&
			strings.Contains(script, "/usr/local/bin/scion") &&
			c.Stdin == "fake-scion-"+machine
	})
	if !sawInstall {
		t.Fatalf("expected atomic scion install into jail via the root prefix; calls=%+v", f.Calls)
	}
}

func TestEnsureScionSkippedWhenEmpty(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/t", ScionSource: "",
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	// go build must NOT be called when ScionSource is empty.
	backendtest.AssertNoSubcommand(t, f, "go", "build")
	if i := f.CallIndex(func(c proc.Call) bool {
		return c.Name == "bash" && len(c.Args) >= 2 && strings.Contains(c.Args[1], "/usr/local/bin/scion")
	}); i >= 0 {
		t.Fatalf("scion install must NOT be called when ScionSource empty: %+v", f.Calls[i])
	}
}

func TestEnsureScionSourceMissing(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptedMachine(f)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	b := New(f, machine, common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/t", ScionSource: missing,
	})
	var srcErr *scionbin.SourceError
	if !errors.As(err, &srcErr) || srcErr.Path != missing {
		t.Fatalf("error should be a scion source error for %q; got: %v", missing, err)
	}
	// go build must NOT be called when the source is missing (stat short-circuits).
	backendtest.AssertNoSubcommand(t, f, "go", "build")
}

func TestEnsureUpRequiresProjectTree(t *testing.T) {
	f := proc.NewFakeRunner()
	// No `orb version` needed: ProjectTree guard fires before the preflight.
	b := New(f, machine, common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine,
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
	backendtest.Guest{User: "orb -m " + machine}.ScriptRunUser(f, "stephen", "501")
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
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/Users/x/tree",
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
	bin := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETExec)

	f := proc.NewFakeRunner()
	scriptedMachine(f)
	orbGuest.ScriptArch(f, "aarch64")
	f.Script(orbGuest.User+" /usr/bin/sha256sum", proc.Result{Code: 1}) // not installed yet
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/t", ScionBinary: bin,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	if i := f.CallIndex(func(c proc.Call) bool { return c.Name == "go" }); i >= 0 {
		t.Fatalf("binary mode must never invoke go through EnsureUp; got %+v", f.Calls[i])
	}
	installed := f.Called(func(c proc.Call) bool {
		return len(c.Args) > 1 && c.Args[len(c.Args)-2] == "-c" && strings.Contains(c.Args[len(c.Args)-1], "cat > ")
	})
	if !installed {
		t.Fatal("EnsureUp with only ScionBinary set installed nothing")
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
	scriptedMachine(f)
	orbGuest.ScriptScionInstall(t, f)
	b := New(f, machine, common.Options{})

	err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/Users/x/tree",
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
	scriptedMachine(f)
	orbGuest.ScriptScionInstall(t, f)
	b := New(f, machine, common.Options{})

	if err := b.EnsureUp(context.Background(), backend.Config{
		MachineName: machine, ProjectTree: "/Users/x/tree",
		ScionSource: backendtest.FakeScionCheckout(t),
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	backendtest.AssertNoNodeTooling(t, f)
}
