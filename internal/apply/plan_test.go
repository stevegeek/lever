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
	want := []string{"jail-up", "scion-server", "hub-enable", "register-manager", "register-grove", "register-grove", "start-manager"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d = %q want %q (all=%v)", i, kinds[i], want[i], kinds)
		}
	}
	if steps[4].Target != "/t/groves/appa" {
		t.Fatalf("register-grove target=%q", steps[4].Target)
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
	// credential must appear AFTER hub-enable and BEFORE register-manager
	credIdx, hubIdx, regIdx := -1, -1, -1
	for i, k := range kinds {
		switch k {
		case "credential":
			credIdx = i
		case "hub-enable":
			hubIdx = i
		case "register-manager":
			regIdx = i
		}
	}
	if credIdx < 0 {
		t.Fatalf("no credential step; kinds=%v", kinds)
	}
	if !(hubIdx < credIdx && credIdx < regIdx) {
		t.Fatalf("credential must be between hub-enable and register-manager; hub=%d cred=%d reg=%d", hubIdx, credIdx, regIdx)
	}
	if steps[credIdx].Target != "/home/x/.scion/oauth-token" {
		t.Fatalf("credential target=%q", steps[credIdx].Target)
	}
}
