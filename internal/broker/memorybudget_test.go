package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

const gib = int64(1) << 30

// memBroker declares workers a, b and c of 6GiB, 6GiB and 2GiB under a
// memory budget of budget bytes, with the hub showing agents.
func memBroker(t *testing.T, budget int64, agents ...scion.Agent) (*Broker, *fakeRuntime, *bytes.Buffer) {
	t.Helper()
	var specs []WorkerSpec
	for _, w := range []struct {
		n   string
		mem int64
	}{{"a", 6 * gib}, {"b", 6 * gib}, {"c", 2 * gib}} {
		specs = append(specs, WorkerSpec{Name: w.n, WorkspaceSubdir: "workers/" + w.n,
			HostWorkspace: t.TempDir() + "/workers/" + w.n, TicketDir: "/run/user/501/lever/tickets/" + w.n,
			MemoryBytes: w.mem, Resources: scion.Resources{Memory: scionMemory(fmtMemory(w.mem))}})
	}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: agents}}
	var buf bytes.Buffer
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, specs...), withAudit(&buf),
		func(c *Config) { c.Dispatch.MemoryBudget = budget }))
	return b, rt, &buf
}

// A resume that does not fit in what the budget has free is refused, with
// the sizes and the agents holding the memory; once the other agent is
// suspended it is admitted.
func TestMemoryBudgetRefusesResumeUntilSuspendFreesIt(t *testing.T) {
	running := []scion.Agent{
		{Slug: "appname", Phase: "running", ContainerStatus: "Up 1 hour"}, // the manager never counts
		{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		{Slug: "b", Phase: "suspended"},
	}
	b, rt, audit := memBroker(t, 10*gib, running...)
	rec := callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager")
	if rec.Code != http.StatusConflict || len(rt.resumed) != 0 {
		t.Fatalf("status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
	}
	body := rec.Body.String()
	if r := rec.Header().Get(ReasonHeader); r != ReasonMemoryBusy {
		t.Errorf("reason %q", r)
	}
	for _, want := range []string{"agent b was not started", "asks 6GiB", "4GiB of the 10GiB memory budget is free", "a 6GiB", "Nothing was changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
	if !strings.Contains(audit.String(), "decision=deny") {
		t.Fatalf("audit lacks the denial:\n%s", audit.String())
	}

	// Suspended agents count nothing.
	b, rt, _ = memBroker(t, 10*gib,
		scion.Agent{Slug: "a", Phase: "suspended"},
		scion.Agent{Slug: "b", Phase: "suspended"},
		scion.Agent{Slug: "c", Phase: "running", ContainerStatus: "Up 1 minute"})
	rec = callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager")
	if rec.Code != http.StatusOK || len(rt.resumed) != 1 {
		t.Fatalf("after the suspend: status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
	}
}

// A fresh start is admitted exactly up to the budget; the agent being
// admitted is not counted twice.
func TestMemoryBudgetAdmitsUpToTheBudget(t *testing.T) {
	b, rt, _ := memBroker(t, 8*gib, scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"})
	rec := callWorker(t, b, "/worker/start", `{"worker":"c","task":"go"}`, "test-manager")
	if rec.Code != http.StatusOK || len(rt.started) != 1 {
		t.Fatalf("status = %d started=%d (%s)", rec.Code, len(rt.started), rec.Body.String())
	}
	if len(b.admitting) != 0 {
		t.Fatalf("the admission was not released: %v", b.admitting)
	}
	release, err := b.admitWorker(t.Context(), "a")
	if err != nil {
		t.Fatalf("the running agent's own resume: %v", err)
	}
	release()
}

// An admission in progress holds its memory: a second start meanwhile is
// refused.
func TestMemoryBudgetCountsAdmittedStarts(t *testing.T) {
	b, _, _ := memBroker(t, 10*gib)
	release, err := b.admitWorker(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := b.admitWorker(t.Context(), "b"); err == nil || !strings.Contains(err.Error(), "a 6GiB") {
		t.Fatalf("second admission: %v", err)
	}
	if _, err := b.admitWorker(t.Context(), "c"); err != nil {
		t.Fatalf("2GiB fits beside 6GiB in 10GiB: %v", err)
	}
}

// A chat login's wake is told the numbers, never the other agents' names.
func TestMemoryBudgetWakeRefusalIsShort(t *testing.T) {
	b, rt, _ := memBroker(t, 10*gib,
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "b", Phase: "suspended"})
	rec := postWake(t, b, `{"worker":"b","login":"c@x","tier":"operator"}`)
	if rec.Code != http.StatusConflict || len(rt.resumed) != 0 {
		t.Fatalf("status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "6GiB asked, 4GiB free of 10GiB") || strings.Contains(body, "a 6GiB") {
		t.Fatalf("wake refusal %q", body)
	}
}

// No budget: nothing is admitted by memory.
func TestMemoryBudgetOff(t *testing.T) {
	b, _, _ := memBroker(t, 0,
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "b", Phase: "running", ContainerStatus: "Up 1 minute"})
	release, err := b.admitWorker(t.Context(), "c")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func withSizes(t *TemplateSpec) {
	t.Sizes = []MemorySize{{Name: "2GiB", Bytes: 2 * gib, Scion: "2Gi"}, {Name: "4GiB", Bytes: 4 * gib, Scion: "4Gi"}}
	t.DefaultMemory = t.Sizes[0]
	w := t.Worker
	t.Worker = func(name string) WorkerSpec {
		s := w(name)
		s.Resources.Memory, s.MemoryBytes = "2Gi", 2*gib
		return s
	}
}

// A spawn picks one of the template's sizes, matched by value; the size is
// sent to scion at create and kept in the record.
func TestSpawnWithMemorySize(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, withSizes)
	got := e.spawn(t, `{"template":"session","task":"go","memory":"4096MiB"}`)
	if got.Memory != "4GiB" {
		t.Fatalf("response %+v", got)
	}
	if len(e.rt.started) != 1 || e.rt.started[0].Resources.Memory != "4Gi" {
		t.Fatalf("started %+v", e.rt.started)
	}
	st := e.state()
	if len(st.Agents) != 1 || st.Agents[0].Memory != "4GiB" || st.Agents[0].MemoryBytes != 4*gib {
		t.Fatalf("registry %+v", st)
	}
	if s, _ := e.b.workers.get(got.Name); s.MemoryBytes != 4*gib || s.Resources.Memory != "4Gi" {
		t.Fatalf("spec %+v", s)
	}
	// Without --memory: the default.
	got = e.spawn(t, `{"template":"session","task":"go"}`)
	if got.Memory != "2GiB" || e.rt.started[1].Resources.Memory != "2Gi" {
		t.Fatalf("default spawn %+v %+v", got, e.rt.started[1])
	}
}

// A size the template does not offer, or one not in the fixed format, is
// refused before anything is reserved.
func TestSpawnRefusesBadMemory(t *testing.T) {
	for _, m := range []string{"3GiB", "4gb", "4 GiB", "-4GiB", "0GiB", "99999999GiB"} {
		e := newSpawnEnv(t, AgentsState{}, withSizes)
		body, _ := json.Marshal(SpawnRequest{Template: "session", Task: "go", Memory: m})
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, string(body))
		if rec.Code != http.StatusBadRequest || rec.Header().Get(ReasonHeader) != ReasonBadMemory {
			t.Errorf("%q: %d %s", m, rec.Code, rec.Body)
		}
		if len(e.state().Agents) != 0 || len(e.rt.started) != 0 {
			t.Errorf("%q: a refused spawn left %+v", m, e.state())
		}
	}
}

// A spawn the budget has no room for is refused before a name is drawn.
func TestSpawnRefusedByMemoryBudget(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, withSizes)
	e.b.memoryBudget = 5 * gib
	got := e.spawn(t, `{"template":"session","task":"go","memory":"4GiB"}`)
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: got.Name, Phase: "running", ContainerStatus: "Up 1 minute"}}
	body, _ := json.Marshal(SpawnRequest{Template: "session", Task: "go", Memory: "2GiB"})
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, string(body))
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonMemoryBusy ||
		!strings.Contains(rec.Body.String(), "1GiB of the 5GiB memory budget is free") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := len(e.state().Agents); n != 1 {
		t.Fatalf("the refused spawn reserved a name: %d records", n)
	}
	// The list carries each agent's size, the template's sizes and the
	// totals.
	lrec := callOperator(t, e.b, http.MethodGet, PathOperatorAgentList, "")
	var list AgentListResponse
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Memory == nil || list.Memory.BudgetBytes != 5*gib || list.Memory.UsedBytes != 4*gib || list.Memory.FreeBytes != gib {
		t.Fatalf("totals %+v", list.Memory)
	}
	if len(list.Agents) != 1 || list.Agents[0].Memory != "4GiB" {
		t.Fatalf("agents %+v", list.Agents)
	}
	if len(list.Templates) != 1 || list.Templates[0].MemoryDefault != "2GiB" || len(list.Templates[0].MemorySizes) != 2 {
		t.Fatalf("templates %+v", list.Templates)
	}
}

// A record from before sizes runs with its template's default; one with a
// size keeps it across a restart.
func TestSpawnedSpecMemoryOnLoad(t *testing.T) {
	saved := AgentsState{Agents: []AgentRecord{
		{Name: "s-aaaaaa", Template: "session", State: AgentActive, Dir: "sessions/s-aaaaaa"},
		{Name: "s-bbbbbb", Template: "session", State: AgentActive, Dir: "sessions/s-bbbbbb", Memory: "4GiB", MemoryBytes: 4 * gib},
	}}
	e := newSpawnEnv(t, saved, withSizes)
	if s, _ := e.b.workers.get("s-aaaaaa"); s.MemoryBytes != 2*gib || s.Resources.Memory != "2Gi" {
		t.Fatalf("old record spec %+v", s)
	}
	if s, _ := e.b.workers.get("s-bbbbbb"); s.MemoryBytes != 4*gib || s.Resources.Memory != "4Gi" {
		t.Fatalf("sized record spec %+v", s)
	}
	lrec := callOperator(t, e.b, http.MethodGet, PathOperatorAgentList+"?registry=1", "")
	var list AgentListResponse
	_ = json.Unmarshal(lrec.Body.Bytes(), &list)
	for _, a := range list.Agents {
		if a.Name == "s-aaaaaa" && a.Memory != "2GiB" {
			t.Fatalf("old record listed as %+v", a)
		}
	}
	if list.Memory != nil {
		t.Fatalf("a registry-only list has totals: %+v", list.Memory)
	}
}

func TestParseMemorySize(t *testing.T) {
	for s, want := range map[string]int64{"8GiB": 8 * gib, "1536MiB": 1536 << 20, "8Gi": 0, "8GB": 0, "": 0, "08GiB": 8 * gib, "12345678GiB": 0} {
		got, ok := ParseMemorySize(s)
		if got != want || ok != (want > 0) {
			t.Errorf("ParseMemorySize(%q) = %d %v, want %d", s, got, ok, want)
		}
	}
}

// The ticket's sequence in one broker: a resume is refused while the budget
// is full, the other agent is suspended through the broker, and the same
// resume is then admitted.
func TestMemoryBudgetSuspendThenResume(t *testing.T) {
	b, rt, _ := memBroker(t, 10*gib,
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "b", Phase: "suspended"})
	if rec := callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager"); rec.Code != http.StatusConflict {
		t.Fatalf("full: %d %s", rec.Code, rec.Body)
	}
	if rec := callWorker(t, b, "/worker/suspend", `{"worker":"a"}`, "test-manager"); rec.Code != http.StatusOK || len(rt.suspend) != 1 {
		t.Fatalf("suspend: %d %s", rec.Code, rec.Body)
	}
	// The hub now reads a suspended (the fake does not move phases itself).
	rt.mu.Lock()
	rt.agents[testInstanceProject] = []scion.Agent{{Slug: "a", Phase: "suspended"}, {Slug: "b", Phase: "suspended"}}
	rt.mu.Unlock()
	if rec := callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager"); rec.Code != http.StatusOK || len(rt.resumed) != 1 {
		t.Fatalf("after the suspend: %d %s", rec.Code, rec.Body)
	}
}

// An agent with no memory limit counts as nothing, even over a full budget;
// one larger than the whole budget is told it can never start.
func TestMemoryBudgetEdgeRefusals(t *testing.T) {
	b, _, _ := memBroker(t, 4*gib, scion.Agent{Slug: "c", Phase: "running", ContainerStatus: "Up 1 minute"})
	if err := b.memoryFits("x", 0, map[string]bool{"c": true}); err != nil {
		t.Fatalf("an agent with no limit: %v", err)
	}
	_, err := b.admitWorker(t.Context(), "a")
	if err == nil || !strings.Contains(err.Error(), "more than the whole 4GiB memory budget, so it can never start") {
		t.Fatalf("larger than the budget: %v", err)
	}
}

// A running agent whose template is gone still counts its memory.
func TestMemoryBudgetCountsOrphanedAgents(t *testing.T) {
	saved := AgentsState{Agents: []AgentRecord{
		{Name: "s-cccccc", Template: "gone", State: AgentActive, Dir: "old/s-cccccc", Memory: "4GiB", MemoryBytes: 4 * gib},
	}}
	e := newSpawnEnv(t, saved, withSizes)
	e.b.memoryBudget = 5 * gib
	e.rt.agents[testInstanceProject] = []scion.Agent{{Slug: "s-cccccc", Phase: "running", ContainerStatus: "Up 1 minute"}}
	body, _ := json.Marshal(SpawnRequest{Template: "session", Task: "go", Memory: "2GiB"})
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentSpawn, string(body))
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonMemoryBusy {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// The manager is told the names of the agents it may reach only; the
// operator's spawned agents are counted, unnamed. A remote caller gets the
// sizes alone; the operator every name.
func TestMemoryBusyTextPerCaller(t *testing.T) {
	saved := AgentsState{Agents: []AgentRecord{
		{Name: "s-aaaaaa", Template: "session", CreatedBy: operatorActor, State: AgentActive, Dir: "sessions/s-aaaaaa", Memory: "4GiB", MemoryBytes: 4 * gib},
	}}
	e := newSpawnEnv(t, saved, withSizes)
	e.b.memoryBudget = 5 * gib
	err := e.b.memoryFits("s-new", 2*gib, map[string]bool{"s-aaaaaa": true})
	var mb *memoryBusyError
	if !errors.As(err, &mb) {
		t.Fatalf("%v", err)
	}
	if got := e.b.memoryBusyText(mb, operatorActor); !strings.Contains(got, "s-aaaaaa 4GiB") {
		t.Errorf("operator: %q", got)
	}
	if got := e.b.memoryBusyText(mb, "test-manager"); strings.Contains(got, "s-aaaaaa") || !strings.Contains(got, "1 other agent(s)") {
		t.Errorf("manager: %q", got)
	}
	if got := e.b.memoryBusyText(mb, "ssh:factory"); strings.Contains(got, "s-aaaaaa") || strings.Contains(got, "other agent") {
		t.Errorf("remote: %q", got)
	}
}
