package apply

import (
	"testing"

	"github.com/lever-to/lever/internal/config"
)

func TestPlanOrder(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img", AllowPorts: []int{3305}},
		Groves:  []config.Grove{{Name: "appa", Dir: "groves/appa"}, {Name: "appb", Dir: "groves/appb"}},
	}
	steps := Plan(app)
	var kinds []string
	for _, s := range steps {
		kinds = append(kinds, s.Kind)
	}
	want := []string{"jail-up", "broker-up", "load-image", "init-machine", "config-registry", "scion-server", "register-manager", "register-grove", "register-grove", "write-manifest", "mint-manager-bootstrap", "start-manager"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d = %q want %q (all=%v)", i, kinds[i], want[i], kinds)
		}
	}
	// register-grove targets: first grove is at index 7, second at index 8
	// (0:jail-up 1:broker-up 2:load-image 3:init-machine 4:config-registry 5:scion-server 6:register-manager 7:register-grove 8:register-grove 9:write-manifest 10:mint-manager-bootstrap 11:start-manager)
	if steps[7].Target != "/t/groves/appa" {
		t.Fatalf("register-grove[0] target=%q", steps[7].Target)
	}
	if steps[8].Target != "/t/groves/appb" {
		t.Fatalf("register-grove[1] target=%q", steps[8].Target)
	}
}

func TestPlanIncludesCredentialWhenSet(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img", CredentialFile: "/home/x/.scion/oauth-token"},
	}
	steps := Plan(app)
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
	steps := Plan(app)
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

func TestPlanLoadsDistinctGroveImages(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "mgr:1"},
		Groves: []config.Grove{
			{Name: "a", Dir: "groves/a"},                 // inherits mgr:1
			{Name: "b", Dir: "groves/b", Image: "alt:1"}, // override
			{Name: "c", Dir: "groves/c", Image: "alt:1"}, // dup override
		},
	}
	var loads []string
	for _, s := range Plan(app) {
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
