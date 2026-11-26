package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// Spawned agents: the broker draws a fresh name for a template, records it,
// grants it the template's capabilities, creates its workspace with its
// lever skill and starts it fresh. Destroy cuts its authority (the policy
// entry, a permanent revoke of the name), backs up its conversation,
// archives, removes or keeps its workspace and deletes its hub record. A
// name is never drawn twice, so no credential of a destroyed agent ever
// names a live one.

// MaxAgentLabel bounds a spawned agent's label, in characters.
const MaxAgentLabel = 60

// spawnBodyLimit bounds a spawn body: the task is checked by
// scion.CheckTask, and a task file by MaxTaskFileBytes.
const spawnBodyLimit = MaxTaskFileBytes + 4<<10

// SpawnRequest is the body of a spawn. The caller picks only these; the
// name, workspace, image, grants and resources come from the template.
type SpawnRequest struct {
	Template string `json:"template"`
	Task     string `json:"task,omitempty"`
	Label    string `json:"label,omitempty"`
	// Memory is the memory limit to create it with, one of the template's
	// sizes ("8GiB"); empty: the template's default.
	Memory string `json:"memory,omitempty"`
	// By names a remote caller the operator socket speaks for (see
	// ValidRemoteCaller): it is recorded as the agent's creator and is the
	// key of its rate limits. Operator socket only.
	By string `json:"by,omitempty"`
}

// SpawnResponse answers a spawn.
type SpawnResponse struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	Label    string `json:"label,omitempty"`
	Phase    string `json:"phase"`
	Memory   string `json:"memory,omitempty"`
	// Restored is what a restore seeded (archives.go).
	Restored *RestoreInfo `json:"restored,omitempty"`
}

// DestroyRequest is the body of a destroy.
type DestroyRequest struct {
	Name string `json:"name"`
	// Stop stops a running agent first; without it a running agent is
	// refused.
	Stop bool `json:"stop,omitempty"`
	// NoBackup skips the conversation backup (operator only).
	NoBackup bool `json:"no_backup,omitempty"`
	// By names a remote caller the operator socket speaks for: it may
	// destroy only the agents it spawned, always with a backup. Operator
	// socket only.
	By string `json:"by,omitempty"`
}

// remoteCallerRE: "ssh:" and a key's name. The colon keeps it apart from
// every agent and manager name, which are what CreatedBy holds otherwise.
var remoteCallerRE = regexp.MustCompile(`^ssh:[a-z0-9][a-z0-9-]{0,31}$`)

// ValidRemoteCaller reports whether s names a remote caller, such as
// "ssh:factory" (`lever agent remote-verb --as factory`).
func ValidRemoteCaller(s string) bool { return remoteCallerRE.MatchString(s) }

// checkBy refuses a By that is not a remote caller's name.
func (b *Broker) checkBy(w http.ResponseWriter, verb, by string) bool {
	if by == "" || ValidRemoteCaller(by) {
		return true
	}
	b.audit("agent", operatorActor, "deny", verb+": bad caller name")
	refuse(w, ReasonBadRequest, "bad request: by must be ssh:<name>", http.StatusBadRequest)
	return false
}

// DestroyResponse answers a destroy.
type DestroyResponse struct {
	Name string `json:"name"`
	// Was is the record's phase before destroy (PhaseNone: no record).
	Was string `json:"was"`
	// Transcript is the conversation backup's id ("" when none was taken).
	Transcript string `json:"transcript,omitempty"`
	// Workspace says what was done with the workspace.
	Workspace string `json:"workspace"`
	// Warnings are steps that failed after authority was cut.
	Warnings []string `json:"warnings,omitempty"`
}

// AgentListResponse is the operator's list of templates and spawned agents.
type AgentListResponse struct {
	Templates []TemplateEntry `json:"templates"`
	Agents    []AgentEntry    `json:"agents"`
	// Memory is the memory budget and its use; absent with no budget or
	// when the hub was not read.
	Memory *MemoryTotals `json:"memory,omitempty"`
}

// MemoryTotals is the memory budget for running agents (the manager not
// counted), and how much of it the running and starting agents' limits
// take.
type MemoryTotals struct {
	BudgetBytes int64 `json:"budget_bytes"`
	UsedBytes   int64 `json:"used_bytes"`
	FreeBytes   int64 `json:"free_bytes"`
}

// TemplateEntry is one template with its counts.
type TemplateEntry struct {
	Name         string `json:"name"`
	Prefix       string `json:"prefix"`
	MaxInstances int    `json:"max_instances"`
	MaxRunning   int    `json:"max_running"`
	Instances    int    `json:"instances"`
	Running      int    `json:"running"`
	ManagerMay   bool   `json:"manager_may"`
	// MemorySizes are the sizes a spawn may choose, smallest first, and
	// MemoryDefault the one it gets when it names none.
	MemorySizes   []MemorySize `json:"memory_sizes,omitempty"`
	MemoryDefault string       `json:"memory_default,omitempty"`
}

// AgentEntry is one spawned agent. Phase is a known label from the hub, or
// "" when its record could not be read.
type AgentEntry struct {
	AgentRecord
	Phase string `json:"phase,omitempty"`
	// Orphaned: its template is no longer configured, or its dir_root
	// changed since the spawn; it can only be destroyed.
	Orphaned bool `json:"orphaned,omitempty"`
	// WaitingSince is when the agents loop first saw it waiting at its
	// prompt, as the hub reports it (agent-reported: a hint, never
	// authority). Unset when it is not waiting or was not looked at yet.
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	// QueuedMessages counts the messages the broker holds for it until it
	// runs again.
	QueuedMessages int `json:"queued_messages"`
}

// Operator routes.
const (
	PathOperatorAgentSpawn   = "/operator/agent/spawn"
	PathOperatorAgentDestroy = "/operator/agent/destroy"
	PathOperatorAgentList    = "/operator/agent/list"
)

// decodeStrict refuses unknown fields and trailing data: a field the
// caller may not choose (a name, an image, grants) is refused, not ignored.
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the body")
	}
	return nil
}

// checkLabel refuses rather than cleans: a label goes into the registry,
// the audit log and every list.
func checkLabel(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("the label is not valid UTF-8")
	}
	if utf8.RuneCountInString(s) > MaxAgentLabel {
		return fmt.Errorf("the label is longer than %d characters", MaxAgentLabel)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("the label has a control or formatting character")
		}
	}
	return nil
}

func (b *Broker) handleOperatorAgentSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := decodeStrict(w, r, spawnBodyLimit, &req); err != nil {
		b.audit("agent", operatorActor, "deny", "spawn: bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return
	}
	if !b.checkBy(w, "spawn", req.By) {
		return
	}
	b.spawnAgent(r.Context(), w, req, operatorActor)
}

func (b *Broker) handleOperatorAgentDestroy(w http.ResponseWriter, r *http.Request) {
	var req DestroyRequest
	if err := decodeStrict(w, r, smallBodyLimit, &req); err != nil {
		b.audit("agent", operatorActor, "deny", "destroy: bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return
	}
	if !b.checkBy(w, "destroy", req.By) {
		return
	}
	b.destroyAgent(r.Context(), w, req, operatorActor)
}

func (b *Broker) handleOperatorAgentList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := q.Get("all") == "1"
	// The registry alone, with no hub read: the remote proxy asks every few
	// seconds and reads the hub's phases itself.
	out := b.agentList(r.Context(), all, "", q.Get("registry") != "1")
	// One agent's record (destroyed ones too, with all): the remote proxy's
	// stable link to a destroyed agent, which must not need every tombstone.
	if name := q.Get("name"); name != "" {
		kept := out.Agents[:0]
		for _, a := range out.Agents {
			if a.Name == name {
				kept = append(kept, a)
			}
		}
		out.Agents = kept
	}
	writeJSON(w, out)
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// agentList lists the templates and the spawned agents (destroyed ones only
// with all). createdBy filters on the spawner ("" = every agent). With hub,
// it reads each agent's phase; a failed read leaves the phases empty, and
// so does hub false (the running counts are then zero).
func (b *Broker) agentList(ctx context.Context, all bool, createdBy string, hub bool) AgentListResponse {
	phases := map[string]string{}
	listed := false
	var hubAgents []scion.Agent
	if b.runtime != nil && hub {
		if agents, err := b.runtime.List(ctx, b.instanceProject); err == nil {
			listed, hubAgents = true, agents
			for _, a := range agents {
				p := scion.PhaseLabel(a.Phase)
				if scion.RunningContainerDown(p, a.ContainerStatus) {
					p = phaseRunningContainerDown
				}
				phases[a.Slug] = p
			}
		}
	}
	out := AgentListResponse{Templates: []TemplateEntry{}, Agents: []AgentEntry{}}
	if listed {
		out.Memory = b.memoryTotals(hubAgents)
	}
	counts := map[string]*TemplateEntry{}
	for _, t := range b.workers.templateList() {
		out.Templates = append(out.Templates, TemplateEntry{Name: t.Name, Prefix: t.Prefix, MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning, ManagerMay: t.ManagerMay,
			MemorySizes: slices.Clone(t.Sizes), MemoryDefault: t.DefaultMemory.Name})
	}
	for i := range out.Templates {
		counts[out.Templates[i].Name] = &out.Templates[i]
	}
	// The counts are of every agent, whoever spawned it: the caps are.
	for _, rec := range b.workers.recordList() {
		e := AgentEntry{AgentRecord: rec}
		t := counts[rec.Template]
		if e.Memory == "" && rec.State != AgentDestroyed {
			// A record from before sizes runs with its spec's limit.
			if spec, ok := b.workers.get(rec.Name); ok && spec.MemoryBytes > 0 {
				e.MemoryBytes = spec.MemoryBytes
				e.Memory = fmtMemory(spec.MemoryBytes)
			}
		}
		if rec.State == AgentActive {
			e.WaitingSince = b.agentWatch.waiting(rec.Name)
			e.QueuedMessages = b.queue.count(rec.Name)
		}
		if rec.State != AgentDestroyed {
			e.Phase = phases[rec.Name]
			if e.Phase == "" && listed {
				e.Phase = PhaseNone
			}
			ts, configured := b.workers.template(rec.Template)
			e.Orphaned = t == nil || !configured || !inPlace(rec, ts)
			if t != nil {
				t.Instances++
				if e.Phase == scion.PhaseRunning || comingUp(e.Phase) {
					t.Running++
				}
			}
		} else if !all {
			continue
		}
		if createdBy != "" && rec.CreatedBy != createdBy {
			continue
		}
		out.Agents = append(out.Agents, e)
	}
	return out
}

// captureWriter holds a response so a failed start can be rolled back
// before the caller sees the refusal.
type captureWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter { return &captureWriter{header: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *captureWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(p)
}

// replay writes the captured refusal to w.
func (c *captureWriter) replay(w http.ResponseWriter) {
	for k, v := range c.header {
		w.Header()[k] = v
	}
	if c.code == 0 {
		c.code = http.StatusInternalServerError
	}
	w.WriteHeader(c.code)
	_, _ = w.Write(c.body.Bytes())
}

// spawnAgent is the spawn verb after its caller is authenticated. Every
// refusal before the registry write changes nothing; a failure after it is
// rolled back to a destroyed record.
func (b *Broker) spawnAgent(ctx context.Context, w http.ResponseWriter, req SpawnRequest, actor string) {
	b.spawnAgentWith(ctx, w, req, actor, spawnExtras{})
}

// spawnAgentWith is spawnAgent with a restore's extras: a seed step after
// the workspace is created and before the start.
func (b *Broker) spawnAgentWith(ctx context.Context, w http.ResponseWriter, req SpawnRequest, actor string, x spawnExtras) {
	spawner := "operator"
	if actor != operatorActor {
		spawner = "manager"
	}
	// A remote caller spawns with the operator's templates, under its own
	// name and rate limits. Only the operator socket sets By.
	limited := spawner == "manager"
	if req.By != "" {
		if actor != operatorActor {
			refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
			return
		}
		actor, limited = req.By, true
	}
	t, ok := b.workers.template(req.Template)
	if !ok || (spawner == "manager" && !t.ManagerMay) {
		b.audit("agent", actor, "deny", "spawn: no template "+boundedLogin(req.Template)+" for the "+spawner)
		refuse(w, ReasonNoTemplate, "no template "+boundedLogin(req.Template)+" that the "+spawner+" may spawn from", http.StatusNotFound)
		return
	}
	verb := "spawn from " + t.Name
	if err := checkLabel(req.Label); err != nil {
		b.audit("agent", actor, "deny", verb+": "+err.Error())
		refuse(w, ReasonBadLabel, err.Error(), http.StatusBadRequest)
		return
	}
	size, ok := b.spawnSize(w, actor, verb, t, req.Memory)
	if !ok {
		return
	}
	if !b.taskOK(w, actor, verb, req.Task, "") {
		return
	}
	if limited {
		if ok, wait := b.spawnRate.take(actor, time.Now()); !ok {
			b.audit("agent", actor, "deny", verb+": rate limited")
			refuse(w, ReasonRateLimited, fmt.Sprintf("rate limited: try again in %ds", int(wait.Seconds())+1), http.StatusTooManyRequests)
			return
		}
	}
	if !b.runtimeReady(w) {
		return
	}
	if b.purger == nil || b.tree == "" {
		b.audit("agent", actor, "error", verb+": no purge channel or no tree is wired")
		refuse(w, ReasonRuntimeUnavailable, "spawn unavailable", http.StatusBadGateway)
		return
	}
	// A template already at max_running refuses before a name is drawn, so
	// a busy template burns nothing. Admission checks again at the start.
	if busy, err := b.templateAtMaxRunning(ctx, t); err != nil {
		b.audit("agent", actor, "error", verb+": "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error: could not read the hub; nothing was changed", http.StatusBadGateway)
		return
	} else if busy != nil {
		b.audit("agent", actor, "deny", verb+": "+busy.Error())
		refuse(w, ReasonTemplateBusy, busy.Error(), http.StatusConflict)
		return
	}
	// So is one whose size the budget has no room for.
	if busy, err := b.memoryBusyFor(ctx, "from template "+t.Name, size.Bytes); err != nil {
		b.audit("agent", actor, "error", verb+": "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error: could not read the hub; nothing was changed", http.StatusBadGateway)
		return
	} else if busy != nil {
		b.audit("agent", actor, "deny", verb+": "+busy.Error())
		refuse(w, ReasonMemoryBusy, b.memoryBusyText(busy, actor), http.StatusConflict)
		return
	}
	// Counted last, so only a spawn that passed every other check uses up
	// the day's quota.
	if limited && !b.takeDailySpawn(actor, time.Now()) {
		b.audit("agent", actor, "deny", verb+": daily spawn quota reached")
		refuse(w, ReasonRateLimited, fmt.Sprintf("rate limited: %s may spawn at most %d agents a day; ask the operator", spawnerName(spawner, req.By), managerSpawnDailyLimit), http.StatusTooManyRequests)
		return
	}
	// taken runs under the registry's lock: it must not read the registry.
	rec, err := b.workers.reserve(t, size, actor, req.Label, b.now(), func(name string) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, sup := b.superseded[name]
		return b.revoked[name] || sup || name == b.manager || name == b.managerSlug
	}, newInstanceID)
	if errors.Is(err, errTemplateFull) {
		b.audit("agent", actor, "deny", verb+": "+err.Error())
		refuse(w, ReasonTemplateFull, "template "+t.Name+" is at max_instances ("+fmt.Sprint(t.MaxInstances)+"); destroy one of its agents first. Nothing was changed.", http.StatusConflict)
		return
	}
	if err != nil {
		b.audit("agent", actor, "error", verb+": registry: "+err.Error())
		refuse(w, ReasonRuntimeError, "registry error; nothing was changed", http.StatusInternalServerError)
		return
	}
	name := rec.Name
	b.audit("agent", actor, "allow", verb+": reserved "+name, "label", req.Label, "memory", size.Name, "task_bytes", len(req.Task))
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		// An operator destroy took it first: the record is its to settle.
		b.audit("agent", actor, "deny", verb+": "+name+" was taken by a destroy before its start")
		refuse(w, ReasonBusy, "busy: the new agent "+name+" is being destroyed", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	// Under the lock: a destroy that took the name between the reserve and
	// the lock, or a revoke, wins.
	if cur, ok := b.workers.record(name); !ok || cur.State != AgentSpawning || b.isRevoked(name) {
		if ok && cur.State == AgentSpawning {
			// Revoked meanwhile: undone here, under the lock.
			b.rollbackSpawn(ctx, cur, actor)
		}
		b.audit("agent", actor, "deny", verb+": "+name+" was destroyed or revoked before its start")
		refuse(w, ReasonRevoked, "the new agent "+name+" was destroyed or revoked before its start", http.StatusConflict)
		return
	}
	spec, _ := b.workers.get(name)
	fail := func(status int, reason, msg, detail string) {
		b.audit("agent", actor, "error", verb+": "+name+": "+detail)
		b.rollbackSpawn(ctx, rec, actor)
		refuse(w, reason, msg+"; the spawn was undone", status)
	}
	if x.restoredFrom != "" {
		if rec, err = b.workers.update(name, func(r *AgentRecord) { r.RestoredFrom = x.restoredFrom }); err != nil {
			fail(http.StatusInternalServerError, ReasonRuntimeError, "registry error", err.Error())
			return
		}
	}
	// A hub record under the fresh name would be started in its place.
	if phase, _, err := b.recordOf(ctx, spec); err != nil || phase != "" {
		detail := "the hub already has a record of that name"
		if err != nil {
			detail = "phase: " + err.Error()
		}
		b.audit("agent", actor, "error", verb+": "+name+": "+detail)
		b.rollbackSpawn(ctx, rec, actor)
		refuse(w, ReasonRuntimeError, "runtime error: the hub could not be read, or already holds a record of the drawn name; the spawn was undone. Try again", http.StatusBadGateway)
		return
	}
	var files map[string][]byte
	if t.SkillRel != "" {
		files = map[string][]byte{t.SkillRel: t.Skill}
	}
	id, err := b.createAgentDir(rec.Dir, files)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			fail(http.StatusConflict, ReasonWorkspaceUnsafe, "the new agent's workspace "+rec.Dir+" already exists", err.Error())
			return
		}
		fail(http.StatusForbidden, ReasonWorkspaceUnsafe, "the new agent's workspace could not be created safely", err.Error())
		return
	}
	if rec, err = b.workers.update(name, func(r *AgentRecord) { r.DirID = id }); err != nil {
		fail(http.StatusInternalServerError, ReasonRuntimeError, "registry error", err.Error())
		return
	}
	// The start re-resolves the workspace: it must still be the directory
	// created above, not a link the manager swapped in meanwhile.
	var restored *RestoreInfo
	if parent, dir, err := b.openAgentDir(rec); err != nil {
		fail(http.StatusForbidden, ReasonWorkspaceUnsafe, "the new agent's workspace changed before its start", err.Error())
		return
	} else {
		// A restore seeds it through the same handle.
		var serr error
		if x.seed != nil {
			restored, serr = x.seed(ctx, dir)
		}
		dir.Close()
		parent.Close()
		if serr != nil {
			fail(http.StatusUnprocessableEntity, ReasonArchiveRefused, "the archive could not be restored into the new workspace ("+firstLineOf(serr.Error())+")", serr.Error())
			return
		}
	}
	if b.isRevoked(name) {
		fail(http.StatusConflict, ReasonRevoked, "the new agent was revoked before its start", "revoked")
		return
	}
	b.policyAdd(name, t)
	cw := newCaptureWriter()
	b.startFreshWorker(ctx, cw, spec, req.Task, false, actor)
	if cw.code != http.StatusOK {
		b.audit("agent", actor, "error", verb+": "+name+": the start failed; undoing the spawn")
		b.rollbackSpawn(ctx, rec, actor)
		cw.replay(w)
		return
	}
	if _, err := b.workers.update(name, func(r *AgentRecord) { r.State = AgentActive }); err != nil {
		if errors.Is(err, errStateBackwards) {
			// A destroy began meanwhile: it owns the agent now.
			if b.rules != nil {
				b.rules.RemoveAgent(name)
			}
			b.audit("agent", actor, "deny", verb+": "+name+" is being destroyed")
			refuse(w, ReasonRevoked, "the new agent "+name+" is being destroyed", http.StatusConflict)
			return
		}
		// Running, but the registry still says spawning: a restart's
		// reconcile marks it active once it sees the hub record.
		b.audit("agent", actor, "error", verb+": "+name+": recording it active: "+err.Error())
	}
	b.audit("agent", actor, "allow", verb+": "+name+" running")
	writeJSON(w, SpawnResponse{Name: name, Template: t.Name, Label: req.Label, Phase: scion.PhaseRunning, Memory: size.Name, Restored: restored})
}

// rollbackSpawn undoes a spawn that did not come up, its caller holding
// the worker lock: the record becomes destroyed and the name revoked, and
// any hub record is deleted. The workspace is removed when no record exists
// (it holds only what spawn wrote), and otherwise handled as the template
// says.
func (b *Broker) rollbackSpawn(ctx context.Context, rec AgentRecord, actor string) {
	// Not cut short by a caller that hung up: a half-undone spawn would be
	// left destroying.
	ctx = context.WithoutCancel(ctx)
	if cur, ok := b.workers.record(rec.Name); ok {
		rec = cur
	}
	res := b.teardown(ctx, rec, actor, teardownOpts{noBackup: true, rollback: true})
	if len(res.Warnings) > 0 {
		b.audit("agent", actor, "error", "rollback of "+rec.Name+": "+strings.Join(res.Warnings, "; "))
	}
}

// destroyAgent is the destroy verb after its caller is authenticated. The
// manager may destroy only the agents it spawned.
func (b *Broker) destroyAgent(ctx context.Context, w http.ResponseWriter, req DestroyRequest, actor string) {
	if req.By != "" {
		if actor != operatorActor {
			refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
			return
		}
		actor = req.By
	}
	rec, ok := b.workers.record(req.Name)
	verb := "destroy " + boundedLogin(req.Name)
	if !ok || rec.State == AgentDestroyed {
		b.audit("agent", actor, "deny", verb+": not a spawned agent, or already destroyed")
		refuse(w, ReasonNotSpawned, "no spawned agent "+boundedLogin(req.Name)+" (a configured worker is never destroyed, and a destroyed agent stays destroyed)", http.StatusNotFound)
		return
	}
	if actor != operatorActor {
		// The manager destroys only what it may still reach: an agent of a
		// template that dropped it from spawners is the operator's alone.
		if rec.CreatedBy != actor || (req.By == "" && !b.managerMayReach(rec.Name)) {
			who := "the manager"
			if req.By != "" {
				who = req.By
			}
			b.audit("agent", actor, "deny", verb+": not spawned by "+who)
			refuse(w, ReasonNotYours, "forbidden: "+rec.Name+" was not spawned by "+who, http.StatusForbidden)
			return
		}
		if req.NoBackup {
			b.audit("agent", actor, "deny", verb+": no_backup is the operator's")
			refuse(w, ReasonBadRequest, "no_backup is for the operator only", http.StatusForbidden)
			return
		}
	}
	if !b.runtimeReady(w) {
		return
	}
	if b.purger == nil {
		b.audit("agent", actor, "error", verb+": no purge channel is wired")
		refuse(w, ReasonRuntimeUnavailable, "destroy unavailable", http.StatusBadGateway)
		return
	}
	unlock, ok := b.tryLockWorker(rec.Name)
	if !ok {
		b.audit("agent", actor, "deny", verb+": busy")
		refuse(w, ReasonBusy, "busy: another start, resume, stop or heal of "+rec.Name+" is under way; nothing was destroyed. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	// Read again under the lock: a destroy that finished meanwhile wins.
	if rec, ok = b.workers.record(req.Name); !ok || rec.State == AgentDestroyed {
		refuse(w, ReasonNotSpawned, "already destroyed", http.StatusNotFound)
		return
	}
	// A spawning record whose spawn is under way is locked by it; one left
	// by a restart may be destroyed here (the spawn re-reads it under the
	// lock and stops).
	spec, _ := b.workers.get(rec.Name)
	if spec.Name == "" {
		spec = bareSpec(rec, b.tree)
	}
	phase, cs, err := b.recordOf(ctx, spec)
	if err != nil {
		b.audit("agent", actor, "error", verb+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error: could not read the hub; nothing was destroyed", http.StatusBadGateway)
		return
	}
	if rec.State != AgentDestroying {
		live := phase != "" && !recyclablePhase(phase) || scion.ContainerLive(cs)
		if !live && phase != "" && b.containerRunning != nil {
			if live, err = b.containerRunning(ctx, rec.Name); err != nil {
				b.audit("agent", actor, "error", verb+": container state: "+err.Error())
				refuse(w, ReasonRuntimeError, "runtime error: could not read the state of "+rec.Name+"'s container; nothing was destroyed", http.StatusBadGateway)
				return
			}
		}
		if live && !req.Stop {
			b.audit("agent", actor, "deny", verb+": running")
			refusePhase(w, ReasonRunning, phase, rec.Name+" is running (phase "+phase+"); stop it first or destroy with stop. Nothing was destroyed.", http.StatusConflict)
			return
		}
		if live {
			if err := b.stopAndConfirm(ctx, rec.Name); err != nil {
				b.audit("agent", actor, "error", verb+": stop: "+err.Error())
				refuse(w, ReasonRuntimeError, "runtime error: "+rec.Name+" did not stop; nothing was destroyed. Try again shortly.", http.StatusBadGateway)
				return
			}
			if phase, _, err = b.recordOf(ctx, spec); err != nil {
				refuse(w, ReasonRuntimeError, "runtime error: could not read the hub; nothing was destroyed", http.StatusBadGateway)
				return
			}
		}
	}
	// Once begun, a destroy is not cut short by a caller that hung up.
	res := b.teardown(context.WithoutCancel(ctx), rec, actor, teardownOpts{noBackup: req.NoBackup, phase: phase, havePhase: true})
	// The operator's answer names host paths and carries error text; the
	// manager (in the jail) gets lever's words only, and the audit log keeps
	// the detail.
	jailed := actor != operatorActor && req.By == ""
	if res.err != nil {
		b.audit("agent", actor, "error", verb+": "+res.err.Error())
		msg := res.err.Error()
		if jailed {
			msg = "the destroy of " + rec.Name + " did not finish (" + res.reason + "); the operator can see why in the broker's audit log"
		}
		refuse(w, res.reason, msg, res.status)
		return
	}
	b.audit("agent", actor, "allow", verb+": destroyed (was "+res.Was+"), workspace "+res.Workspace)
	out := res.DestroyResponse
	if jailed {
		out.Workspace = workspaceOutcome(out.Workspace)
		if len(out.Warnings) > 0 {
			out.Warnings = []string{fmt.Sprintf("%d step(s) after the revoke failed; the operator can see them in the broker's audit log", len(out.Warnings))}
		}
	}
	writeJSON(w, out)
}

// workspaceOutcome is a destroy's workspace line without its path: what was
// done, in a word.
func workspaceOutcome(s string) string {
	for _, w := range []string{"archived", "removed", "kept"} {
		if strings.HasPrefix(s, w) {
			return w
		}
	}
	return "see the broker's audit log"
}

// stopAndConfirm stops name and waits until podman shows its container down.
func (b *Broker) stopAndConfirm(ctx context.Context, name string) error {
	if err := b.runtime.Stop(ctx, name, b.instanceProject); err != nil {
		return err
	}
	if b.containerRunning == nil {
		return nil
	}
	for range 20 {
		live, err := b.containerRunning(ctx, name)
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.liveInterval):
		}
	}
	return errors.New("its container is still running")
}

type teardownOpts struct {
	noBackup bool
	// rollback: undoing a spawn. With no hub record the agent never ran, so
	// its workspace holds only what spawn wrote and is removed whatever the
	// template says.
	rollback bool
	// phase is the record's phase when havePhase (read under the lock).
	phase     string
	havePhase bool
}

type teardownResult struct {
	DestroyResponse
	err    error
	reason string
	status int
}

// teardown destroys rec, the caller holding its worker lock. Order:
//  1. the conversation backup, before anything else: a failure stops here
//     with nothing changed;
//  2. the record becomes destroying, saved;
//  3. authority is cut: the policy entry, a permanent revoke (saved), the
//     staged tickets; a revoke that is not saved stops here;
//  4. the workspace, as on_destroy says, and the conversation backup moved
//     into the same archive;
//  5. the hub record and staged ticket are deleted; a failure stops here
//     and a later destroy picks up from step 3;
//  6. the record becomes destroyed.
//
// Every step from 3 on is idempotent.
func (b *Broker) teardown(ctx context.Context, rec AgentRecord, actor string, o teardownOpts) teardownResult {
	res := teardownResult{DestroyResponse: DestroyResponse{Name: rec.Name, Was: PhaseNone}}
	if cur, ok := b.workers.record(rec.Name); !ok || cur.State == AgentDestroyed {
		// Another destroy finished it.
		return res
	}
	stop := func(status int, reason string, err error) teardownResult {
		res.err, res.reason, res.status = err, reason, status
		return res
	}
	phase := o.phase
	if !o.havePhase {
		p, _, err := b.recordOf(ctx, WorkerSpec{Name: rec.Name})
		if err != nil {
			res.Warnings = append(res.Warnings, "could not read the hub: "+err.Error())
			p = "unknown"
		}
		phase = p
	}
	if phase != "" {
		res.Was = phase
	}
	// 1. Backup. A record already being destroyed was backed up before.
	if !o.noBackup && rec.State != AgentDestroying && phase != "" && b.backupConversation != nil {
		id, err := b.capture(ctx, func() (string, error) { return b.backupConversation(ctx, rec.Name) })
		if err != nil {
			return stop(http.StatusBadGateway, ReasonBackupFailed, fmt.Errorf("the conversation backup of %s failed (%v); nothing was destroyed. Destroy with no_backup (`lever agent destroy --no-backup`) to go on without one", rec.Name, err))
		}
		res.Transcript = id
	} else if o.noBackup && !o.rollback {
		b.audit("agent", actor, "allow", "destroy "+rec.Name+": no conversation backup, as asked")
	}
	// 2.
	if rec.State != AgentDestroying {
		var err error
		rec, err = b.workers.update(rec.Name, func(r *AgentRecord) {
			r.State = AgentDestroying
			if res.Transcript != "" {
				r.Transcript = res.Transcript
			}
		})
		if err != nil {
			return stop(http.StatusInternalServerError, ReasonRuntimeError, fmt.Errorf("registry error; nothing was destroyed: %v", err))
		}
	}
	res.Transcript = rec.Transcript
	// 3.
	if b.rules != nil {
		b.rules.RemoveAgent(rec.Name)
	}
	if err := b.revokeSaved(rec.Name); err != nil {
		return stop(http.StatusInternalServerError, ReasonRuntimeError, fmt.Errorf("the revocation of %s was not saved (%v); its record is kept, being destroyed: run destroy again", rec.Name, err))
	}
	if b.tickets != nil {
		b.tickets.DropFor(rec.Name)
	}
	// 4.
	if rec.Workspace == "" {
		action := WorkspaceActionOf(b.workers, rec)
		if o.rollback && phase == "" {
			action = "remove"
		}
		done, warn := b.workspaceOnDestroy(ctx, rec, action)
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
		if r2, err := b.workers.update(rec.Name, func(r *AgentRecord) { r.Workspace = done }); err == nil {
			rec = r2
		}
	}
	res.Workspace = rec.Workspace
	// 4b. The conversation backup joins the archive (archives.go).
	if !o.rollback {
		if warn := b.archiveConversation(rec); warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}
	// 5.
	if phase != "" {
		ticketErr, err := b.purger.PurgeWorker(ctx, rec.Name)
		if err != nil {
			return stop(http.StatusBadGateway, ReasonRuntimeError, fmt.Errorf("the hub record of %s was not deleted, or its delete was not confirmed (%v); its authority is cut and it is kept as being destroyed: run destroy again", rec.Name, err))
		}
		if ticketErr != nil {
			res.Warnings = append(res.Warnings, "removing the staged ticket: "+ticketErr.Error())
		}
	}
	// 6.
	if _, err := b.workers.update(rec.Name, func(r *AgentRecord) {
		r.State = AgentDestroyed
		r.DestroyedAt = b.now().UTC()
	}); err != nil {
		return stop(http.StatusInternalServerError, ReasonRuntimeError, fmt.Errorf("everything is done but the registry did not record %s as destroyed (%v): run destroy again", rec.Name, err))
	}
	b.pruneAgentState(rec.Name)
	return res
}

// WorkspaceActionOf is the template's on_destroy for rec; keep when the
// template is gone (nothing is removed on a guess).
func WorkspaceActionOf(r *agentRegistry, rec AgentRecord) string {
	if t, ok := r.template(rec.Template); ok && t.OnDestroy != "" {
		return t.OnDestroy
	}
	return "keep"
}

// workspaceOnDestroy archives, removes or keeps rec's workspace and returns
// what was done. A failure keeps the workspace and says why.
func (b *Broker) workspaceOnDestroy(ctx context.Context, rec AgentRecord, action string) (done, warn string) {
	if action == "keep" {
		return "kept at " + rec.Dir, ""
	}
	parent, dir, err := b.openAgentDir(rec)
	if errors.Is(err, fs.ErrNotExist) {
		return "gone (not found at " + rec.Dir + ")", ""
	}
	if err != nil {
		return "kept at " + rec.Dir, "the workspace was not " + action + "d: " + err.Error()
	}
	defer parent.Close()
	defer dir.Close()
	_, name := splitAgentDir(rec.Dir)
	if action == "archive" {
		dst, err := b.archiveAgentDir(ctx, dir, rec)
		if err != nil {
			return "kept at " + rec.Dir, "the workspace was not archived (" + err.Error() + "), so it was kept"
		}
		if err := removeAgentDir(parent, dir, name); err != nil {
			return "archived to " + dst + ", partly removed", "removing the workspace after its archive: " + err.Error()
		}
		return "archived to " + dst, ""
	}
	if err := removeAgentDir(parent, dir, name); err != nil {
		return "partly removed at " + rec.Dir, "removing the workspace: " + err.Error()
	}
	return "removed", ""
}

// revokeSaved revokes agent for good and reports a failed save.
func (b *Broker) revokeSaved(agent string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[agent] = true
	return b.saveRevocationLocked()
}

// pruneAgentState drops a destroyed agent's per-name state. Its lock stays:
// the caller holds it, and the name is never used again.
func (b *Broker) pruneAgentState(name string) {
	b.reenrolMu.Lock()
	delete(b.reenrolLast, name)
	delete(b.reenrolTries, name)
	delete(b.tokenHealLast, name)
	b.reenrolMu.Unlock()
	// Its queue goes with it, not into its archive: its senders are told.
	ctx, cancel := context.WithTimeout(b.background(), time.Minute)
	defer cancel()
	b.dropQueue(ctx, name, "the recipient was destroyed")
}

// ReconcileAgents settles what a broker that stopped mid-spawn or
// mid-destroy left: spawning agents with a hub record become active, those
// without are rolled back; destroying agents are destroyed again. Until it
// has run they are no workers to any route and hold no grants (New), so it
// may run after the broker starts serving.
func (b *Broker) ReconcileAgents(ctx context.Context) (settled bool) {
	if b.runtime == nil {
		return true
	}
	settled = true
	for _, rec := range b.workers.recordList() {
		if rec.State != AgentSpawning && rec.State != AgentDestroying {
			continue
		}
		// Only what an earlier broker left: a spawn or destroy of this one
		// is its caller's to finish.
		if !b.inheritedUnsettled[rec.Name] {
			continue
		}
		unlock, ok := b.tryLockWorker(rec.Name)
		if !ok {
			settled = false
			continue
		}
		func() {
			defer unlock()
			cur, ok := b.workers.record(rec.Name)
			if !ok || cur.State != rec.State {
				return
			}
			phase, _, err := b.recordOf(ctx, WorkerSpec{Name: rec.Name})
			if err != nil {
				settled = false
				b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+err.Error())
				return
			}
			if rec.State == AgentSpawning && phase != "" && !b.isRevoked(rec.Name) {
				if _, err := b.workers.update(rec.Name, func(r *AgentRecord) { r.State = AgentActive }); err != nil {
					b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+err.Error())
					return
				}
				if t, ok := b.workers.template(rec.Template); ok && inPlace(rec, t) {
					b.policyAdd(rec.Name, t)
				}
				b.audit("agent", "broker", "allow", "reconcile "+rec.Name+": spawned before the restart; active")
				return
			}
			res := b.teardown(ctx, cur, "broker", teardownOpts{noBackup: true, rollback: cur.State == AgentSpawning, phase: phase, havePhase: true})
			if res.err != nil {
				settled = false
				b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+res.err.Error())
				return
			}
			b.audit("agent", "broker", "allow", "reconcile "+rec.Name+": destroyed (left "+rec.State+" by a restart)")
		}()
	}
	return settled
}

// reconcileRetry spaces RunReconcile's passes.
const reconcileRetry = 30 * time.Second

// RunReconcile runs ReconcileAgents until every interrupted spawn and
// destroy is settled, or ctx ends.
func (b *Broker) RunReconcile(ctx context.Context) {
	for !b.ReconcileAgents(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileRetry):
		}
	}
}

// policyAdd grants name exactly template t's capabilities.
func (b *Broker) policyAdd(name string, t TemplateSpec) {
	if b.rules != nil {
		b.rules.AddAgent(name, t.Obtain, t.Delegate)
	}
}

// handleAgentSpawn is the manager's spawn, from a template that names the
// manager in spawners. The manager is authenticated before the body is
// read, and a revoked manager is refused.
func (b *Broker) handleAgentSpawn(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireManager(w, r, "agent", "spawn: ")
	if !ok {
		return
	}
	var req wire.AgentSpawnRequest
	if err := decodeStrict(w, r, spawnBodyLimit, &req); err != nil {
		b.audit("agent", caller, "deny", "spawn: bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return
	}
	b.spawnAgent(r.Context(), w, SpawnRequest{Template: req.Template, Task: req.Task, Label: req.Label, Memory: req.Memory}, caller)
}

// handleAgentDestroy is the manager's destroy, of an agent it spawned. The
// conversation is always backed up.
func (b *Broker) handleAgentDestroy(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireManager(w, r, "agent", "destroy: ")
	if !ok {
		return
	}
	var req wire.AgentDestroyRequest
	if err := decodeStrict(w, r, smallBodyLimit, &req); err != nil {
		b.audit("agent", caller, "deny", "destroy: bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return
	}
	b.destroyAgent(r.Context(), w, DestroyRequest{Name: req.Name, Stop: req.Stop}, caller)
}

// handleAgentList lists the templates the manager may spawn from and the
// agents it spawned. Labels are the spawner's own words; phases are labels.
func (b *Broker) handleAgentList(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireManager(w, r, "agent", "list: ")
	if !ok {
		return
	}
	full := b.agentList(r.Context(), false, caller, true)
	out := wire.AgentListResponse{Templates: []wire.AgentTemplate{}, Agents: []wire.SpawnedAgent{}}
	if m := full.Memory; m != nil {
		out.Memory = &wire.MemoryTotals{BudgetBytes: m.BudgetBytes, UsedBytes: m.UsedBytes, FreeBytes: m.FreeBytes}
	}
	for _, t := range full.Templates {
		if t.ManagerMay {
			at := wire.AgentTemplate{Name: t.Name, MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning, Instances: t.Instances, Running: t.Running, MemoryDefault: t.MemoryDefault}
			for _, sz := range t.MemorySizes {
				at.MemorySizes = append(at.MemorySizes, sz.Name)
			}
			out.Templates = append(out.Templates, at)
		}
	}
	for _, a := range full.Agents {
		// Only those it may still reach: not those of a template that
		// dropped it from spawners.
		if !b.managerMayReach(a.Name) {
			continue
		}
		out.Agents = append(out.Agents, wire.SpawnedAgent{Name: a.Name, Template: a.Template, Label: a.Label, State: a.State, Phase: a.Phase, Memory: a.Memory})
	}
	writeJSON(w, out)
}

// memorySizeRE is the shape of a size a spawner names: checked before it is
// compared with the template's sizes or put in a message.
var memorySizeRE = regexp.MustCompile(`^([0-9]{1,7})(MiB|GiB)$`)

// ParseMemorySize reads a spawn's memory size ("8GiB", "1536MiB").
func ParseMemorySize(s string) (int64, bool) {
	m := memorySizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	if m[2] == "GiB" {
		return n << 30, true
	}
	return n << 20, true
}

// spawnSize is the size a spawn asked for: one of t's sizes, matched by
// value, or t's default when it names none.
func (b *Broker) spawnSize(w http.ResponseWriter, actor, verb string, t TemplateSpec, asked string) (MemorySize, bool) {
	if asked == "" {
		return t.DefaultMemory, true
	}
	n, ok := ParseMemorySize(asked)
	if !ok {
		b.audit("agent", actor, "deny", verb+": bad memory size")
		refuse(w, ReasonBadMemory, "bad memory size: want a number and MiB or GiB, such as 8GiB", http.StatusBadRequest)
		return MemorySize{}, false
	}
	size, ok := t.size(n)
	if !ok {
		b.audit("agent", actor, "deny", verb+": memory "+asked+" is not one of its sizes")
		refuse(w, ReasonBadMemory, "template "+t.Name+" has no memory size "+asked+"; its sizes are "+t.sizeNames(), http.StatusBadRequest)
		return MemorySize{}, false
	}
	return size, true
}

// memoryBusyFor reports a memoryBusyError when asked bytes do not fit in
// what the memory budget has free.
func (b *Broker) memoryBusyFor(ctx context.Context, agent string, asked int64) (*memoryBusyError, error) {
	if b.memoryBudget <= 0 {
		return nil, nil
	}
	// Read under the lock, as admitWorker reads, so a start admitted
	// meanwhile is either in the records or in admitting.
	b.admitMu.Lock()
	defer b.admitMu.Unlock()
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		return nil, err
	}
	err = b.memoryFits(agent, asked, b.runningSet(agents, ""))
	var mb *memoryBusyError
	if errors.As(err, &mb) {
		return mb, nil
	}
	return nil, err
}

// memoryTotals is the budget and its use, from the hub's records.
func (b *Broker) memoryTotals(agents []scion.Agent) *MemoryTotals {
	if b.memoryBudget <= 0 {
		return nil
	}
	b.admitMu.Lock()
	used := b.memoryUsed(b.runningSet(agents, ""))
	b.admitMu.Unlock()
	return &MemoryTotals{BudgetBytes: b.memoryBudget, UsedBytes: used, FreeBytes: max(b.memoryBudget-used, 0)}
}

// templateAtMaxRunning reports a templateCapError when t's agents running
// or being admitted already reach its max_running.
func (b *Broker) templateAtMaxRunning(ctx context.Context, t TemplateSpec) (error, error) {
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		return nil, err
	}
	var same []string
	for _, a := range agents {
		if tt, ok := b.workers.templateOfRecord(a.Slug); ok && tt.Name == t.Name && countsAsRunning(a.Phase, a.ContainerStatus) {
			same = append(same, a.Slug)
		}
	}
	b.admitMu.Lock()
	for name := range b.admitting {
		if tt, ok := b.workers.templateOfRecord(name); ok && tt.Name == t.Name && !slices.Contains(same, name) {
			same = append(same, name)
		}
	}
	b.admitMu.Unlock()
	if len(same) < t.MaxRunning {
		return nil, nil
	}
	slices.Sort(same)
	return &templateCapError{agent: "from template " + t.Name, template: t.Name, max: t.MaxRunning, active: same}, nil
}

// managerSpawnDailyLimit bounds the manager's spawns in 24 hours: every
// spawn leaves a tombstone, a revocation entry, a backup and an archive.
const managerSpawnDailyLimit = 200

// takeDailySpawn counts one spawn against who's day: the manager's, or a
// remote caller's. Each has its own day, which starts at its first spawn.
func (b *Broker) takeDailySpawn(who string, now time.Time) bool {
	b.spawnDayMu.Lock()
	defer b.spawnDayMu.Unlock()
	if b.spawnDays == nil {
		b.spawnDays = map[string]*winCount{}
	}
	d := b.spawnDays[who]
	if d == nil || now.Sub(d.start) >= 24*time.Hour {
		d = &winCount{start: now}
		b.spawnDays[who] = d
	}
	if d.n >= managerSpawnDailyLimit {
		return false
	}
	d.n++
	return true
}

// spawnerName names a limited spawner in a refusal.
func spawnerName(spawner, by string) string {
	if by != "" {
		return by
	}
	return "the " + spawner
}

// Operator routes for a remote caller's suspend and resume of its own
// agents (`lever agent remote-verb`). The operator's own verbs are the
// /operator/worker routes.
const (
	PathOperatorAgentSuspend = "/operator/agent/suspend"
	PathOperatorAgentResume  = "/operator/agent/resume"
)

// AgentVerbRequest is the body of PathOperatorAgentSuspend and
// PathOperatorAgentResume: an agent, and the remote caller that spawned it.
type AgentVerbRequest struct {
	Name string `json:"name"`
	By   string `json:"by"`
}

// remoteCallersAgent decodes an AgentVerbRequest and finds its agent: an
// active agent that By spawned. Anything else is refused alike, so the
// caller learns nothing of other agents.
func (b *Broker) remoteCallersAgent(w http.ResponseWriter, r *http.Request, verb string) (WorkerSpec, string, bool) {
	var req AgentVerbRequest
	if err := decodeStrict(w, r, smallBodyLimit, &req); err != nil {
		b.audit("agent", operatorActor, "deny", verb+": bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return WorkerSpec{}, "", false
	}
	if !ValidRemoteCaller(req.By) {
		b.audit("agent", operatorActor, "deny", verb+": bad caller name")
		refuse(w, ReasonBadRequest, "bad request: by must be ssh:<name>", http.StatusBadRequest)
		return WorkerSpec{}, "", false
	}
	rec, ok := b.workers.record(req.Name)
	spec, isWorker := b.workerSpec(req.Name)
	if !ok || !isWorker || rec.State != AgentActive || rec.CreatedBy != req.By {
		b.audit("agent", req.By, "deny", verb+" "+boundedLogin(req.Name)+": not an active agent it spawned")
		refuse(w, ReasonNotSpawned, "no agent "+boundedLogin(req.Name)+" of yours", http.StatusNotFound)
		return WorkerSpec{}, "", false
	}
	return spec, req.By, true
}

func (b *Broker) handleOperatorAgentSuspend(w http.ResponseWriter, r *http.Request) {
	spec, by, ok := b.remoteCallersAgent(w, r, "suspend")
	if !ok || !b.runtimeReady(w) {
		return
	}
	b.haltWorker(w, r.Context(), spec, "suspend", by, func(ctx context.Context, name string) error {
		return b.runtime.Suspend(ctx, name, b.instanceProject)
	})
}

func (b *Broker) handleOperatorAgentResume(w http.ResponseWriter, r *http.Request) {
	spec, by, ok := b.remoteCallersAgent(w, r, "resume")
	if !ok {
		return
	}
	if b.isRevoked(spec.Name) {
		b.audit("agent", by, "deny", "resume "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "agent "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	b.resumeWorker(r.Context(), w, spec, by)
}
