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
)

// TestProfileIsSingleSourced guards against re-hardcoding the profile: Lima's
// runtime Profile() must be the same value the guarantee matrix declares.
func TestProfileIsSingleSourced(t *testing.T) {
	want, ok := backend.ProfileFor("lima")
	if !ok {
		t.Fatal("backend.Candidates is missing lima")
	}
	if got := New(proc.NewFakeRunner(), "lever-x").Profile(); got != want {
		t.Errorf("Profile() = %+v, want declared %+v", got, want)
	}
}

func TestProfileDeclaresSeparateKernel(t *testing.T) {
	p := New(proc.NewFakeRunner(), "lever-x").Profile()
	if !p.SeparateKernel {
		t.Fatalf("lima profile should declare SeparateKernel=true (own VM kernel); got %+v", p)
	}
}

// limaVersionScript scripts a successful `limactl --version` response for the
// installed dev version (verified live: `limactl version 2.1.3`).
func limaVersionScript(f *proc.FakeRunner) {
	f.Script("limactl --version", proc.Result{Stdout: "limactl version 2.1.3\n"})
}

// scriptedVM scripts a fully up (Running) VM: version, list, whoami/id -u,
// runtimes, egress. Used by tests that only care about post-EnsureUp state.
func scriptedVM(f *proc.FakeRunner) {
	limaVersionScript(f)
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Running\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl shell lever-x whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", proc.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x uname -m", proc.Result{Stdout: "arm64\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", proc.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
}

// callIndex returns the index of the first "limactl" call whose leading args
// exactly match want, or -1.
func callIndex(calls []proc.Call, want []string) int {
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
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", proc.Result{Stdout: ""}) // no VMs yet
	f.Script("limactl create --name=lever-x --tty=false", proc.Result{Stdout: "created\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl start --tty=false lever-x", proc.Result{Stdout: "started\n"})
	f.Script("limactl shell lever-x whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", proc.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", proc.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
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
	if _, err := os.Stat(tmpPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected tmpfile %q to be removed after EnsureUp, stat err=%v", tmpPath, err)
	}
}

// --- Test 2: idempotency — Running VM → no create, no start. ---

func TestEnsureUpIsIdempotentWhenRunning(t *testing.T) {
	f := proc.NewFakeRunner()
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
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Stopped\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl start --tty=false lever-x", proc.Result{})
	f.Script("limactl shell lever-x whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", proc.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", proc.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
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
	f := proc.NewFakeRunner()
	f.Script("limactl --version", proc.Result{Stdout: "limactl version 0.23.0\n"})
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
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Running\n"})
	f.Script("limactl shell lever-x whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", proc.Result{Stdout: "501\n"})
	l := New(f, "lever-x")

	if err := l.ResolveRunUser(context.Background()); err != nil {
		t.Fatalf("ResolveRunUser: %v", err)
	}
	if l.RunUser() != "leveruser" || l.RunUID() != "501" {
		t.Fatalf("run user/uid not resolved: user=%q uid=%q", l.RunUser(), l.RunUID())
	}
	// No provisioning calls: only the read-only list + whoami/id -u probes.
	for _, c := range f.Calls {
		if c.Name != "limactl" || len(c.Args) == 0 {
			continue
		}
		if c.Args[0] == "create" || c.Args[0] == "start" {
			t.Fatalf("ResolveRunUser must not provision anything; saw call: %+v", c)
		}
	}
}

func TestResolveRunUserErrorsWhenVMAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: ""}) // no VMs
	l := New(f, "lever-x")

	err := l.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the VM does not exist")
	}
	if !strings.Contains(err.Error(), "lever-x") {
		t.Fatalf("error should name the VM; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 && c.Args[0] == "create" {
			t.Fatalf("create must NOT be called by ResolveRunUser: %+v", c)
		}
	}
}

func TestResolveRunUserErrorsWhenVMStopped(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Stopped\n"})
	l := New(f, "lever-x")

	err := l.ResolveRunUser(context.Background())
	if err == nil {
		t.Fatal("expected error when the VM is not running")
	}
	if !strings.Contains(err.Error(), "lever-x") {
		t.Fatalf("error should name the VM; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name != "limactl" || len(c.Args) == 0 {
			continue
		}
		if c.Args[0] == "start" {
			t.Fatalf("start must NOT be called by ResolveRunUser: %+v", c)
		}
	}
}

// --- Test 5: teardown. ---

func TestTeardownDeletesVMWhenPresent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Running\n"})
	f.Script("limactl delete --force lever-x", proc.Result{})
	if err := New(f, "lever-x").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	last := f.Calls[len(f.Calls)-1]
	if last.Name != "limactl" || last.Args[0] != "delete" {
		t.Fatalf("expected last call limactl delete --force; got %+v", f.Calls)
	}
}

func TestTeardownIsNoopWhenAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: ""})
	if err := New(f, "lever-x").Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 && c.Args[0] == "delete" {
			t.Fatalf("delete must NOT be called when VM absent: %+v", f.Calls)
		}
	}
}

// --- Stop: power off, keep disk (distinct from Teardown). ---

func TestStopStopsVMWhenListed(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Running\n"})
	f.Script("limactl stop lever-x", proc.Result{})
	if err := New(f, "lever-x").Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	last := f.Calls[len(f.Calls)-1]
	if last.Name != "limactl" || len(last.Args) == 0 || last.Args[0] != "stop" {
		t.Fatalf("expected last call limactl stop lever-x; got %+v", f.Calls)
	}
}

func TestStopIsNoopWhenVMAbsent(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: ""}) // no VMs
	if err := New(f, "lever-x").Stop(context.Background()); err != nil {
		t.Fatalf("Stop should be a no-op, got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 && c.Args[0] == "stop" {
			t.Fatalf("stop must NOT be called when VM absent: %+v", f.Calls)
		}
	}
}

func TestStopOnAlreadyStoppedVMIsHarmless(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Stopped\n"})
	f.Script("limactl stop lever-x", proc.Result{})
	if err := New(f, "lever-x").Stop(context.Background()); err != nil {
		t.Fatalf("Stop on an already-stopped VM should be harmless, got: %v", err)
	}
}

// --- Test 6: DockerHost — default before EnsureUp, resolved uid after. ---

func TestDockerHostDefaultBeforeEnsureUp(t *testing.T) {
	l := New(proc.NewFakeRunner(), "lever-x")
	if got := l.DockerHost(); got != "unix:///run/user/501/docker.sock" {
		t.Fatalf("DockerHost() before EnsureUp = %q", got)
	}
}

func TestDockerHostReflectsResolvedUIDAfterEnsureUp(t *testing.T) {
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", proc.Result{Stdout: "lever-x Running\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl shell lever-x whoami", proc.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", proc.Result{Stdout: "1000\n"}) // non-default uid
	f.Script("limactl shell lever-x bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", proc.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", proc.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
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
	if got := New(proc.NewFakeRunner(), "lever-x").HostToolAlias(); got != "host.lima.internal" {
		t.Fatalf("HostToolAlias() = %q", got)
	}
	if got := JailPrefix("v"); !reflect.DeepEqual(got, []string{"limactl", "shell", "v"}) {
		t.Fatalf("JailPrefix(v) = %v", got)
	}
}

// resolvedLima returns a backend whose run user is resolved (leveruser/501)
// through the same probes EnsureUp issues, without provisioning anything.
func resolvedLima(t *testing.T, f *proc.FakeRunner, vm string) *Lima {
	t.Helper()
	backendtest.ScriptRunUser(f, "limactl shell "+vm, "leveruser", "501")
	l := New(f, vm)
	if err := l.ReadRunUser(context.Background()); err != nil {
		t.Fatalf("ReadRunUser: %v", err)
	}
	f.Calls = nil
	return l
}

func TestJailTransportMethods(t *testing.T) {
	l := resolvedLima(t, proc.NewFakeRunner(), "lever-x")

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
	l := New(f, "lever-x")
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
	l := resolvedLima(t, f, "lever-x")
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
	l := resolvedLima(t, proc.NewFakeRunner(), "lever-x")
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
	f := proc.NewFakeRunner()
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "" +
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
	f := proc.NewFakeRunner()
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", proc.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
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

func TestApplyEgressSkipsRebuildWhenAlreadyClosed(t *testing.T) {
	r := &backendtest.ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: "limactl", Open: true}
	r.Script("limactl shell lever-x sudo iptables", proc.Result{})
	r.Script("limactl shell lever-x sudo ip6tables", proc.Result{})
	r.Script("limactl shell lever-x getent ahosts host.lima.internal", proc.Result{Stdout: "0.250.250.254 STREAM \nfd07::fe STREAM \n"})
	l := New(r, "lever-x")
	// A prior apply resolved a v6 alias; the skip path parses only v4 from the
	// live chain, so a re-apply that hits the skip must leave a prior
	// aliasV6 untouched rather than zeroing it.
	if err := l.ApplyEgress(context.Background(), []int{8443}, true); err != nil {
		t.Fatalf("first ApplyEgress: %v", err)
	}
	if l.HostAliasV6() != "fd07::fe" {
		t.Fatalf("the rebuild must record v6; got %q", l.HostAliasV6())
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
	if l.HostAliasV4() != "0.250.250.254" {
		t.Fatalf("alias should be read from the existing chain, got %q", l.HostAliasV4())
	}
	if l.HostAliasV6() != "fd07::fe" {
		t.Fatalf("skip path must not clobber a prior aliasV6; got %q", l.HostAliasV6())
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
	scriptedVM(f)
	backendtest.ScriptScionInstall(t, f, "limactl shell lever-x", "lever-x")
	l := New(f, "lever-x")

	err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree",
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
	scriptedVM(f)
	backendtest.ScriptScionInstall(t, f, "limactl shell lever-x", "lever-x")
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: "lever-x", ProjectTree: "/Users/x/tree",
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
