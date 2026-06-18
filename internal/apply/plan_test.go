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
	want := []string{"jail-up", "load-image", "init-machine", "config-registry", "scion-server", "register-manager", "register-grove", "register-grove", "write-manifest", "start-manager"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d = %q want %q (all=%v)", i, kinds[i], want[i], kinds)
		}
	}
	// register-grove targets: first grove is at index 6, second at index 7
	// (0:jail-up 1:load-image 2:init-machine 3:config-registry 4:scion-server 5:register-manager 6:register-grove 7:register-grove 8:start-manager)
	if steps[6].Target != "/t/groves/appa" {
		t.Fatalf("register-grove[0] target=%q", steps[6].Target)
	}
	if steps[7].Target != "/t/groves/appb" {
		t.Fatalf("register-grove[1] target=%q", steps[7].Target)
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
