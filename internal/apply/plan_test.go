package apply

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// planStepNames extracts the Kind from each Step.
func planStepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return out
}

// contains reports whether needle is in haystack.
func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func TestPlanBrokerOnlyKeepsOnlyBrokerSteps(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img", CredentialFile: "cred"},
		Workers: []config.Worker{{Name: "worker"}},
		Broker:  config.Broker{JailPort: 8443, AdminPort: 8444},
	}
	got := planStepNames(Plan(app, PlanOpts{BrokerOnly: true}))
	want := []string{"jail-up", "broker-up", "mint-manager-bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("broker-only plan = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("broker-only plan = %v, want exactly %v", got, want)
		}
	}
	// Scion/container/registration steps must NOT appear (the fresh machine has no
	// scion binary; init-machine would fail).
	for _, banned := range []string{"init-machine", "scion-server", "load-image", "register-manager", "register-worker", "write-manifest", "start-manager", "config-registry", "credential"} {
		if contains(got, banned) {
			t.Fatalf("broker-only plan must omit %q: %v", banned, got)
		}
	}
}

func TestPlanDefaultIncludesStartManager(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
	}
	steps := planStepNames(Plan(app, PlanOpts{}))
	if !contains(steps, "start-manager") {
		t.Fatalf("default plan must include start-manager: %v", steps)
	}
}

func TestPlanOrder(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img", AllowPorts: []int{3305}},
		Workers: []config.Worker{{Name: "appa", Dir: "workers/appa"}, {Name: "appb", Dir: "workers/appb"}},
	}
	steps := Plan(app, PlanOpts{})
	var kinds []string
	for _, s := range steps {
		kinds = append(kinds, s.Kind)
	}
	want := []string{"jail-up", "broker-up", "load-image", "init-machine", "config-registry", "scion-server", "register-manager", "register-worker", "register-worker", "mint-manager-bootstrap", "start-manager"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d = %q want %q (all=%v)", i, kinds[i], want[i], kinds)
		}
	}
	// register-worker targets: first worker is at index 7, second at index 8
	// (0:jail-up 1:broker-up 2:load-image 3:init-machine 4:config-registry 5:scion-server 6:register-manager 7:register-worker 8:register-worker 9:mint-manager-bootstrap 10:start-manager)
	if steps[7].Target != "/t/workers/appa" {
		t.Fatalf("register-worker[0] target=%q", steps[7].Target)
	}
	if steps[8].Target != "/t/workers/appb" {
		t.Fatalf("register-worker[1] target=%q", steps[8].Target)
	}
}

func TestPlanIncludesCredentialWhenSet(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img", CredentialFile: "/home/x/.scion/oauth-token"},
	}
	steps := Plan(app, PlanOpts{})
	var kinds []string
	for _, s := range steps {
		kinds = append(kinds, s.Kind)
	}
	// credential must appear AFTER scion-server and BEFORE register-manager
	credIdx, scionIdx, regIdx := -1, -1, -1
	for i, k := range kinds {
		switch k {
		case "credential":
			credIdx = i
		case "scion-server":
			scionIdx = i
		case "register-manager":
			regIdx = i
		}
	}
	if credIdx < 0 {
		t.Fatalf("no credential step; kinds=%v", kinds)
	}
	if !(scionIdx < credIdx && credIdx < regIdx) {
		t.Fatalf("credential must be between scion-server and register-manager; scion=%d cred=%d reg=%d", scionIdx, credIdx, regIdx)
	}
	if steps[credIdx].Target != "/home/x/.scion/oauth-token" {
		t.Fatalf("credential target=%q", steps[credIdx].Target)
	}
}

func TestPlanInsertsBrokerSteps(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/tmp/work",
		Manager: config.Manager{Image: "img"},
		Broker:  config.Broker{JailPort: 8443, AdminPort: 8444},
	}
	steps := Plan(app, PlanOpts{})
	idx := map[string]int{}
	for i, s := range steps {
		idx[s.Kind] = i
	}
	if _, ok := idx["broker-up"]; !ok {
		t.Fatal("plan must include broker-up")
	}
	if _, ok := idx["mint-manager-bootstrap"]; !ok {
		t.Fatal("plan must include mint-manager-bootstrap")
	}
	if !(idx["jail-up"] < idx["broker-up"]) {
		t.Fatal("broker-up must come after jail-up")
	}
	if !(idx["mint-manager-bootstrap"] < idx["start-manager"]) {
		t.Fatal("mint-manager-bootstrap must come before start-manager")
	}
}

func TestApplyPlan_noWriteManifest(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
		Workers: []config.Worker{{Name: "worker", Dir: "workers/worker"}},
	}
	for _, s := range Plan(app, PlanOpts{}) {
		if s.Kind == "write-manifest" {
			t.Fatal("plan must not include write-manifest (manifest is write-only dead code)")
		}
	}
}

func TestPlanLoadsDistinctWorkerImages(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "mgr:1"},
		Workers: []config.Worker{
			{Name: "a", Dir: "workers/a"},                 // inherits mgr:1
			{Name: "b", Dir: "workers/b", Image: "alt:1"}, // override
			{Name: "c", Dir: "workers/c", Image: "alt:1"}, // dup override
		},
	}
	var loads []string
	for _, s := range Plan(app, PlanOpts{}) {
		if s.Kind == "load-image" {
			loads = append(loads, s.Target)
		}
	}
	want := []string{"mgr:1", "alt:1"}
	if len(loads) != len(want) {
		t.Fatalf("load-image targets=%v want=%v", loads, want)
	}
	for i := range want {
		if loads[i] != want[i] {
			t.Fatalf("load[%d]=%q want %q (all=%v)", i, loads[i], want[i], loads)
		}
	}
}
