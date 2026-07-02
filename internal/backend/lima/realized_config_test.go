package lima

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

// matchingRealizedConfigJSON returns a `limactl list --json <vm>` line (the
// shape lima's `store.Inspect` produces: a single NDJSON object with a
// "config" key holding the merged LimaYAML) whose containment fields exactly
// match what template.go renders for projectTree. Callers mutate a copy of
// this to script drift scenarios.
func matchingRealizedConfigJSON(vm, projectTree string) string {
	return `{"name":"` + vm + `","status":"Running","config":{` +
		`"mounts":[{"location":"` + projectTree + `","mountPoint":"/lever","writable":true}],` +
		`"portForwards":[` +
		`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
		`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
		`],` +
		`"containerd":{"system":false,"user":false}` +
		`}}`
}

func scriptRealizedConfig(f *exec.FakeRunner, vm, json string) {
	f.Script("limactl list --json "+vm, exec.Result{Stdout: json + "\n"})
}

// --- verifyRealizedConfig: direct unit tests (fake runner, no full EnsureUp). ---

func TestVerifyRealizedConfigAcceptsMatch(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	l := New(f, "lever-x")

	if err := l.verifyRealizedConfig(context.Background(), "/Users/x/tree"); err != nil {
		t.Fatalf("verifyRealizedConfig on a matching config: %v", err)
	}
}

func TestVerifyRealizedConfigDetectsDrift(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			name: "second mount",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true},{"location":"/etc","mountPoint":"/etc-host","writable":false}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "mount not writable",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":false}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "missing the 127.0.0.1 ignore rule",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "a portForward not marked ignore (a real forward slipped in)",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":false}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "proto tcp instead of any (the FIX 1 regression case)",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"tcp","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "containerd system enabled",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":true,"user":false}}}`,
		},
		{
			name: "containerd user enabled",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":true}}}`,
		},
		{
			name: "0.0.0.0 rule missing guestIPMustBeZero",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":false,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
		{
			name: "mount points at the wrong project tree (stale adoption)",
			json: `{"name":"lever-x","status":"Running","config":{` +
				`"mounts":[{"location":"/Users/x/OTHER-tree","mountPoint":"/lever","writable":true}],` +
				`"portForwards":[` +
				`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
				`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
				`],"containerd":{"system":false,"user":false}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			scriptRealizedConfig(f, "lever-x", tc.json)
			l := New(f, "lever-x")

			err := l.verifyRealizedConfig(context.Background(), "/Users/x/tree")
			if err == nil {
				t.Fatal("expected a drift error, got nil")
			}
			if !strings.Contains(err.Error(), "mismatched containment config") || !strings.Contains(err.Error(), "lever-x") {
				t.Fatalf("error should name the VM and say 'mismatched containment config'; got: %v", err)
			}
			if !strings.Contains(err.Error(), "lever down") || !strings.Contains(err.Error(), "lever up") {
				t.Fatalf("error should tell the operator to 'lever down' then 'lever up'; got: %v", err)
			}
		})
	}
}

// --- EnsureUp integration: drift must fail closed BEFORE any provisioning,
// and a matching config must not break the Running->skip idempotency. ---

// TestEnsureUpFailsClosedOnDriftedRunningVM proves the exact F2 bug is fixed:
// an already-Running VM (adopted, not created by this EnsureUp call) whose
// realized config has drifted from the template must fail EnsureUp closed,
// and must NOT proceed to provision runtimes/scion/egress on the drifted VM.
func TestEnsureUpFailsClosedOnDriftedRunningVM(t *testing.T) {
	f := exec.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: "lever-x Running\n"})
	// Drift: a second mount an operator's global override could have added.
	f.Script("limactl list --json lever-x", exec.Result{Stdout: `{"name":"lever-x","status":"Running","config":{` +
		`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true},{"location":"/","mountPoint":"/host","writable":true}],` +
		`"portForwards":[` +
		`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},` +
		`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}` +
		`],"containerd":{"system":false,"user":false}}}` + "\n"})
	l := New(f, "lever-x")

	err := l.EnsureUp(context.Background(), backend.Config{MachineName: "lever-x", ProjectTree: "/Users/x/tree"})
	if err == nil {
		t.Fatal("expected EnsureUp to fail closed on a drifted realized config")
	}
	if !strings.Contains(err.Error(), "mismatched containment config") {
		t.Fatalf("error should say 'mismatched containment config'; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) >= 2 && c.Args[0] == "shell" {
			t.Fatalf("EnsureUp must not provision (no `limactl shell` calls) a VM that failed the drift check: %+v", f.Calls)
		}
	}
}

// TestEnsureUpVerifiesRealizedConfigOnFreshCreate proves the check also runs
// right after `limactl create` (not just on adoption of a pre-existing VM).
func TestEnsureUpVerifiesRealizedConfigOnFreshCreate(t *testing.T) {
	f := exec.NewFakeRunner()
	limaVersionScript(f)
	f.Script("limactl list --format", exec.Result{Stdout: ""}) // no VM yet
	f.Script("limactl create --name=lever-x --tty=false", exec.Result{Stdout: "created\n"})
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	f.Script("limactl start --tty=false lever-x", exec.Result{Stdout: "started\n"})
	f.Script("limactl shell lever-x whoami", exec.Result{Stdout: "leveruser\n"})
	f.Script("limactl shell lever-x id -u", exec.Result{Stdout: "501\n"})
	f.Script("limactl shell lever-x bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x sudo bash", exec.Result{Stdout: "ok\n"})
	f.Script("limactl shell lever-x getent ahosts host.lima.internal", exec.Result{Stdout: "0.250.250.254 STREAM \n"})
	f.Script("limactl shell lever-x sudo iptables", exec.Result{})
	f.Script("limactl shell lever-x sudo ip6tables", exec.Result{})
	l := New(f, "lever-x")

	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: "lever-x", ProjectTree: "/Users/x/tree"}); err != nil {
		t.Fatalf("EnsureUp with a matching freshly-created config: %v", err)
	}

	createIdx := callIndex(f.Calls, []string{"create", "--name=lever-x", "--tty=false"})
	verifyIdx := callIndex(f.Calls, []string{"list", "--json", "lever-x"})
	startIdx := callIndex(f.Calls, []string{"start", "--tty=false", "lever-x"})
	if createIdx < 0 || verifyIdx < 0 || startIdx < 0 {
		t.Fatalf("expected create, verify (list --json), and start calls; got %+v", f.Calls)
	}
	if !(createIdx < verifyIdx && verifyIdx < startIdx) {
		t.Fatalf("expected create < verify < start; got create=%d verify=%d start=%d", createIdx, verifyIdx, startIdx)
	}
}

// TestEnsureUpIsIdempotentWhenRunningAndMatching re-asserts the Running->skip
// idempotency test (lima_test.go) still holds now that a verify call is
// interposed: a matching config must still result in no create/start calls.
func TestEnsureUpIsIdempotentWhenRunningAndMatching(t *testing.T) {
	f := exec.NewFakeRunner()
	scriptedVM(f) // scripts a matching realized config for "/Users/x/tree" too
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
