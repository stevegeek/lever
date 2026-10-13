package apply

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

func planStepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = string(s.Kind)
	}
	return out
}

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
	want := []string{"broker-up", "mint-manager-bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("broker-only plan = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("broker-only plan = %v, want exactly %v", got, want)
		}
	}
	// The fresh machine has no scion binary, so init-machine would fail.
	for _, banned := range []string{"init-machine", "scion-server", "load-image", "register-project", "write-manifest", "start-manager", "config-registry", "credential", "bootstrap-token"} {
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
		kinds = append(kinds, string(s.Kind))
	}
	want := []string{"broker-up", "load-image", "init-machine", "config-registry", "bootstrap-token", "scion-server", "register-project", "agent-template", "mint-manager-bootstrap", "start-manager"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step %d = %q want %q (all=%v)", i, kinds[i], want[i], kinds)
		}
	}
	// One registration of the instance tree, whatever the worker count.
	if steps[6].Target != "/t" {
		t.Fatalf("register-project target=%q", steps[6].Target)
	}
}

// The controller PAT must exist before scion-server locks the hub down.
func TestPlanIncludesBootstrapTokenOnceBeforeScionServer(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
	}
	kinds := planStepNames(Plan(app, PlanOpts{}))
	btIdx, scionIdx, count := -1, -1, 0
	for i, k := range kinds {
		if k == "bootstrap-token" {
			count++
			btIdx = i
		}
		if k == "scion-server" {
			scionIdx = i
		}
	}
	if count != 1 {
		t.Fatalf("bootstrap-token appears %d times, want exactly 1 (kinds=%v)", count, kinds)
	}
	if scionIdx < 0 {
		t.Fatalf("no scion-server step; kinds=%v", kinds)
	}
	if !(btIdx < scionIdx) {
		t.Fatalf("bootstrap-token (idx %d) must come before scion-server (idx %d); kinds=%v", btIdx, scionIdx, kinds)
	}
}

func TestPlanRemoteProxyAbsentWhenDisabled(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
	}
	kinds := planStepNames(Plan(app, PlanOpts{}))
	if contains(kinds, "remote-proxy") {
		t.Fatalf("plan must omit remote-proxy when remote is disabled: %v", kinds)
	}
}

func TestPlanRemoteProxyPresentAndOrderedAfterScionServer(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
		Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"},
	}
	kinds := planStepNames(Plan(app, PlanOpts{}))
	rpIdx, scionIdx := -1, -1
	for i, k := range kinds {
		if k == "remote-proxy" {
			rpIdx = i
		}
		if k == "scion-server" {
			scionIdx = i
		}
	}
	if rpIdx < 0 {
		t.Fatalf("plan must include remote-proxy when remote is enabled: %v", kinds)
	}
	if scionIdx < 0 {
		t.Fatalf("no scion-server step; kinds=%v", kinds)
	}
	if !(scionIdx < rpIdx) {
		t.Fatalf("remote-proxy (idx %d) must come after scion-server (idx %d); kinds=%v", rpIdx, scionIdx, kinds)
	}
}

func TestPlanBrokerOnlyOmitsRemoteProxyEvenWhenEnabled(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img"},
		Remote:  config.Remote{Enabled: true, BaseURL: "https://mac.tail.ts.net"},
	}
	kinds := planStepNames(Plan(app, PlanOpts{BrokerOnly: true}))
	if contains(kinds, "remote-proxy") {
		t.Fatalf("broker-only plan must omit remote-proxy even when remote is enabled: %v", kinds)
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
		kinds = append(kinds, string(s.Kind))
	}
	// credential must appear AFTER scion-server and BEFORE register-project
	credIdx, scionIdx, regIdx := -1, -1, -1
	for i, k := range kinds {
		switch k {
		case "credential":
			credIdx = i
		case "scion-server":
			scionIdx = i
		case "register-project":
			regIdx = i
		}
	}
	if credIdx < 0 {
		t.Fatalf("no credential step; kinds=%v", kinds)
	}
	if !(scionIdx < credIdx && credIdx < regIdx) {
		t.Fatalf("credential must be between scion-server and register-project; scion=%d cred=%d reg=%d", scionIdx, credIdx, regIdx)
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
		idx[string(s.Kind)] = i
	}
	if _, ok := idx["broker-up"]; !ok {
		t.Fatal("plan must include broker-up")
	}
	if _, ok := idx["mint-manager-bootstrap"]; !ok {
		t.Fatal("plan must include mint-manager-bootstrap")
	}
	if idx["broker-up"] != 0 {
		t.Fatal("broker-up must be the first step")
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

// agent-template must follow register-project (its settings are written at
// project scope) and precede start-manager: an agent's system prompt is staged
// when its home is provisioned and never re-staged.
func TestPlanAgentTemplateIsOrdered(t *testing.T) {
	app := &config.App{Name: "demo", Backend: "orbstack", Tree: "/t", Manager: config.Manager{Image: "img"}}
	idx := map[string]int{}
	for i, s := range Plan(app, PlanOpts{}) {
		idx[string(s.Kind)] = i
	}
	at, ok := idx["agent-template"]
	if !ok {
		t.Fatal("default plan must include agent-template")
	}
	if rp := idx["register-project"]; at < rp {
		t.Errorf("agent-template (%d) must run AFTER register-project (%d): project-scope settings need the project", at, rp)
	}
	if sm := idx["start-manager"]; at > sm {
		t.Errorf("agent-template (%d) must run BEFORE start-manager (%d): the prompt is staged at provisioning and never re-staged", at, sm)
	}
}

func TestPlanLoadImageCarriesTarPath(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img:latest", ImageTar: "images/img.tar"},
		Workers: []config.Worker{
			{Name: "inherits", Dir: "w/a"},
			{Name: "docker", Dir: "w/b", Image: "other:latest"},
			{Name: "own", Dir: "w/c", Image: "third:latest", ImageTar: "images/third.tar"},
		},
	}
	var got []Step
	for _, s := range Plan(app, PlanOpts{}) {
		if s.Kind == KindLoadImage {
			got = append(got, s)
		}
	}
	want := []Step{
		{Kind: KindLoadImage, Target: "img:latest", TarPath: app.ManagerImageTarPath()},
		{Kind: KindLoadImage, Target: "other:latest"},
		{Kind: KindLoadImage, Target: "third:latest", TarPath: app.WorkerImageTarPath(app.Workers[2])},
	}
	if len(got) != len(want) {
		t.Fatalf("load-image steps = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A ref named both with and without an archive loads from the archive
// whatever the declaration order.
func TestPlanLoadImageTarWinsOverDockerSource(t *testing.T) {
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: "/t",
		Manager: config.Manager{Image: "img:latest"},
		Workers: []config.Worker{{Name: "w", Dir: "w/a", Image: "img:latest", ImageTar: "images/img.tar"}},
	}
	var got []Step
	for _, s := range Plan(app, PlanOpts{}) {
		if s.Kind == KindLoadImage {
			got = append(got, s)
		}
	}
	want := []Step{{Kind: KindLoadImage, Target: "img:latest", TarPath: app.WorkerImageTarPath(app.Workers[0])}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("load-image steps = %+v, want %+v", got, want)
	}
}
