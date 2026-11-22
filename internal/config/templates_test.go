package config

import (
	"strings"
	"testing"
)

const templateBase = "name: app\nbackend: orbstack\ntree: ./tree\nmanager:\n  obtain: [{tool: db, op: read}]\n" +
	"broker:\n  llm_auth: subscription\n  tools:\n    - {name: db, backend: \"http://127.0.0.1:3201\", command: [/bin/true], operations: [{name: read}, {name: write}]}\n" +
	"workers:\n  - {name: scratch, dir: workers/scratch}\n"

const sessionTmpl = "templates:\n  - name: session\n    prefix: s\n    dir_root: sessions\n    resources: {memory: 2GiB}\n    max_instances: 4\n    max_running: 2\n"

func loadTemplates(t *testing.T, body string) *App {
	t.Helper()
	app, err := LoadNoHostChecks(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestTemplateLoads(t *testing.T) {
	app := loadTemplates(t, templateBase+sessionTmpl+"    obtain: [{tool: db, op: read}]\n")
	tm, ok := app.TemplateByName("session")
	if !ok || tm.EffectivePrefix() != "s" || tm.EffectiveOnDestroy() != WorkspaceArchive || tm.MaySpawn(SpawnerManager) || !tm.MaySpawn(SpawnerOperator) {
		t.Fatalf("%+v", tm)
	}
	if got, ok := app.TemplateOf("s-abc234"); !ok || got.Name != "session" {
		t.Fatal("instance name not recognised")
	}
	for _, n := range []string{"s-abc23", "s-abc2345", "s-ABC234", "s-abc018", "session-abc234", "s"} {
		if _, ok := app.TemplateOf(n); ok {
			t.Errorf("%q read as an instance", n)
		}
	}
	w := tm.Worker("s-abc234")
	if w.Dir != "sessions/s-abc234" || w.Resources.Memory != "2GiB" || len(w.Obtain) != 1 {
		t.Fatalf("%+v", w)
	}
	w.Obtain[0].Op = "write" // the instance's lists are its own
	if tm.Obtain[0].Op != "read" {
		t.Fatal("Worker aliased the template's grants")
	}
	dirs := app.AgentDirs()
	if len(dirs) != 2 || dirs[1].Dir != "sessions" {
		t.Fatalf("%+v", dirs)
	}
}

func TestTemplateRefusals(t *testing.T) {
	lima := strings.Replace(templateBase, "backend: orbstack", "backend: lima", 1)
	for name, tc := range map[string]struct{ body, want string }{
		"bad name":                  {strings.Replace(sessionTmpl, "name: session", "name: Session", 1), "template name"},
		"no dir_root":               {strings.Replace(sessionTmpl, "    dir_root: sessions\n", "", 1), "dir_root"},
		"dir_root escapes":          {strings.Replace(sessionTmpl, "dir_root: sessions", "dir_root: ../x", 1), "dir_root"},
		"dir_root is tree":          {strings.Replace(sessionTmpl, "dir_root: sessions", "dir_root: .", 1), "dir_root"},
		"overlaps a worker":         {strings.Replace(sessionTmpl, "dir_root: sessions", "dir_root: workers", 1), "overlap"},
		"no memory":                 {strings.Replace(sessionTmpl, "    resources: {memory: 2GiB}\n", "", 1), "resources.memory"},
		"no max_running":            {strings.Replace(sessionTmpl, "    max_running: 2\n", "", 1), "max_running"},
		"instances < running":       {strings.Replace(sessionTmpl, "max_instances: 4", "max_instances: 1", 1), "max_instances"},
		"bad on_destroy":            {sessionTmpl + "    on_destroy: {workspace: shred}\n", "on_destroy"},
		"bad spawner":               {sessionTmpl + "    spawners: [worker]\n", "spawners"},
		"unknown tool":              {sessionTmpl + "    obtain: [{tool: nope, op: x}]\n", "nope"},
		"manager wider":             {sessionTmpl + "    spawners: [manager]\n    obtain: [{tool: db, op: write}]\n", "wider than itself"},
		"manager wider by delegate": {sessionTmpl + "    spawners: [manager]\n    delegate: [{tool: db, op: write, to: [scratch]}]\n", "wider than itself"},
		"named like a worker":       {strings.Replace(sessionTmpl, "name: session", "name: scratch", 1), "already the name"},
		"prefix twice": {sessionTmpl + "  - name: other\n    prefix: s\n    dir_root: others\n    resources: {memory: 1GiB}\n    max_instances: 1\n    max_running: 1\n",
			"both use the prefix"},
		"past scion's cap": {strings.Replace(strings.Replace(sessionTmpl, "max_running: 2", "max_running: 11", 1), "max_instances: 4", "max_instances: 11", 1),
			"scion's cap"},
		"past the VM's memory": {"memory: 4GiB\n" + sessionTmpl, "more than the VM's memory"},
	} {
		t.Run(name, func(t *testing.T) {
			base := templateBase
			if strings.HasPrefix(tc.body, "memory:") {
				base = lima
			}
			rejectNoHost(t, base+tc.body, tc.want)
		})
	}
}

// A worker whose name has an instance's shape would share its CN.
func TestTemplateRefusesAWorkerShapedLikeAnInstance(t *testing.T) {
	body := strings.Replace(templateBase, "name: scratch, dir: workers/scratch", "name: s-abcdef, dir: workers/s", 1)
	rejectNoHost(t, body+sessionTmpl, "has the shape of an instance")
}

// A manager-spawnable template may hold only what the manager holds.
func TestTemplateManagerSpawnerWithinTheManager(t *testing.T) {
	app := loadTemplates(t, templateBase+sessionTmpl+"    spawners: [manager]\n    obtain: [{tool: db, op: read}]\n")
	tm, _ := app.TemplateByName("session")
	if !tm.MaySpawn(SpawnerManager) {
		t.Fatal("manager may not spawn")
	}
}

// Delegates may name a template's instances, and only a declared template.
func TestTemplateDelegateTargets(t *testing.T) {
	body := strings.Replace(templateBase, "manager:\n  obtain: [{tool: db, op: read}]\n",
		"manager:\n  obtain: [{tool: db, op: read}]\n  delegate: [{tool: db, op: read, to: [\"template:session\"]}]\n", 1)
	loadTemplates(t, body+sessionTmpl)
	rejectNoHost(t, strings.Replace(body, "template:session", "template:nope", 1)+sessionTmpl, "names no declared template")
}

// Templates go into the shared-folder and read_only overlap checks.
func TestTemplateDirRootIsProtectedLikeAWorkerDir(t *testing.T) {
	body := strings.Replace(templateBase, "manager:\n", "manager:\n  read_only: [sessions/docs]\n", 1)
	rejectNoHost(t, body+sessionTmpl, "overlaps manager.read_only")
}
