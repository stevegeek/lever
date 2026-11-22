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
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/fsutil"
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
}

// SpawnResponse answers a spawn.
type SpawnResponse struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	Label    string `json:"label,omitempty"`
	Phase    string `json:"phase"`
}

// DestroyRequest is the body of a destroy.
type DestroyRequest struct {
	Name string `json:"name"`
	// Stop stops a running agent first; without it a running agent is
	// refused.
	Stop bool `json:"stop,omitempty"`
	// NoBackup skips the conversation backup (operator only).
	NoBackup bool `json:"no_backup,omitempty"`
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
}

// AgentEntry is one spawned agent. Phase is a known label from the hub, or
// "" when its record could not be read.
type AgentEntry struct {
	AgentRecord
	Phase string `json:"phase,omitempty"`
	// Orphaned: its template is no longer configured; it can only be
	// destroyed.
	Orphaned bool `json:"orphaned,omitempty"`
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
	b.spawnAgent(r.Context(), w, req, operatorActor)
}

func (b *Broker) handleOperatorAgentDestroy(w http.ResponseWriter, r *http.Request) {
	var req DestroyRequest
	if err := decodeStrict(w, r, smallBodyLimit, &req); err != nil {
		b.audit("agent", operatorActor, "deny", "destroy: bad body")
		refuse(w, ReasonBadRequest, "bad request: "+firstLineOf(err.Error()), http.StatusBadRequest)
		return
	}
	b.destroyAgent(r.Context(), w, req, operatorActor)
}

func (b *Broker) handleOperatorAgentList(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	writeJSON(w, b.agentList(r.Context(), all, ""))
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// agentList lists the templates and the spawned agents (destroyed ones only
// with all). createdBy filters on the spawner ("" = every agent). A failed
// read of the hub leaves the phases empty.
func (b *Broker) agentList(ctx context.Context, all bool, createdBy string) AgentListResponse {
	phases := map[string]string{}
	if b.runtime != nil {
		if agents, err := b.runtime.List(ctx, b.instanceProject); err == nil {
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
	counts := map[string]*TemplateEntry{}
	for _, t := range b.workers.templateList() {
		out.Templates = append(out.Templates, TemplateEntry{Name: t.Name, Prefix: t.Prefix, MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning, ManagerMay: t.ManagerMay})
	}
	for i := range out.Templates {
		counts[out.Templates[i].Name] = &out.Templates[i]
	}
	// The counts are of every agent, whoever spawned it: the caps are.
	for _, rec := range b.workers.recordList() {
		e := AgentEntry{AgentRecord: rec}
		t := counts[rec.Template]
		if rec.State != AgentDestroyed {
			e.Phase = phases[rec.Name]
			if e.Phase == "" && len(phases) > 0 {
				e.Phase = PhaseNone
			}
			e.Orphaned = t == nil
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
	w.WriteHeader(c.code)
	_, _ = w.Write(c.body.Bytes())
}

// spawnAgent is the spawn verb after its caller is authenticated. Every
// refusal before the registry write changes nothing; a failure after it is
// rolled back to a destroyed record.
func (b *Broker) spawnAgent(ctx context.Context, w http.ResponseWriter, req SpawnRequest, actor string) {
	spawner := "operator"
	if actor != operatorActor {
		spawner = "manager"
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
	if !b.taskOK(w, actor, verb, req.Task, "") {
		return
	}
	if spawner == "manager" {
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
	// taken runs under the registry's lock: it must not read the registry.
	rec, err := b.workers.reserve(t, actor, req.Label, b.now(), func(name string) bool {
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
	b.audit("agent", actor, "allow", verb+": reserved "+name, "label", req.Label, "task_bytes", len(req.Task))
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		// A fresh name: nothing else can hold it.
		b.rollbackSpawn(ctx, rec, actor, false)
		refuse(w, ReasonBusy, "busy", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	spec, _ := b.workers.get(name)
	fail := func(status int, reason, msg, detail string) {
		b.audit("agent", actor, "error", verb+": "+name+": "+detail)
		b.rollbackSpawn(ctx, rec, actor, false)
		refuse(w, reason, msg+"; the spawn was undone", status)
	}
	// A hub record under the fresh name would be started in its place.
	if phase, _, err := b.recordOf(ctx, spec); err != nil || phase != "" {
		detail := "the hub already has a record of that name"
		if err != nil {
			detail = "phase: " + err.Error()
		}
		b.audit("agent", actor, "error", verb+": "+name+": "+detail)
		b.rollbackSpawn(ctx, rec, actor, false)
		refuse(w, ReasonRuntimeError, "runtime error: the hub could not be read, or already holds a record of the drawn name; the spawn was undone. Try again", http.StatusBadGateway)
		return
	}
	id, err := b.createAgentDir(rec.Dir)
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
	if t.SkillRel != "" {
		if err := b.writeAgentFile(path.Join(rec.Dir, t.SkillRel), t.Skill); err != nil {
			fail(http.StatusInternalServerError, ReasonWorkspaceUnsafe, "the agent's lever skill could not be written", err.Error())
			return
		}
	}
	b.policyAdd(name, t)
	cw := newCaptureWriter()
	b.startFreshWorker(ctx, cw, spec, req.Task, false, actor)
	if cw.code != http.StatusOK {
		b.audit("agent", actor, "error", verb+": "+name+": the start failed; undoing the spawn")
		b.rollbackSpawn(ctx, rec, actor, true)
		cw.replay(w)
		return
	}
	if _, err := b.workers.update(name, func(r *AgentRecord) { r.State = AgentActive }); err != nil {
		// Running, but the registry still says spawning: a restart's
		// reconcile marks it active once it sees the hub record.
		b.audit("agent", actor, "error", verb+": "+name+": recording it active: "+err.Error())
	}
	b.audit("agent", actor, "allow", verb+": "+name+" running")
	writeJSON(w, SpawnResponse{Name: name, Template: t.Name, Label: req.Label, Phase: scion.PhaseRunning})
}

// writeAgentFile writes the broker's file into a new agent's workspace:
// no link on the path, a fresh file renamed over whatever is there.
func (b *Broker) writeAgentFile(rel string, data []byte) error {
	return fsutil.ReplaceInTreeNoLinks(b.tree, rel, data, 0o755, 0o644)
}

// rollbackSpawn undoes a spawn that did not come up: the record becomes
// destroyed and the name revoked, and any hub record is deleted. started
// says the start was tried, so a record may exist. The workspace is
// removed when no record exists (it holds only what spawn wrote), and
// otherwise handled as the template says.
func (b *Broker) rollbackSpawn(ctx context.Context, rec AgentRecord, actor string, started bool) {
	res := b.teardown(ctx, rec, actor, teardownOpts{noBackup: true, rollback: !started})
	if len(res.Warnings) > 0 {
		b.audit("agent", actor, "error", "rollback of "+rec.Name+": "+strings.Join(res.Warnings, "; "))
	}
}

// destroyAgent is the destroy verb after its caller is authenticated. The
// manager may destroy only the agents it spawned.
func (b *Broker) destroyAgent(ctx context.Context, w http.ResponseWriter, req DestroyRequest, actor string) {
	rec, ok := b.workers.record(req.Name)
	verb := "destroy " + boundedLogin(req.Name)
	if !ok || rec.State == AgentDestroyed {
		b.audit("agent", actor, "deny", verb+": not a spawned agent, or already destroyed")
		refuse(w, ReasonNotSpawned, "no spawned agent "+boundedLogin(req.Name)+" (a configured worker is never destroyed, and a destroyed agent stays destroyed)", http.StatusNotFound)
		return
	}
	if actor != operatorActor {
		if rec.CreatedBy != actor {
			b.audit("agent", actor, "deny", verb+": not spawned by the manager")
			refuse(w, ReasonNotYours, "forbidden: "+rec.Name+" was not spawned by the manager", http.StatusForbidden)
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
	res := b.teardown(ctx, rec, actor, teardownOpts{noBackup: req.NoBackup, phase: phase, havePhase: true})
	if res.err != nil {
		b.audit("agent", actor, "error", verb+": "+res.err.Error())
		refuse(w, res.reason, res.err.Error(), res.status)
		return
	}
	b.audit("agent", actor, "allow", verb+": destroyed (was "+res.Was+"), workspace "+res.Workspace)
	writeJSON(w, res.DestroyResponse)
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
	// rollback: a spawn that never started; the workspace holds only what
	// spawn wrote, so it is removed whatever the template says.
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
//  4. the workspace, as on_destroy says;
//  5. the hub record and staged ticket are deleted; a failure stops here
//     and a later destroy picks up from step 3;
//  6. the record becomes destroyed.
//
// Every step from 3 on is idempotent.
func (b *Broker) teardown(ctx context.Context, rec AgentRecord, actor string, o teardownOpts) teardownResult {
	res := teardownResult{DestroyResponse: DestroyResponse{Name: rec.Name, Was: PhaseNone}}
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
		id, err := b.backupConversation(ctx, rec.Name)
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
		if o.rollback {
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
}

// ReconcileAgents settles what a broker that stopped mid-spawn or
// mid-destroy left: spawning agents with a hub record become active, those
// without are rolled back; destroying agents are destroyed again. Until it
// has run they are no workers to any route and hold no grants (New), so it
// may run after the broker starts serving.
func (b *Broker) ReconcileAgents(ctx context.Context) {
	if b.runtime == nil {
		return
	}
	for _, rec := range b.workers.recordList() {
		if rec.State != AgentSpawning && rec.State != AgentDestroying {
			continue
		}
		unlock, ok := b.tryLockWorker(rec.Name)
		if !ok {
			continue
		}
		func() {
			defer unlock()
			phase, _, err := b.recordOf(ctx, WorkerSpec{Name: rec.Name})
			if err != nil {
				b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+err.Error())
				return
			}
			if rec.State == AgentSpawning && phase != "" {
				if _, err := b.workers.update(rec.Name, func(r *AgentRecord) { r.State = AgentActive }); err != nil {
					b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+err.Error())
					return
				}
				if t, ok := b.workers.template(rec.Template); ok {
					b.policyAdd(rec.Name, t)
				}
				b.audit("agent", "broker", "allow", "reconcile "+rec.Name+": spawned before the restart; active")
				return
			}
			res := b.teardown(ctx, rec, "broker", teardownOpts{noBackup: true, rollback: rec.State == AgentSpawning, phase: phase, havePhase: true})
			if res.err != nil {
				b.audit("agent", "broker", "error", "reconcile "+rec.Name+": "+res.err.Error())
				return
			}
			b.audit("agent", "broker", "allow", "reconcile "+rec.Name+": destroyed (left "+rec.State+" by a restart)")
		}()
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
	b.spawnAgent(r.Context(), w, SpawnRequest{Template: req.Template, Task: req.Task, Label: req.Label}, caller)
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
	full := b.agentList(r.Context(), false, caller)
	out := wire.AgentListResponse{Templates: []wire.AgentTemplate{}, Agents: []wire.SpawnedAgent{}}
	for _, t := range full.Templates {
		if t.ManagerMay {
			out.Templates = append(out.Templates, wire.AgentTemplate{Name: t.Name, MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning, Instances: t.Instances, Running: t.Running})
		}
	}
	for _, a := range full.Agents {
		out.Agents = append(out.Agents, wire.SpawnedAgent{Name: a.Name, Template: a.Template, Label: a.Label, State: a.State, Phase: a.Phase})
	}
	writeJSON(w, out)
}
