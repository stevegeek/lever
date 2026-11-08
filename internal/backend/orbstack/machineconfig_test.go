package orbstack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
)

// testTree is the project tree the fixtures mount. The testdata files are
// real `orb info --format json` and in-guest /proc/mounts captures from
// OrbStack 2.2.4, with host paths and the user name replaced.
const testTree = "/Users/x/tree"

const (
	infoKey     = "orb info --format json " + machine
	mountsKey   = "orb -m " + machine + " cat /proc/mounts"
	usersKey    = "orb -m " + machine + " sh -c if [ -e /Users ]"
	usersAbsent = "absent\n"
)

func fixture(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return string(b)
}

// scriptConforming scripts a machine lever made: isolated, the test tree
// at /lever and nothing else.
func scriptConforming(f *proc.FakeRunner) {
	f.Script(infoKey, proc.Result{Stdout: fixture("orb-info-good.json")})
	f.Script(mountsKey, proc.Result{Stdout: fixture("proc-mounts-good.txt")})
	f.Script(usersKey, proc.Result{Stdout: usersAbsent})
}

func assertRefused(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the machine to be refused")
	}
	for _, s := range []string{machine, "mismatched containment config", "lever down", want} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error should mention %q; got: %v", s, err)
		}
	}
}

func TestEnsureMachineReusesConformingRunningMachine(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	scriptConforming(f)
	b := New(f, machine, common.Options{})

	skip, err := b.ensureMachine(context.Background(), testTree)
	if err != nil || skip != "already running" {
		t.Fatalf("ensureMachine = %q, %v; want already running", skip, err)
	}
	for _, key := range []string{infoKey, mountsKey, usersKey} {
		if !f.Called(func(c proc.Call) bool { return strings.HasPrefix(c.Argv(), key) }) {
			t.Errorf("expected a call starting %q", key)
		}
	}
}

// Every capture of a machine lever did not make is refused before reuse, and
// a stopped one is not started.
func TestEnsureMachineRefusesMismatchedMachines(t *testing.T) {
	good := fixture("orb-info-good.json")
	cases := []struct {
		name, info, want string
	}{
		{"not isolated", fixture("orb-info-plain.json"), "not isolated"},
		{"no mount", fixture("orb-info-nomount.json"), "no project tree mount"},
		{"another tree", fixture("orb-info-othertree.json"), "/Users/x/other"},
		{"extra mount", fixture("orb-info-extra.json"), "2 host mounts"},
		{"ssh agent forwarded", strings.Replace(good, `"forward_ssh_agent": false`, `"forward_ssh_agent": true`, 1), "SSH agent"},
		{"mount elsewhere", strings.Replace(good, `"destination": "/lever"`, `"destination": "/work"`, 1), "/work"},
		{"unparseable", "Name: lever-jail\nState: running\n", "does not parse"},
		{"no isolation field", `{"record": {"config": {"mounts": []}}}`, "no isolation setting"},
		{"no config", `{"record": {"name": "lever-jail"}}`, "no isolation setting"},
	}
	for _, list := range []string{listRunning, listStopped} {
		for _, tc := range cases {
			t.Run(strings.Fields(list)[1]+"/"+tc.name, func(t *testing.T) {
				f := proc.NewFakeRunner()
				scriptList(f, list)
				f.Script(infoKey, proc.Result{Stdout: tc.info})
				f.Script("orb start "+machine, proc.Result{})
				b := New(f, machine, common.Options{})

				_, err := b.ensureMachine(context.Background(), testTree)
				assertRefused(t, err, tc.want)
				backendtest.AssertNoSubcommand(t, f, "orb", "start", "create")
			})
		}
	}
}

func TestEnsureMachineErrorsWhenOrbInfoFails(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script(infoKey, proc.Result{Code: 1, Stderr: "machine not found"})
	b := New(f, machine, common.Options{})

	if _, err := b.ensureMachine(context.Background(), testTree); err == nil {
		t.Fatal("expected an error when orb info fails")
	}
}

// The in-guest layer refuses what OrbStack's record missed.
func TestEnsureMachineRefusesGuestMismatch(t *testing.T) {
	cases := []struct {
		name, mounts, users, want string
	}{
		{"extra host mount", fixture("proc-mounts-extra.txt"), usersAbsent, "/extra"},
		{"Mac folders mounted", fixture("proc-mounts-plain.txt"), usersAbsent, "/Users"},
		{"no /lever", strings.ReplaceAll(fixture("proc-mounts-good.txt"), "mac /lever virtiofs", "tmpfs /lever tmpfs"), usersAbsent, "no host mount at /lever"},
		{"/Users visible", fixture("proc-mounts-good.txt"), "present\n", "/Users is visible"},
		{"/Users unknown", fixture("proc-mounts-good.txt"), "", "cannot tell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			scriptList(f, listRunning)
			f.Script(infoKey, proc.Result{Stdout: fixture("orb-info-good.json")})
			f.Script(mountsKey, proc.Result{Stdout: tc.mounts})
			f.Script(usersKey, proc.Result{Stdout: tc.users})
			b := New(f, machine, common.Options{})

			_, err := b.ensureMachine(context.Background(), testTree)
			assertRefused(t, err, tc.want)
		})
	}
}

// A mount below /lever is the project tree's own business, not a second
// host mount the guest can tell apart.
func TestGuestMountMismatchAllowsMountsBelowTree(t *testing.T) {
	mounts := fixture("proc-mounts-good.txt") + "mac /lever/sub virtiofs rw,relatime 0 0\n"
	if got := guestMountMismatch(mounts); got != "" {
		t.Fatalf("guestMountMismatch = %q, want none", got)
	}
}

func TestEnsureMachineChecksGuestAfterStartingStoppedMachine(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listStopped)
	f.Script(infoKey, proc.Result{Stdout: fixture("orb-info-good.json")})
	f.Script("orb start "+machine, proc.Result{})
	f.Script(orbGuest.User+" true", proc.Result{})
	f.Script(mountsKey, proc.Result{Stdout: fixture("proc-mounts-extra.txt")})
	f.Script(usersKey, proc.Result{Stdout: usersAbsent})
	f.Script("orb stop "+machine, proc.Result{})
	b := New(f, machine, common.Options{})

	_, err := b.ensureMachine(context.Background(), testTree)
	assertRefused(t, err, "/extra")
	info := f.CallIndex(func(c proc.Call) bool { return c.Argv() == infoKey })
	start := f.CallIndex(proc.Subcommand("orb", "start"))
	mounts := f.CallIndex(func(c proc.Call) bool { return c.Argv() == mountsKey })
	stop := f.CallIndex(proc.Subcommand("orb", "stop"))
	if !(info >= 0 && info < start && start < mounts && mounts < stop) {
		t.Fatalf("want orb info, orb start, the guest check, then orb stop; got indexes %d, %d, %d, %d", info, start, mounts, stop)
	}
}

// A running machine that fails the guest check is refused but not stopped:
// lever did not start it.
func TestEnsureMachineLeavesRefusedRunningMachineRunning(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	f.Script(infoKey, proc.Result{Stdout: fixture("orb-info-good.json")})
	f.Script(mountsKey, proc.Result{Stdout: fixture("proc-mounts-extra.txt")})
	f.Script(usersKey, proc.Result{Stdout: usersAbsent})
	b := New(f, machine, common.Options{})

	_, err := b.ensureMachine(context.Background(), testTree)
	assertRefused(t, err, "/extra")
	backendtest.AssertNoSubcommand(t, f, "orb", "stop", "start", "create")
}

// A project tree reached through a symbolic link matches the real path
// OrbStack recorded.
func TestEnsureMachineAcceptsTreeThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	f := proc.NewFakeRunner()
	scriptList(f, listRunning)
	scriptConforming(f)
	f.Script(infoKey, proc.Result{Stdout: strings.Replace(fixture("orb-info-good.json"), testTree, resolved, 1)})
	b := New(f, machine, common.Options{})

	if _, err := b.ensureMachine(context.Background(), link); err != nil {
		t.Fatalf("ensureMachine through a symlink: %v", err)
	}
}
