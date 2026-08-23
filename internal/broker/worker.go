package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// WorkerRuntime is the subset of scion.Client the broker uses to drive worker
// agents host-side. *scion.Client satisfies it; tests inject a fake.
type WorkerRuntime interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Start(ctx context.Context, o scion.StartOpts) error
	Resume(ctx context.Context, worker, project string) error
	// ResumeForce recovers an error-phase record (scion resume --force,
	// scion#895); refuses phase running. Used by the recovery paths.
	ResumeForce(ctx context.Context, worker, project string) error
	Stop(ctx context.Context, worker, project string) error
	Suspend(ctx context.Context, worker, project string) error
	EnvSet(ctx context.Context, projectDir, key, value string) error
	// Message and Inbox ride the same host-side scion client so container
	// pinning/auth never applies.
	Message(ctx context.Context, o scion.MsgOpts) error
	Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error)
}

// WorkerSpec is the config-derived, path-authoritative description of one worker.
// The broker never accepts any of these from the manager; they come from config.
type WorkerSpec struct {
	Name            string // worker identity (== scion agent slug within the instance project)
	WorkspaceSubdir string // relative --workspace: path RELATIVE to the project root, e.g. "workers/worker" — scion mounts this subtree at /workspace
	HostWorkspace   string // host path to the same subdir, e.g. <tree>/workers/worker; MkdirAll'd before start (scion's guard requires it to exist)
	BootstrapDir    string // host path to <tree>/<dir>/.lever (where bootstrap.json is staged)
	Image           string // effective agent image
	APIKey          bool   // true ⇒ api-key LLM mode for this worker
}

func (b *Broker) workerSpec(name string) (WorkerSpec, bool) {
	s, ok := b.workers[name]
	return s, ok
}

// WorkerListResponse is the wire envelope for /worker/list. It stays here
// (not in internal/wire) because its payload is the host-side scion record.
type WorkerListResponse struct {
	Agents []scion.Agent `json:"agents"`
}

// runtimeReady returns true when the scion runtime is wired. When the runtime
// is nil (no LEVER_JAIL_USER/UID env, e.g. a manual `lever broker serve` with
// no prior `lever apply`), it writes a 502 and an audit line and returns false.
// MUST be called after authn/authz: it audits b.manager (correct only
// post-authn) and unauthenticated callers must receive 403, not 502.
func (b *Broker) runtimeReady(w http.ResponseWriter) bool {
	if b.runtime == nil {
		b.audit("worker", b.manager, "error", "runtime not wired")
		http.Error(w, "worker dispatch unavailable", http.StatusBadGateway)
		return false
	}
	return true
}

// requireManagerWorker authenticates the caller as the manager and authorizes the
// requested worker against config. Returns the resolved spec, or writes 403/502.
func (b *Broker) requireManagerWorker(w http.ResponseWriter, r *http.Request, worker string) (WorkerSpec, bool) {
	// A revoked manager cannot dispatch or tear down workers. Dispatching a worker
	// is a stronger steering primitive than messaging (it spawns a fresh,
	// fully-capable agent), so revocation must cut it too — otherwise revoke
	// leaves the loudest channel open.
	caller, ok := b.requireManager(w, r, "worker", "")
	if !ok {
		return WorkerSpec{}, false
	}
	spec, ok := b.workerSpec(worker)
	if !ok {
		b.audit("worker", caller, "deny", "unknown worker: "+worker)
		http.Error(w, "forbidden", http.StatusForbidden)
		return WorkerSpec{}, false
	}
	// Runtime check is last — authn/authz above must fire first so an
	// unauthenticated caller gets 403, not 502.
	if !b.runtimeReady(w) {
		return WorkerSpec{}, false
	}
	return spec, true
}

func (b *Broker) phaseOf(ctx context.Context, spec WorkerSpec) (string, error) {
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Slug == spec.Name {
			return a.Phase, nil
		}
	}
	return "", nil
}

// stage-step sentinels discriminate stageFreshTicket failures by which step
// failed, so callers can map each to its own HTTP status/body. The wrap
// prefixes ("ticket:"/"stage:") also match the healer's existing audit text.
var (
	errStepTicket = errors.New("ticket")
	errStepStage  = errors.New("stage")
)

// stageFreshTicket mints a one-use enrolment ticket for cn and stages a fresh
// bootstrap.json under dir (the same host authority `lever up` uses), via the
// shared wire.Stage — the single construction+deposit path for the enrolment
// envelope. Called on the fresh-start and resume dispatch paths and by the
// auto-re-enrol healer, which heals the manager too — hence (cn, dir), not a
// WorkerSpec.
func (b *Broker) stageFreshTicket(cn, dir string) error {
	ticket, err := b.tickets.Issue(cn, b.ticketTTL)
	if err != nil {
		return fmt.Errorf("%w: %w", errStepTicket, err)
	}
	bs := wire.Bootstrap{Ticket: ticket, BrokerCA: b.brokerCAPEM, BrokerURL: b.brokerURL, AgentCN: cn}
	root, rel, err := b.stagingPath(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", errStepStage, err)
	}
	if err := wire.Stage(root, rel, bs); err != nil {
		return fmt.Errorf("%w: %w", errStepStage, err)
	}
	return nil
}

// stagingPath splits an absolute staging directory into the confinement anchor
// wire.Stage needs and the path below it. The anchor is the instance tree: it is
// the mount point, so an agent cannot replace it, while everything under it is
// agent-writable.
//
// With no tree wired (a Broker built directly in a test) it falls back to the
// staging directory's parent. That still refuses a symlink planted at `.lever`
// itself — the reachable attack, since that is the name inside an agent's own
// workspace — but not one planted at an ancestor. Production always sets Tree
// (brokerctl.decorateConfig), which is what closes the ancestor case too.
func (b *Broker) stagingPath(dir string) (root, rel string, err error) {
	if b.tree == "" {
		return filepath.Dir(dir), filepath.Base(dir), nil
	}
	rel, err = filepath.Rel(b.tree, dir)
	if err != nil {
		return "", "", fmt.Errorf("staging dir %q is not under the instance tree %q: %w", dir, b.tree, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("staging dir %q escapes the instance tree %q", dir, b.tree)
	}
	return b.tree, rel, nil
}

// stageErrorBody maps a stageFreshTicket step error to its 500 response body.
func stageErrorBody(err error) string {
	if errors.Is(err, errStepStage) {
		return "stage error"
	}
	return "ticket error"
}

func (b *Broker) handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.requireManagerWorker(w, r, req.Worker)
	if !ok {
		return
	}
	phase, err := b.phaseOf(r.Context(), spec)
	if err != nil {
		b.audit("worker", b.manager, "error", "phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	switch {
	case phase == scion.PhaseRunning:
		// Already running: a no-op. Any new task in req.Task is intentionally
		// ignored here — the task-mismatch 409 guard on the resume path covers
		// only the non-running branch (a running worker's task is likewise fixed,
		// and there is nothing to resume). To run a new task, purge then re-dispatch.
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
	case phase != "":
		b.resumeExistingWorker(w, r, spec, phase, req.Task)
	default:
		b.startFreshWorker(w, r, spec, req.Task)
	}
}

// resumeExistingWorker brings a non-running record (suspended/stopped/terminal/
// error) back up. It refuses a re-dispatch that carries a NEW task, then stages
// a fresh ticket and resumes (resume --force for an error-phase record).
func (b *Broker) resumeExistingWorker(w http.ResponseWriter, r *http.Request, spec WorkerSpec, phase, task string) {
	ctx := r.Context()
	// Resuming replays the record's ORIGINAL task — scion pins the task at
	// creation and Resume takes no task — so a re-dispatch carrying a NEW task
	// must NOT silently resume the old one. Refuse loudly and point at purge.
	if strings.TrimSpace(task) != "" {
		b.audit("worker", b.manager, "deny", "start "+spec.Name+": task given but worker exists (phase "+phase+")")
		http.Error(w, "worker "+spec.Name+" already exists (phase "+phase+"); its task is fixed at creation. Run `lever worker purge "+spec.Name+"` to start it fresh with a new task, or dispatch with no task to resume.", http.StatusConflict)
		return
	}
	// Refuse a record whose stored role this scion would read as full, BEFORE
	// staging anything (see Config.VerifyAgentRole).
	if err := b.checkAgentRole(ctx, spec.Name); err != nil {
		b.audit("worker", b.manager, "deny", "resume "+spec.Name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Stage a fresh one-use ticket BEFORE resuming (mirrors apply's
	// ensureFreshBootstrap for the manager): a worker resumed after its
	// leaf/ticket lifetime re-enrols on boot, and the previously staged
	// ticket is long spent — without this it wedges into phase=error
	// (live-hit 2026-07-31). Harmless when the leaf is still valid: boot
	// skips enrol and the ticket ages out unspent.
	if err := b.stageFreshTicket(spec.Name, spec.BootstrapDir); err != nil {
		b.audit("worker", b.manager, "error", "resume "+err.Error())
		http.Error(w, stageErrorBody(err), http.StatusInternalServerError)
		return
	}
	resume := b.runtime.Resume
	if phase == scion.PhaseError {
		// Only resume --force (scion#895) recovers an error-phase record.
		resume = b.runtime.ResumeForce
	}
	if err := resume(ctx, spec.Name, b.instanceProject); err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if err := b.waitWorkerLive(ctx, spec); err != nil {
		b.audit("worker", b.manager, "error", "resume "+spec.Name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	b.audit("worker", b.manager, "allow", "resume "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
}

// startFreshWorker provisions an absent worker: mint a one-use ticket, stage the
// worker's OWN bootstrap, then scion start.
func (b *Broker) startFreshWorker(w http.ResponseWriter, r *http.Request, spec WorkerSpec, task string) {
	ctx := r.Context()
	if err := b.stageFreshTicket(spec.Name, spec.BootstrapDir); err != nil {
		http.Error(w, stageErrorBody(err), http.StatusInternalServerError)
		return
	}
	if spec.APIKey {
		if err := b.runtime.EnvSet(ctx, b.instanceProject, "LEVER_LLM_AUTH", "api-key"); err != nil {
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
	}
	if err := os.MkdirAll(spec.HostWorkspace, 0o755); err != nil {
		b.audit("worker", b.manager, "error", "workspace dir: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if err := b.runtime.Start(ctx, scion.StartOpts{
		Worker: spec.Name, Task: task, Harness: "claude",
		Project: b.instanceProject, WorkspaceSubdir: spec.WorkspaceSubdir,
		Image: spec.Image, APIKey: spec.APIKey,
	}); err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if err := b.waitWorkerLive(ctx, spec); err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	b.audit("worker", b.manager, "allow", "start "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
}

// workerLiveAttempts/workerLiveInterval bound waitWorkerLive's post-start poll.
// Package vars (not consts) so tests can shrink them.
var (
	workerLiveAttempts = 20
	workerLiveInterval = 500 * time.Millisecond
)

// waitWorkerLive polls the worker's scion record until it shows Phase=="running"
// AND a live container, or the budget runs out — so a crash-looping worker
// surfaces as an error instead of a false "running" (mirrors apply's
// waitManagerLive). scion's own start/resume success can lie (it reports
// "resumed" for a container whose harness dies moments later), so the observed
// record — not the CLI exit code — is what makes success meaningful.
func (b *Broker) waitWorkerLive(ctx context.Context, spec WorkerSpec) error {
	err := scion.WaitAgentLive(ctx, func(c context.Context) ([]scion.Agent, error) {
		return b.runtime.List(c, b.instanceProject)
	}, spec.Name, workerLiveAttempts, workerLiveInterval)
	if err == nil {
		return nil
	}
	// WaitAgentLive returns ctx.Err() unwrapped on cancellation; pass it through
	// as-is and prefix only the exhaustion error with the worker subject.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("worker %q %w", spec.Name, err)
}

func (b *Broker) workerVerb(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, spec WorkerSpec) error) {
	var req wire.WorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.requireManagerWorker(w, r, req.Worker)
	if !ok {
		return
	}
	if err := do(r.Context(), spec); err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	phase, perr := b.phaseOf(r.Context(), spec)
	if perr != nil {
		phase = "unknown"
	}
	b.audit("worker", b.manager, "allow", r.URL.Path+" "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: phase})
}

func (b *Broker) handleWorkerStop(w http.ResponseWriter, r *http.Request) {
	b.workerVerb(w, r, func(ctx context.Context, s WorkerSpec) error { return b.runtime.Stop(ctx, s.Name, b.instanceProject) })
}
func (b *Broker) handleWorkerSuspend(w http.ResponseWriter, r *http.Request) {
	b.workerVerb(w, r, func(ctx context.Context, s WorkerSpec) error {
		return b.runtime.Suspend(ctx, s.Name, b.instanceProject)
	})
}
func (b *Broker) handleWorkerResume(w http.ResponseWriter, r *http.Request) {
	b.workerVerb(w, r, func(ctx context.Context, s WorkerSpec) error {
		if err := b.checkAgentRole(ctx, s.Name); err != nil {
			return err
		}
		return b.runtime.Resume(ctx, s.Name, b.instanceProject)
	})
}

// checkAgentRole runs the pre-role record guard for one agent, if wired.
func (b *Broker) checkAgentRole(ctx context.Context, agent string) error {
	if b.verifyRole == nil {
		return nil
	}
	return b.verifyRole(ctx, agent)
}

func (b *Broker) handleWorkerList(w http.ResponseWriter, r *http.Request) {
	// A revoked manager cannot enumerate the fleet either (recon that helps a
	// compromised-then-revoked manager) — consistent with /msg/list.
	if _, ok := b.requireManager(w, r, "worker", "list: "); !ok {
		return
	}
	// Runtime check is after the manager-CN check — authz precedes so an
	// unauthenticated caller still gets 403, not 502.
	if !b.runtimeReady(w) {
		return
	}
	agents, err := b.runtime.List(r.Context(), b.instanceProject)
	if err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	writeJSON(w, WorkerListResponse{Agents: agents})
}
