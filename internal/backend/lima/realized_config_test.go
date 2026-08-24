package lima

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
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

func scriptRealizedConfig(f *proc.FakeRunner, vm, json string) {
	f.Script("limactl list --json "+vm, proc.Result{Stdout: json + "\n"})
}

// configDriftMsg is the fragment of the drift error the operator reads; the
// full wording (naming the VM and the `lever down`/`lever up` repair) is the
// contract these tests pin.
const configDriftMsg = "mismatched containment config"

// --- verifyRealizedConfig: direct unit tests (fake runner, no full EnsureUp). ---

func TestVerifyRealizedConfigAcceptsMatch(t *testing.T) {
	f := proc.NewFakeRunner()
	scriptRealizedConfig(f, "lever-x", matchingRealizedConfigJSON("lever-x", "/Users/x/tree"))
	l := New(f, "lever-x", common.Options{})

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
			f := proc.NewFakeRunner()
			scriptRealizedConfig(f, "lever-x", tc.json)
			l := New(f, "lever-x", common.Options{})

			err := l.verifyRealizedConfig(context.Background(), "/Users/x/tree")
			if err == nil {
				t.Fatal("expected a drift error, got nil")
			}
			if !strings.Contains(err.Error(), configDriftMsg) || !strings.Contains(err.Error(), vm) {
				t.Fatalf("error should name the VM and say %q; got: %v", configDriftMsg, err)
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
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listRunning)
	// Drift: a second mount an operator's global override could have added.
	scriptRealizedConfig(f, vm, `{"name":"lever-x","status":"Running","config":{`+
		`"mounts":[{"location":"/Users/x/tree","mountPoint":"/lever","writable":true},{"location":"/","mountPoint":"/host","writable":true}],`+
		`"portForwards":[`+
		`{"guestIP":"0.0.0.0","guestIPMustBeZero":true,"guestPortRange":[1,65535],"proto":"any","ignore":true},`+
		`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true}`+
		`],"containerd":{"system":false,"user":false}}}`)
	l := New(f, vm, common.Options{})

	err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree})
	if err == nil {
		t.Fatal("expected EnsureUp to fail closed on a drifted realized config")
	}
	if !strings.Contains(err.Error(), configDriftMsg) {
		t.Fatalf("error should say %q; got: %v", configDriftMsg, err)
	}
	if f.Called(proc.Subcommand("limactl", "shell")) {
		t.Fatalf("EnsureUp must not provision (no `limactl shell` calls) a VM that failed the drift check: %+v", f.Calls)
	}
}

// TestEnsureUpVerifiesRealizedConfigOnFreshCreate proves the check also runs
// right after `limactl create` (not just on adoption of a pre-existing VM).
func TestEnsureUpVerifiesRealizedConfigOnFreshCreate(t *testing.T) {
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listAbsent)
	scriptLifecycle(f)
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsV4)
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree}); err != nil {
		t.Fatalf("EnsureUp with a matching freshly-created config: %v", err)
	}

	createIdx := callIndex(f, "create", "--name="+vm, "--tty=false")
	verifyIdx := callIndex(f, "list", "--json", vm)
	startIdx := callIndex(f, "start", "--tty=false", vm)
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
	f := proc.NewFakeRunner()
	scriptedVM(f) // scripts a matching realized config for "/Users/x/tree" too
	l := New(f, vm, common.Options{})

	if err := l.EnsureUp(context.Background(), backend.Config{
		MachineName: vm, ProjectTree: tree,
	}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	backendtest.AssertNoSubcommand(t, f, "limactl", "create", "start")
}
