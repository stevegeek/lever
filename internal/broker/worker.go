package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/scion"
)

// workerBootstrap is a broker-local mirror of the bootstrap envelope written to
// <bootstrapDir>/bootstrap.json. JSON tags MUST remain byte-for-byte identical
// to agent.Bootstrap so the agent package's LoadBootstrap can decode it.
type workerBootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

// WorkerRuntime is the subset of scion.Client the broker uses to drive worker
// agents host-side. *scion.Client satisfies it; tests inject a fake.
type WorkerRuntime interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Start(ctx context.Context, o scion.StartOpts) error
	Resume(ctx context.Context, worker, project string) error
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

type workerStartRequest struct {
	Worker string `json:"worker"`
	Task   string `json:"task"`
}

type workerResponse struct {
	Worker string `json:"worker"`
	Phase  string `json:"phase"`
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
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("worker", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return WorkerSpec{}, false
	}
	if caller != b.manager {
		b.audit("worker", caller, "deny", "not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return WorkerSpec{}, false
	}
	// A revoked manager cannot dispatch or tear down workers. Dispatching a worker
	// is a stronger steering primitive than messaging (it spawns a fresh,
	// fully-capable agent), so revocation must cut it too — otherwise revoke
	// leaves the loudest channel open.
	if b.isRevoked(caller) {
		b.audit("worker", caller, "deny", "revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
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

func (b *Broker) handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	var req workerStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.requireManagerWorker(w, r, req.Worker)
	if !ok {
		return
	}
	ctx := r.Context()
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", b.manager, "error", "phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == "running" {
		// Already running: a no-op. Any new task in req.Task is intentionally
		// ignored here — the task-mismatch 409 guard below covers only the
		// non-running branch (a running worker's task is likewise fixed, and
		// there is nothing to resume). To run a new task, purge then re-dispatch.
		writeJSON(w, workerResponse{Worker: spec.Name, Phase: "running"})
		return
	}
	if phase != "" {
		// exists in a non-running state (suspended/stopped/terminal). Resuming
		// replays the record's ORIGINAL task — scion pins the task at creation and
		// Resume takes no task — so a re-dispatch carrying a NEW task must NOT
		// silently resume the old one. Refuse loudly and point at purge.
		if strings.TrimSpace(req.Task) != "" {
			b.audit("worker", b.manager, "deny", "start "+spec.Name+": task given but worker exists (phase "+phase+")")
			http.Error(w, "worker "+spec.Name+" already exists (phase "+phase+"); its task is fixed at creation. Run `lever worker purge "+spec.Name+"` to start it fresh with a new task, or dispatch with no task to resume.", http.StatusConflict)
			return
		}
		if err := b.runtime.Resume(ctx, spec.Name, b.instanceProject); err != nil {
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
		if err := b.waitWorkerLive(ctx, spec); err != nil {
			b.audit("worker", b.manager, "error", "resume "+spec.Name+": "+err.Error())
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		b.audit("worker", b.manager, "allow", "resume "+spec.Name)
		writeJSON(w, workerResponse{Worker: spec.Name, Phase: "running"})
		return
	}
	// phase == "" → absent: mint a one-use ticket, stage the worker's OWN bootstrap, start.
	ticket, err := b.tickets.Issue(spec.Name, b.ticketTTL)
	if err != nil {
		http.Error(w, "ticket error", http.StatusInternalServerError)
		return
	}
	bs := workerBootstrap{Ticket: ticket, BrokerCA: b.brokerCAPEM, BrokerURL: b.brokerURL, AgentCN: spec.Name}
	if err := stageBootstrap(spec.BootstrapDir, bs); err != nil {
		http.Error(w, "stage error", http.StatusInternalServerError)
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
		Worker: spec.Name, Task: req.Task, Harness: "claude",
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
	writeJSON(w, workerResponse{Worker: spec.Name, Phase: "running"})
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
	var lastPhase, lastContainer string
	for attempt := 0; attempt < workerLiveAttempts; attempt++ {
		agents, err := b.runtime.List(ctx, b.instanceProject)
		if err == nil {
			lastPhase, lastContainer = "", ""
			for _, a := range agents {
				if a.Slug == spec.Name {
					lastPhase, lastContainer = a.Phase, a.ContainerStatus
					break
				}
			}
			if lastPhase == "running" && scion.ContainerLive(lastContainer) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(workerLiveInterval):
		}
	}
	return fmt.Errorf("worker %q did not come up (last phase %q, container %q) — scion reported success but the harness is not live", spec.Name, lastPhase, lastContainer)
}

func (b *Broker) workerVerb(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, spec WorkerSpec) error) {
	var req struct {
		Worker string `json:"worker"`
	}
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
	writeJSON(w, workerResponse{Worker: spec.Name, Phase: phase})
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
	b.workerVerb(w, r, func(ctx context.Context, s WorkerSpec) error { return b.runtime.Resume(ctx, s.Name, b.instanceProject) })
}

func (b *Broker) handleWorkerList(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil || caller != b.manager {
		b.audit("worker", caller, "deny", "list: not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// A revoked manager cannot enumerate the fleet either (recon that helps a
	// compromised-then-revoked manager) — consistent with /msg/list.
	if b.isRevoked(caller) {
		b.audit("worker", caller, "deny", "list: revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
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
	writeJSON(w, struct {
		Agents []scion.Agent `json:"agents"`
	}{Agents: agents})
}

// stageBootstrap writes bs to <dir>/bootstrap.json (dir 0700, file 0600).
func stageBootstrap(dir string, bs workerBootstrap) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("stage bootstrap: mkdir: %w", err)
	}
	raw, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("stage bootstrap: marshal: %w", err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: write: %w", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: chmod: %w", err)
	}
	return nil
}

// writeJSON encodes v as JSON to w with Content-Type set.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
