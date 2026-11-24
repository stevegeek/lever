package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// A template whose spawners dropped the manager may since have been given
// wider grants: the manager no longer drives, messages or lists the agents
// it spawned from it.
func TestManagerLosesAgentsOfATemplateThatNoLongerLetsIt(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true })
	rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager")
	var mine SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !e.b.managerMayReach(mine.Name) {
		t.Fatal("the manager cannot reach its own agent")
	}
	e.b.workers.mu.Lock()
	tm := e.b.workers.templates["session"]
	tm.ManagerMay = false
	e.b.workers.templates["session"] = tm
	e.b.workers.mu.Unlock()
	if e.b.managerMayReach(mine.Name) {
		t.Fatal("the manager still reaches an agent of a template that no longer lets it spawn")
	}
	if rec := callWorker(t, e.b, "/worker/suspend", `{"worker":"`+mine.Name+`"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("suspend: %d", rec.Code)
	}
	if rec := callWorker(t, e.b, "/agent/destroy", `{"name":"`+mine.Name+`","stop":true}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body)
	}
	if rec := callWorker(t, e.b, "/agent/list", `{}`, "test-manager"); strings.Contains(rec.Body.String(), mine.Name) {
		t.Fatalf("listed: %s", rec.Body)
	}
}

// The manager's worker list leaves out the spawned agents it may not reach.
func TestManagerWorkerListLeavesOutOthersAgents(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true; t.MaxRunning = 3 })
	theirs := e.spawn(t, `{"template":"session"}`).Name
	rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager")
	var mine SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.rt.mu.Lock()
	e.rt.staticPhases = true
	e.rt.agents[e.b.instanceProject] = []scion.Agent{{Slug: "worker", Phase: "running"}, {Slug: theirs, Phase: "running"}, {Slug: mine.Name, Phase: "running"}}
	e.rt.mu.Unlock()
	rec = callWorker(t, e.b, "/worker/list", `{}`, "test-manager")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, theirs) || !strings.Contains(body, mine.Name) || !strings.Contains(body, `"worker"`) {
		t.Fatalf("%d %s", rec.Code, body)
	}
}

// The manager's destroy answer names no host path; the operator's does.
func TestManagerDestroyAnswerHasNoHostPath(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true })
	rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager")
	var mine SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = callWorker(t, e.b, "/agent/destroy", `{"name":"`+mine.Name+`","stop":true}`, "test-manager")
	var got DestroyResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got.Workspace != "archived" || strings.Contains(rec.Body.String(), e.archives) || strings.Contains(rec.Body.String(), "/") {
		t.Fatalf("the manager's answer: %s", rec.Body)
	}
}

// An agent whose template's dir_root changed since its spawn is driven no
// more: it is listed as orphaned, is no worker and no template: recipient,
// and destroy still finds its old workspace.
func TestMovedDirRootLeavesOnlyDestroy(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	saved := e.state()
	moved := newSpawnEnv(t, saved, func(t *TemplateSpec) { t.DirRoot = "elsewhere" })
	moved.tree = e.tree
	if moved.b.workers.usable(name) {
		t.Fatal("an agent outside its template's dir_root is usable")
	}
	if _, ok := moved.b.workers.activeTemplateOf(name); ok {
		t.Fatal("an agent outside its template's dir_root is a template: recipient")
	}
	for _, a := range moved.b.agentList(context.Background(), false, "", false).Agents {
		if a.Name == name && !a.Orphaned {
			t.Fatal("not listed as orphaned")
		}
	}
	if spec, _ := moved.b.workers.get(name); spec.WorkspaceSubdir != "sessions/"+name {
		t.Fatalf("destroy would look in %q", spec.WorkspaceSubdir)
	}
}

// Conversation captures run one at a time: a second waits for the first,
// and the daily backup skips while one runs.
func TestCapturesRunOneAtATime(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	var running, most atomic.Int32
	slow := func() (string, error) {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return "id", nil
	}
	done := make(chan struct{}, 3)
	for range 3 {
		go func() {
			_, _ = e.b.capture(context.Background(), slow)
			done <- struct{}{}
		}()
	}
	for range 3 {
		<-done
	}
	if most.Load() != 1 {
		t.Fatalf("%d captures ran at once", most.Load())
	}
	e.b.captureSem <- struct{}{}
	called := false
	e.b.dailyBackup = func(context.Context, string) (string, error) { called = true; return "", nil }
	name := e.spawn(t, `{"template":"session"}`).Name
	e.b.dailyBackupOf(context.Background(), name)
	<-e.b.captureSem
	if called {
		t.Fatal("the daily backup ran during another capture")
	}
}

// A moved agent gets no grants when the broker starts.
func TestMovedAgentGetsNoGrantsAtStart(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	name := e.spawn(t, `{"template":"session"}`).Name
	moved := newSpawnEnv(t, e.state(), func(t *TemplateSpec) { t.DirRoot = "elsewhere" })
	if _, ok := moved.b.rules.MayObtainRule(name, name, "db", "read"); ok {
		t.Fatal("a moved agent holds its template's grant")
	}
	again := newSpawnEnv(t, e.state())
	if _, ok := again.b.rules.MayObtainRule(name, name, "db", "read"); !ok {
		t.Fatal("an agent in place lost its grant")
	}
}
