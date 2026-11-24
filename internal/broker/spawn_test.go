package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/scion"
)

// spawnEnv is a broker with one template, "session" (prefix s, dir_root
// sessions), over a temporary tree, with its registry, revocations,
// archives and conversation backups observable.
type spawnEnv struct {
	b        *Broker
	rt       *fakeRuntime
	tree     string
	archives string
	mu       sync.Mutex
	saved    AgentsState
	revoked  RevocationState
	backups  []string
	backErr  error
	saveErr  error
}

func (e *spawnEnv) state() AgentsState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saved
}

func sessionTemplate(tree string, opts ...func(*TemplateSpec)) TemplateSpec {
	t := TemplateSpec{
		Name: "session", Prefix: "s", DirRoot: "sessions", MaxInstances: 3, MaxRunning: 2, OnDestroy: "archive",
		Obtain:   []rules.Grant{{Tool: "db", Op: "read"}},
		SkillRel: ".claude/skills/lever-agent/SKILL.md", Skill: []byte("skill text\n"),
	}
	t.Worker = func(name string) WorkerSpec {
		return WorkerSpec{Name: name, WorkspaceSubdir: "sessions/" + name, HostWorkspace: filepath.Join(tree, "sessions", name),
			TicketDir: "/run/user/1000/lever/" + name, Image: fakeRecordImage}
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func newSpawnEnv(t *testing.T, saved AgentsState, opts ...func(*TemplateSpec)) *spawnEnv {
	t.Helper()
	tree := t.TempDir()
	e := &spawnEnv{rt: &fakeRuntime{agents: map[string][]scion.Agent{}}, tree: tree, archives: filepath.Join(t.TempDir(), "archives"), saved: saved}
	cfg := testConfig(t, withManager("test-manager", "appname"), withRuntime(e.rt, WorkerSpec{Name: "worker"}))
	cfg.Dispatch.Tree = tree
	cfg.Dispatch.Templates = []TemplateSpec{sessionTemplate(tree, opts...)}
	cfg.Dispatch.ArchiveDir = e.archives
	cfg.Dispatch.BackupConversation = func(_ context.Context, agent string) (string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.backErr != nil {
			return "", e.backErr
		}
		e.backups = append(e.backups, agent)
		return "20261122T120000Z", nil
	}
	cfg.Persistence.Agents = saved
	cfg.Persistence.PersistAgents = func(as AgentsState) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.saveErr != nil {
			return e.saveErr
		}
		e.saved = as
		return nil
	}
	cfg.Persistence.PersistRevocation = func(rs RevocationState) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.revoked = rs
		return nil
	}
	e.b = New(cfg)
	e.b.liveInterval = 0
	return e
}

var spawnedNameRE = regexp.MustCompile(`^s-[a-z2-7]{6}$`)

func (e *spawnEnv) spawn(t *testing.T, body string) SpawnResponse {
	t.Helper()
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, body)
	var got SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("spawn: %d %s", rec.Code, rec.Body)
	}
	return got
}

// A spawn draws a fresh name, creates its workspace with the skill, grants
// the template's capabilities, starts it fresh and records it active.
func TestSpawnStartsAnAgentFromItsTemplate(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	got := e.spawn(t, `{"template":"session","task":"review the PR","label":"PR 42"}`)
	if !spawnedNameRE.MatchString(got.Name) || got.Template != "session" || got.Label != "PR 42" || got.Phase != scion.PhaseRunning {
		t.Fatalf("%+v", got)
	}
	if len(e.rt.started) != 1 || e.rt.started[0].Worker != got.Name || e.rt.started[0].WorkspaceSubdir != "sessions/"+got.Name || e.rt.started[0].Task != "review the PR" {
		t.Fatalf("started %+v", e.rt.started)
	}
	skill, err := os.ReadFile(filepath.Join(e.tree, "sessions", got.Name, ".claude/skills/lever-agent/SKILL.md"))
	if err != nil || string(skill) != "skill text\n" {
		t.Fatalf("skill %q %v", skill, err)
	}
	st := e.state()
	if len(st.Agents) != 1 || st.Agents[0].State != AgentActive || st.Agents[0].CreatedBy != operatorActor || st.Agents[0].DirID == "" {
		t.Fatalf("registry %+v", st)
	}
	if _, ok := e.b.rules.MayObtainRule(got.Name, got.Name, "db", "read"); !ok {
		t.Fatal("the template's grant is missing")
	}
	if _, ok := e.b.workerSpec(got.Name); !ok {
		t.Fatal("an active agent is no worker")
	}
}

// The caller chooses the template, the task and the label only: any other
// field is refused, and a refused spawn leaves nothing behind.
func TestSpawnRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body, reason string
		status       int
	}{
		"unknown field: name":    {`{"template":"session","name":"s-aaaaaa"}`, ReasonBadRequest, http.StatusBadRequest},
		"unknown field: image":   {`{"template":"session","image":"evil:1"}`, ReasonBadRequest, http.StatusBadRequest},
		"trailing data":          {`{"template":"session"}{"template":"session"}`, ReasonBadRequest, http.StatusBadRequest},
		"no such template":       {`{"template":"nope"}`, ReasonNoTemplate, http.StatusNotFound},
		"label with a newline":   {`{"template":"session","label":"a\nb"}`, ReasonBadLabel, http.StatusBadRequest},
		"label with a bidi mark": {`{"template":"session","label":"a‮b"}`, ReasonBadLabel, http.StatusBadRequest},
		"label with an escape":   {`{"template":"session","label":"\u001b[2J"}`, ReasonBadLabel, http.StatusBadRequest},
		"label too long":         {`{"template":"session","label":"` + strings.Repeat("x", MaxAgentLabel+1) + `"}`, ReasonBadLabel, http.StatusBadRequest},
		"flag-shaped task":       {`{"template":"session","task":"--help"}`, ReasonBadTask, http.StatusBadRequest},
	} {
		e := newSpawnEnv(t, AgentsState{})
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, tc.body)
		if rec.Code != tc.status || rec.Header().Get(ReasonHeader) != tc.reason {
			t.Errorf("%s: %d %q %s", name, rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
		}
		if len(e.state().Agents) != 0 || len(e.rt.started) != 0 {
			t.Errorf("%s: left %+v, started %d", name, e.state(), len(e.rt.started))
		}
		if entries, _ := os.ReadDir(e.tree); len(entries) != 0 {
			t.Errorf("%s: the tree holds %v", name, entries)
		}
	}
}

// max_instances counts every agent not destroyed; max_running counts the
// running ones, inside the admission every start goes through.
func TestSpawnTemplateCaps(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.MaxInstances, t.MaxRunning = 2, 2 })
	e.spawn(t, `{"template":"session"}`)
	e.rt.staticPhases = true // both records stay running
	first := e.state().Agents[0].Name
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: first, Phase: "suspended"}}
	e.rt.staticPhases = false
	e.spawn(t, `{"template":"session"}`)
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`)
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonTemplateFull {
		t.Fatalf("third: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}

	e = newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.MaxRunning = 1 })
	e.spawn(t, `{"template":"session"}`) // the fake lists it running from now on
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`)
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonTemplateBusy {
		t.Fatalf("over max_running: %d %q %s", rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
	}
	// A busy template refuses before a name is drawn: nothing is burnt.
	if st := e.state(); len(st.Agents) != 1 {
		t.Fatalf("registry %+v", st)
	}
	if entries, _ := os.ReadDir(e.archives); len(entries) != 0 {
		t.Fatalf("archived %v", entries)
	}
}

// A start that fails is undone: the name is destroyed and revoked, its
// grants dropped, and the caller gets the start's refusal.
func TestSpawnFailedStartIsUndone(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	e.rt.startErr = errors.New("boom")
	e.rt.staticPhases = true // the failed start left no record
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st := e.state()
	if len(st.Agents) != 1 || st.Agents[0].State != AgentDestroyed {
		t.Fatalf("registry %+v", st)
	}
	name := st.Agents[0].Name
	if !e.b.isRevoked(name) {
		t.Fatal("not revoked")
	}
	if _, ok := e.b.rules.MayObtainRule(name, name, "db", "read"); ok {
		t.Fatal("grants kept")
	}
	if _, ok := e.b.workerSpec(name); ok {
		t.Fatal("still a worker")
	}
	// It never ran: its workspace is removed, not archived.
	if _, err := os.Stat(filepath.Join(e.tree, st.Agents[0].Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace: %v", err)
	}
	if entries, _ := os.ReadDir(e.archives); len(entries) != 0 {
		t.Fatalf("archived %v", entries)
	}
}

// Destroy refuses a running agent unless asked to stop it; then it backs up
// the conversation, cuts authority, archives the workspace, deletes the
// record and keeps a tombstone.
func TestDestroy(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	if err := os.WriteFile(filepath.Join(e.tree, "sessions", name, "work.txt"), []byte("product"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonRunning || len(e.rt.purged) != 0 {
		t.Fatalf("no stop: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","stop":true}`)
	var got DestroyResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.rt.stopped) != 1 || len(e.rt.purged) != 1 || e.rt.purged[0] != name || len(e.backups) != 1 || got.Transcript == "" {
		t.Fatalf("stopped %v purged %v backups %v %+v", e.rt.stopped, e.rt.purged, e.backups, got)
	}
	if !strings.HasPrefix(got.Workspace, "archived to ") || len(got.Warnings) != 0 {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(filepath.Join(e.tree, "sessions", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.archives, name+".tar.gz")); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	if st.Agents[0].State != AgentDestroyed || st.Agents[0].DestroyedAt.IsZero() || !e.b.isRevoked(name) {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(strings.Join(e.revoked.Revoked, ","), name) {
		t.Fatalf("revocation not saved: %+v", e.revoked)
	}
	// A tombstone is never destroyed again nor driven.
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("again: %d", rec.Code)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerResume, `{"worker":"`+name+`"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("resume of a destroyed agent: %d", rec.Code)
	}
}

// A failed backup stops destroy before anything changes; no_backup goes on.
func TestDestroyBackupFailure(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: name, Phase: "suspended"}}
	e.backErr = errors.New("no space")
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusBadGateway || rec.Header().Get(ReasonHeader) != ReasonBackupFailed {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
	if e.b.isRevoked(name) || len(e.rt.purged) != 0 || e.state().Agents[0].State != AgentActive {
		t.Fatal("a failed backup changed something")
	}
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","no_backup":true}`)
	if rec.Code != http.StatusOK || !e.b.isRevoked(name) {
		t.Fatalf("no_backup: %d %s", rec.Code, rec.Body)
	}
}

// A purge that fails leaves the agent destroying with its authority cut;
// destroy again finishes it.
func TestDestroyResumesAfterAFailedPurge(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: name, Phase: "suspended"}}
	e.rt.purgeErr = scion.ErrDeleteUnconfirmed
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusBadGateway || e.state().Agents[0].State != AgentDestroying || !e.b.isRevoked(name) {
		t.Fatalf("%d %+v", rec.Code, e.state())
	}
	if _, ok := e.b.workerSpec(name); ok {
		t.Fatal("a destroying agent is a worker")
	}
	e.rt.purgeErr = nil
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusOK || e.state().Agents[0].State != AgentDestroyed || len(e.backups) != 1 {
		t.Fatalf("again: %d %s %+v backups %v", rec.Code, rec.Body, e.state(), e.backups)
	}
}

// Destroy is only for spawned agents, and the manager destroys only its own.
func TestDestroyRefusals(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	for _, n := range []string{"worker", "appname", "test-manager", "s-zzzzzz", "../x"} {
		if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+n+`"}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", n, rec.Code)
		}
	}
	w := httptest.NewRecorder()
	e.b.destroyAgent(context.Background(), w, DestroyRequest{Name: name, Stop: true}, "test-manager")
	if w.Code != http.StatusForbidden || w.Header().Get(ReasonHeader) != ReasonNotYours || len(e.rt.purged) != 0 {
		t.Fatalf("manager: %d %q", w.Code, w.Header().Get(ReasonHeader))
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","force":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}

// A spawned agent is never purged or recycled: destroy revokes it.
func TestSpawnedAgentIsNotPurgedOrRecycled(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	for _, body := range []string{`{"worker":"` + name + `"}`, `{"worker":"` + name + `","orphan":true}`} {
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerPurge, body)
		if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonSpawned {
			t.Fatalf("purge %s: %d %q", body, rec.Code, rec.Header().Get(ReasonHeader))
		}
	}
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerRecycle, `{"worker":"`+name+`"}`)
	if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonSpawned {
		t.Fatalf("recycle: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
	if len(e.rt.purged) != 0 {
		t.Fatal("purged")
	}
}

// A spawned agent with no hub record is never started fresh outside spawn.
func TestSpawnedAgentIsNotStartedFresh(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = nil
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerStart, `{"worker":"`+name+`","task":"x"}`)
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonNoRecord || len(e.rt.started) != 1 {
		t.Fatalf("%d %q started %d", rec.Code, rec.Header().Get(ReasonHeader), len(e.rt.started))
	}
}

// A "template:" delegate matches only an active agent of that template.
func TestTemplateDelegateFollowsTheRegistry(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	e.b.rules.AllowDelegate("test-manager", "db", "read", rules.TemplateRefPrefix+"session")
	name := e.spawn(t, `{"template":"session"}`).Name
	if _, ok := e.b.rules.MayObtainRule("test-manager", name, "db", "read"); !ok {
		t.Fatal("not delegable to an active agent")
	}
	for _, bound := range []string{"worker", "template:session", "s-zzzzzz"} {
		if _, ok := e.b.rules.MayObtainRule("test-manager", bound, "db", "read"); ok {
			t.Fatalf("delegable to %q", bound)
		}
	}
	callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","stop":true}`)
	if _, ok := e.b.rules.MayObtainRule("test-manager", name, "db", "read"); ok {
		t.Fatal("delegable to a destroyed agent")
	}
}

// After a restart: an active agent keeps its grants; a destroyed one stays
// revoked even when the revocation file lost it; an interrupted spawn
// without a record is undone and an interrupted destroy finished.
func TestRegistryAcrossARestart(t *testing.T) {
	saved := AgentsState{Agents: []AgentRecord{
		{Name: "s-aaaaaa", Template: "session", State: AgentActive, Dir: "sessions/s-aaaaaa"},
		{Name: "s-bbbbbb", Template: "session", State: AgentDestroyed, Dir: "sessions/s-bbbbbb"},
		{Name: "s-cccccc", Template: "session", State: AgentSpawning, Dir: "sessions/s-cccccc"},
		{Name: "s-dddddd", Template: "session", State: AgentDestroying, Dir: "sessions/s-dddddd"},
		{Name: "x-eeeeee", Template: "gone", State: AgentActive, Dir: "old/x-eeeeee"},
	}}
	e := newSpawnEnv(t, saved)
	if _, ok := e.b.rules.MayObtainRule("s-aaaaaa", "s-aaaaaa", "db", "read"); !ok {
		t.Fatal("an active agent lost its grant")
	}
	for _, n := range []string{"s-bbbbbb", "s-dddddd"} {
		if !e.b.isRevoked(n) {
			t.Fatalf("%s not revoked", n)
		}
	}
	for _, n := range []string{"s-cccccc", "s-dddddd", "x-eeeeee"} {
		if _, ok := e.b.workerSpec(n); ok {
			t.Fatalf("%s is a worker before its reconcile", n)
		}
		if _, ok := e.b.rules.MayObtainRule(n, n, "db", "read"); ok {
			t.Fatalf("%s holds grants", n)
		}
	}
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: "s-dddddd", Phase: "suspended"}}
	e.b.ReconcileAgents(context.Background())
	states := map[string]string{}
	for _, r := range e.state().Agents {
		states[r.Name] = r.State
	}
	if states["s-cccccc"] != AgentDestroyed || states["s-dddddd"] != AgentDestroyed || states["x-eeeeee"] != AgentActive || !e.b.isRevoked("s-cccccc") {
		t.Fatalf("%v", states)
	}
	list := e.b.agentList(context.Background(), false, "", true)
	orphaned := false
	for _, a := range list.Agents {
		if a.Name == "x-eeeeee" {
			orphaned = a.Orphaned
		}
	}
	if !orphaned {
		t.Fatalf("an agent of a removed template is not orphaned: %+v", list.Agents)
	}
}

// A name already used, by a tombstone or a revoked name, is drawn again.
func TestReserveNeverReusesAName(t *testing.T) {
	r := newAgentRegistry([]WorkerSpec{{Name: "s-aaaaaa"}})
	tmpl := sessionTemplate("/tree")
	r.load([]TemplateSpec{tmpl}, AgentsState{Agents: []AgentRecord{{Name: "s-bbbbbb", Template: "session", State: AgentDestroyed}}}, "/tree")
	ids := []string{"aaaaaa", "bbbbbb", "cccccc", "dddddd"}
	draw := func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil }
	rec, err := r.reserve(tmpl, operatorActor, "", time.Time{}, func(n string) bool { return n == "s-cccccc" }, draw)
	if err != nil || rec.Name != "s-dddddd" {
		t.Fatalf("%+v %v", rec, err)
	}
}

// A registry that cannot be saved refuses the spawn.
func TestSpawnRefusedWhenTheRegistryIsNotSaved(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	e.saveErr = errors.New("disk full")
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`)
	if rec.Code != http.StatusInternalServerError || len(e.rt.started) != 0 {
		t.Fatalf("%d started %d", rec.Code, len(e.rt.started))
	}
	if entries, _ := os.ReadDir(e.tree); len(entries) != 0 {
		t.Fatalf("the tree holds %v", entries)
	}
}

// The spawn routes are on the operator socket only.
func TestSpawnRoutesAreNotOnTheTCPListeners(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	for name, h := range map[string]http.Handler{"admin": e.b.AdminHandler(), "jail": e.b.JailHandler()} {
		for _, p := range []string{PathOperatorAgentSpawn, PathOperatorAgentDestroy, PathOperatorAgentList} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"template":"session"}`)))
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s answers %s: %d", name, p, rec.Code)
			}
		}
	}
	if len(e.rt.started) != 0 {
		t.Fatal("spawned through a TCP listener")
	}
}

// The manager spawns only from a template that names it, authenticated
// before its body is read; it drives and destroys only its own agents.
func TestManagerSpawnAndDestroy(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	if rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager"); rec.Code != http.StatusNotFound || rec.Header().Get(ReasonHeader) != ReasonNoTemplate {
		t.Fatalf("a template without the manager: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
	e = newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true })
	for _, cn := range []string{"worker", "s-aaaaaa", ""} {
		if rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, cn); rec.Code != http.StatusForbidden {
			t.Fatalf("%q: %d", cn, rec.Code)
		}
	}
	if rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session","name":"x"}`, "test-manager"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session","task":"t","label":"mine"}`, "test-manager")
	var mine SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if r, _ := e.b.workers.record(mine.Name); r.CreatedBy != "test-manager" {
		t.Fatalf("created_by %q", r.CreatedBy)
	}
	theirs := e.spawn(t, `{"template":"session"}`).Name
	// Lifecycle verbs: only on its own.
	if rec := callWorker(t, e.b, "/worker/suspend", `{"worker":"`+theirs+`"}`, "test-manager"); rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonNotYours {
		t.Fatalf("suspend theirs: %d", rec.Code)
	}
	if rec := callWorker(t, e.b, "/worker/suspend", `{"worker":"`+mine.Name+`"}`, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("suspend mine: %d %s", rec.Code, rec.Body)
	}
	// The list shows its own agents only, and the counts of all.
	rec = callWorker(t, e.b, "/agent/list", `{}`, "test-manager")
	var list struct {
		Templates []struct {
			Name      string
			Instances int
		}
		Agents []struct{ Name string }
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Agents) != 1 || list.Agents[0].Name != mine.Name || list.Templates[0].Instances != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := callWorker(t, e.b, "/agent/destroy", `{"name":"`+theirs+`","stop":true}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("destroy theirs: %d", rec.Code)
	}
	if rec := callWorker(t, e.b, "/agent/destroy", `{"name":"`+mine.Name+`","stop":true,"no_backup":true}`, "test-manager"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no_backup from the manager: %d", rec.Code)
	}
	if rec := callWorker(t, e.b, "/agent/destroy", `{"name":"`+mine.Name+`","stop":true}`, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("destroy mine: %d %s", rec.Code, rec.Body)
	}
	// A revoked manager spawns nothing.
	e.b.Revoke("test-manager")
	if rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked manager: %d", rec.Code)
	}
}

// The manager's spawns are rate limited.
func TestManagerSpawnRateLimit(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true; t.MaxInstances = 20; t.MaxRunning = 20 })
	var last int
	for range managerSpawnRateLimit + 1 {
		last = callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager").Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("spawn %d: %d", managerSpawnRateLimit+1, last)
	}
}

// Destroy acts only on the directory spawn created: a workspace swapped for
// a link, or replaced by another directory, is kept, and nothing it points
// at is touched.
func TestDestroyRefusesASwappedWorkspace(t *testing.T) {
	for _, swap := range []string{"symlink", "rename"} {
		e := newSpawnEnv(t, AgentsState{})
		name := e.spawn(t, `{"template":"session"}`).Name
		victim := filepath.Join(e.tree, "workers", "scratch")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(e.tree, "sessions", name)
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		if swap == "symlink" {
			if err := os.Symlink(victim, ws); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Rename(victim, ws); err != nil {
			t.Fatal(err)
		}
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","stop":true}`)
		var got DestroyResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: %d %s", swap, rec.Code, rec.Body)
		}
		if !strings.HasPrefix(got.Workspace, "kept") || len(got.Warnings) == 0 {
			t.Fatalf("%s: %+v", swap, got)
		}
		precious := filepath.Join(victim, "precious")
		if swap == "rename" {
			precious = filepath.Join(ws, "precious")
		}
		if _, err := os.Stat(precious); err != nil {
			t.Fatalf("%s: the other directory was touched: %v", swap, err)
		}
		if entries, _ := os.ReadDir(e.archives); len(entries) != 0 {
			t.Fatalf("%s: archived %v", swap, entries)
		}
	}
}

// A planted entry at the new agent's workspace refuses the spawn.
func TestSpawnRefusesAPlantedWorkspace(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	ids := []string{"plantd"}
	if err := os.MkdirAll(filepath.Join(e.tree, "sessions", "s-plantd"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl, _ := e.b.workers.template("session")
	rec, err := e.b.workers.reserve(tmpl, operatorActor, "", time.Time{}, func(string) bool { return false }, func() (string, error) { id := ids[0]; return id, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.createAgentDir(rec.Dir, nil); err == nil {
		t.Fatal("created over a planted directory")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(e.tree, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.createAgentDir("elsewhere/s-zzzzzz", nil); err == nil {
		t.Fatal("created through a link")
	}
}

// Parallel spawns never pass the template's caps.
func TestParallelSpawnsKeepTheCaps(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.MaxInstances = 3; t.MaxRunning = 3 })
	e.rt.staticPhases = true // records stay as seeded: none running
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`).Code
		}()
	}
	wg.Wait()
	live := 0
	for _, r := range e.state().Agents {
		if r.State != AgentDestroyed {
			live++
		}
	}
	if live > 3 {
		t.Fatalf("%d agents live past max_instances 3 (codes %v)", live, codes)
	}
}

// An archive stores links as links and skips special files.
func TestArchiveDirStoresLinksAndSkipsSpecials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := syscallMkfifo(filepath.Join(dir, "p")); err != nil {
		t.Skip("no mkfifo:", err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var buf strings.Builder
	files, skipped, err := archiveDir(context.Background(), r, &buf)
	if err != nil || files != 3 || skipped != 1 {
		t.Fatalf("files %d skipped %d err %v", files, skipped, err)
	}
	if strings.Contains(buf.String(), "root:") {
		t.Fatal("followed the link")
	}
}

// A record's state never goes back, so a spawn or reconcile that read it
// before a destroy began cannot make it active again.
func TestRecordStateOnlyMovesForward(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{Agents: []AgentRecord{{Name: "s-aaaaaa", Template: "session", State: AgentDestroying, Dir: "sessions/s-aaaaaa"}}})
	if _, err := e.b.workers.update("s-aaaaaa", func(r *AgentRecord) { r.State = AgentActive }); !errors.Is(err, errStateBackwards) {
		t.Fatalf("%v", err)
	}
	if r, _ := e.b.workers.record("s-aaaaaa"); r.State != AgentDestroying {
		t.Fatalf("%+v", r)
	}
}

// Reconcile keeps a spawn that has a hub record, with its grants.
func TestReconcileKeepsASpawnWithARecord(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{Agents: []AgentRecord{{Name: "s-aaaaaa", Template: "session", State: AgentSpawning, Dir: "sessions/s-aaaaaa"}}})
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: "s-aaaaaa", Phase: "running", ContainerStatus: "Up 1 minute"}}
	if !e.b.ReconcileAgents(context.Background()) {
		t.Fatal("not settled")
	}
	if r, _ := e.b.workers.record("s-aaaaaa"); r.State != AgentActive {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.b.rules.MayObtainRule("s-aaaaaa", "s-aaaaaa", "db", "read"); !ok {
		t.Fatal("no grants")
	}
	// A hub that cannot be read leaves it unsettled, to be tried again.
	e = newSpawnEnv(t, AgentsState{Agents: []AgentRecord{{Name: "s-bbbbbb", Template: "session", State: AgentSpawning, Dir: "sessions/s-bbbbbb"}}})
	e.rt.listErr = errors.New("hub down")
	if e.b.ReconcileAgents(context.Background()) {
		t.Fatal("settled with no hub")
	}
}

// An agent whose template is gone, or whose hub record is gone, can still
// be destroyed: its workspace is kept (no template says otherwise), and a
// missing record is neither backed up nor purged.
func TestDestroyWithoutTemplateOrRecord(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{Agents: []AgentRecord{{Name: "x-aaaaaa", Template: "gone", State: AgentActive, Dir: "old/x-aaaaaa"}}})
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: "x-aaaaaa", Phase: "stopped"}}
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"x-aaaaaa"}`)
	var got DestroyResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !strings.HasPrefix(got.Workspace, "kept") || len(e.rt.purged) != 1 {
		t.Fatalf("%d %s purged %v", rec.Code, rec.Body, e.rt.purged)
	}
	e = newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = nil
	rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`"}`)
	if rec.Code != http.StatusOK || len(e.rt.purged) != 0 || len(e.backups) != 0 || !e.b.isRevoked(name) {
		t.Fatalf("%d %s purged %v backups %v", rec.Code, rec.Body, e.rt.purged, e.backups)
	}
}

// A resume refuses a spawned agent's workspace swapped for a link, even
// with no manager.read_only.
func TestResumeRefusesASwappedSpawnedWorkspace(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: name, Phase: "suspended"}}
	ws := filepath.Join(e.tree, "sessions", name)
	other := filepath.Join(e.tree, "workers", "scratch")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, ws); err != nil {
		t.Fatal(err)
	}
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerResume, `{"worker":"`+name+`"}`)
	if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonWorkspaceUnsafe || len(e.rt.resumed) != 0 {
		t.Fatalf("%d %q resumed %v", rec.Code, rec.Header().Get(ReasonHeader), e.rt.resumed)
	}
}

// A destroy whose caller hangs up still finishes.
func TestDestroyOutlivesItsCaller(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	e.b.destroyAgent(ctx, w, DestroyRequest{Name: name, Stop: true}, operatorActor)
	if r, _ := e.b.workers.record(name); w.Code != http.StatusOK || r.State != AgentDestroyed {
		t.Fatalf("%d %s %+v", w.Code, w.Body, r)
	}
}

// The manager messages and reads only the spawned agents it spawned.
func TestManagerMessagesOnlyItsOwnSpawnedAgents(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true })
	theirs := e.spawn(t, `{"template":"session"}`).Name
	if _, err := e.b.resolveMsgTarget("test-manager", theirs); err == nil {
		t.Fatal("the manager may message the operator's agent")
	}
	if _, err := e.b.resolveListSubject("test-manager", theirs); err == nil {
		t.Fatal("the manager may read the operator's agent's inbox")
	}
	for _, r := range e.b.msgRecipients("test-manager") {
		if strings.Contains(r, theirs) {
			t.Fatalf("recipients name %s", theirs)
		}
	}
	rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager")
	var mine SpawnResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &mine)
	if _, err := e.b.resolveMsgTarget("test-manager", mine.Name); err != nil {
		t.Fatalf("own agent: %v (%d %s)", err, rec.Code, rec.Body)
	}
}

// A file that cannot be written into the new workspace (a planted one)
// refuses the spawn and leaves no workspace behind.
func TestCreateAgentDirCleansUpAFailedWrite(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	files := map[string][]byte{"a/../../x": []byte("y")}
	if _, err := e.b.createAgentDir("sessions/s-aaaaaa", files); err == nil {
		t.Fatal("wrote outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(e.tree, "sessions", "s-aaaaaa")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace left: %v", err)
	}
}

// Reconcile settles only what an earlier broker left, never a spawn this
// broker is running.
func TestReconcileLeavesThisBrokersSpawns(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	tmpl, _ := e.b.workers.template("session")
	if _, err := e.b.workers.reserve(tmpl, operatorActor, "", time.Now(), func(string) bool { return false }, newInstanceID); err != nil {
		t.Fatal(err)
	}
	e.rt.staticPhases = true
	if !e.b.ReconcileAgents(context.Background()) {
		t.Fatal("not settled")
	}
	if st := e.state(); st.Agents[0].State != AgentSpawning {
		t.Fatalf("reconcile touched a live spawn: %+v", st)
	}
}

// The remote proxy's list (registry=1) reads no hub; a chat login of a
// contact may not wake a spawned agent, the operator's login may.
func TestRegistryListAndContactWakeOfASpawnedAgent(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	got := e.spawn(t, `{"template":"session","label":"PR 42"}`)
	e.rt.mu.Lock()
	e.rt.listCalls = 0
	e.rt.mu.Unlock()
	rec := callOperator(t, e.b, http.MethodGet, PathOperatorAgentList+"?all=1&registry=1", "")
	var list AgentListResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	e.rt.mu.Lock()
	calls := e.rt.listCalls
	e.rt.mu.Unlock()
	if calls != 0 || len(list.Agents) != 1 || list.Agents[0].Name != got.Name || list.Agents[0].Phase != "" || list.Agents[0].Label != "PR 42" {
		t.Fatalf("hub reads %d, list %+v", calls, list.Agents)
	}

	e.rt.staticPhases = true
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: got.Name, Phase: "suspended"}}
	body := `{"for":{"worker":"` + got.Name + `","login":"c@x","tier":"contact"}}`
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerResume, body); rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonNotAWorker {
		t.Fatalf("contact wake: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
	if len(e.rt.resumed) != 0 {
		t.Fatalf("resumed %v", e.rt.resumed)
	}
	body = `{"for":{"worker":"` + got.Name + `","login":"op@x","tier":"operator"}}`
	// The fake keeps the record suspended, so only the resume call is checked.
	callOperator(t, e.b, http.MethodPost, PathOperatorWorkerResume, body)
	if len(e.rt.resumed) != 1 || e.rt.resumed[0] != got.Name {
		t.Fatalf("operator wake resumed %v", e.rt.resumed)
	}
	// Destroyed (revoked): a contact still gets the answer of a name that is
	// no worker, not the revoked one.
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+got.Name+`","no_backup":true}`); rec.Code != http.StatusOK {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body)
	}
	body = `{"for":{"worker":"` + got.Name + `","login":"c@x","tier":"contact"}}`
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorWorkerResume, body); rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonNotAWorker {
		t.Fatalf("contact wake of a destroyed agent: %d %q", rec.Code, rec.Header().Get(ReasonHeader))
	}
}

// A remote caller (lever agent remote-verb) spawns through the operator
// socket under its own name: it is the record's creator, it may destroy
// only its own agents and always with a backup, and its spawns are rate
// limited apart from everyone else's. Only the operator socket takes By.
func TestRemoteCallerSpawnAndDestroy(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true; t.MaxInstances = 20; t.MaxRunning = 20 })
	for _, by := range []string{"factory", "ssh:", "ssh:Factory", "ssh:-x", "ssh:a:b", "ssh:" + strings.Repeat("a", 33), "operator", "test-manager"} {
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session","by":"`+by+`"}`)
		if rec.Code != http.StatusBadRequest || len(e.state().Agents) != 0 {
			t.Fatalf("by %q: %d", by, rec.Code)
		}
		rec = callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"s-aaaaaa","by":"`+by+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("destroy by %q: %d", by, rec.Code)
		}
	}
	if rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session","by":"ssh:factory"}`, "test-manager"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the manager set by: %d", rec.Code)
	}
	if rec := callWorker(t, e.b, "/agent/destroy", `{"name":"s-aaaaaa","by":"ssh:factory"}`, "test-manager"); rec.Code != http.StatusBadRequest {
		t.Fatalf("the manager set by on destroy: %d", rec.Code)
	}
	theirs := e.spawn(t, `{"template":"session"}`).Name
	other := e.spawn(t, `{"template":"session","by":"ssh:other"}`).Name
	// Last, so the fake runtime has it running.
	mine := e.spawn(t, `{"template":"session","task":"t","by":"ssh:factory"}`).Name
	if r, _ := e.b.workers.record(mine); r.CreatedBy != "ssh:factory" {
		t.Fatalf("created_by %q", r.CreatedBy)
	}
	for _, name := range []string{theirs, other} {
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","stop":true,"by":"ssh:factory"}`)
		if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonNotYours {
			t.Fatalf("destroy %s: %d", name, rec.Code)
		}
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+mine+`","stop":true,"no_backup":true,"by":"ssh:factory"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("no_backup by a remote caller: %d", rec.Code)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+mine+`","stop":true,"by":"ssh:factory"}`); rec.Code != http.StatusOK {
		t.Fatalf("destroy mine: %d %s", rec.Code, rec.Body)
	}
	if len(e.backups) != 1 || e.backups[0] != mine {
		t.Fatalf("backups %v", e.backups)
	}
	// Its rate limit is its own: ssh:factory used one spawn this minute,
	// ssh:other one; the operator has none.
	var last int
	for range managerSpawnRateLimit {
		last = callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session","by":"ssh:factory"}`).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("spawn %d by ssh:factory: %d", managerSpawnRateLimit+1, last)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session","by":"ssh:other"}`); rec.Code != http.StatusOK {
		t.Fatalf("ssh:other: %d %s", rec.Code, rec.Body)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, `{"template":"session"}`); rec.Code != http.StatusOK {
		t.Fatalf("the operator: %d %s", rec.Code, rec.Body)
	}
}

// Each limited spawner has its own day.
func TestDailySpawnQuotaIsPerSpawner(t *testing.T) {
	b := &Broker{}
	now := time.Now()
	for range managerSpawnDailyLimit {
		if !b.takeDailySpawn("ssh:factory", now) {
			t.Fatal("refused under the quota")
		}
	}
	if b.takeDailySpawn("ssh:factory", now) {
		t.Fatal("over the quota")
	}
	if !b.takeDailySpawn("test-manager", now) {
		t.Fatal("the manager shares a remote caller's day")
	}
	if !b.takeDailySpawn("ssh:factory", now.Add(24*time.Hour)) {
		t.Fatal("a new day is still refused")
	}
}
