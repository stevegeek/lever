package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
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

// TicketStager delivers a worker's marshalled enrolment envelope
// (wire.Bootstrap) to the guest location only that worker's container
// mounts. Production is jail.StageWorkerTicket over the jail runner; tests
// record. The broker runs on the host, so this is the ONE way a worker
// ticket leaves the broker process.
type TicketStager interface {
	StageWorkerTicket(ctx context.Context, worker string, payload []byte) error
}

// Where a worker finds its staged ticket INSIDE its container: the broker
// mounts the guest ticket directory (WorkerSpec.TicketDir) read-only at
// workerTicketMount and points lever-agent boot at it through
// workerTicketEnv (cmd/lever-agent's $LEVER_BOOTSTRAP; the pre-start hook
// also probes the mount itself). The manager's container has no such mount.
const (
	workerTicketMount = "/run/lever"
	workerTicketPath  = workerTicketMount + "/bootstrap.json"
	workerTicketEnv   = "LEVER_BOOTSTRAP"
)

// WorkerSpec is the config-derived, path-authoritative description of one worker.
// The broker never accepts any of these from the manager; they come from config.
type WorkerSpec struct {
	Name            string // worker identity (== scion agent slug within the instance project)
	WorkspaceSubdir string // relative --workspace: path RELATIVE to the project root, e.g. "workers/worker" — scion mounts this subtree at /workspace
	HostWorkspace   string // host path to the same subdir, e.g. <tree>/workers/worker; created tree-confined before start (ensureWorkspaceDir; scion's guard requires it to exist)
	// TicketDir is the GUEST path of this worker's ticket directory
	// (jail.WorkerTicketDir: under the run user's XDG_RUNTIME_DIR, outside
	// every container's default view), where TicketStager writes
	// bootstrap.json and which the worker's container mounts read-only at
	// workerTicketMount. Empty ⇒ the worker cannot be dispatched.
	TicketDir string
	Image     string // effective agent image
	Model     string // effective LLM model; empty ⇒ no --model, scion decides
	// InstructionsPath is the HOST path of this worker's standing-instructions
	// file, "" when it has none. Config-authoritative like the rest of the spec
	// (the manager cannot choose it), but its CONTENT is read at dispatch, not
	// baked here: the text is create-time material for the agent, so reading it
	// late lets an edit reach the next fresh start without a broker restart.
	InstructionsPath string
	APIKey           bool // true ⇒ api-key LLM mode for this worker
}

// workerSpec looks up a declared worker by name (its cert CN, which is also
// its scion slug).
func (b *Broker) workerSpec(name string) (WorkerSpec, bool) {
	s, ok := b.workers[name]
	return s, ok
}

// identity resolves an agent name to its (cert CN, scion slug) pair. The
// manager answers to its cert CN or its scion slug (the app name — distinct,
// see IdentityConfig.ManagerSlug); a declared worker's CN IS its slug. A caller that
// needs a strict CN (not an alias) compares the returned cn with its input.
func (b *Broker) identity(name string) (cn, slug string, isManager, ok bool) {
	if name == b.manager || name == b.managerSlug {
		return b.manager, b.managerSlug, true, true
	}
	if spec, ok := b.workerSpec(name); ok {
		return spec.Name, spec.Name, false, true
	}
	return "", "", false, false
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

// requireManagerWorker is the shared preamble of the worker dispatch routes:
// authenticate the caller as the manager, THEN decode the body into req (so
// an unauthenticated caller gets 403, never 400 — matching /msg), then
// authorize the named worker against config and check the
// runtime is wired. Returns the resolved spec, or writes 403/400/502.
func (b *Broker) requireManagerWorker(w http.ResponseWriter, r *http.Request, req any, worker func() string) (WorkerSpec, bool) {
	// A revoked manager cannot dispatch or tear down workers. Dispatching a worker
	// is a stronger steering primitive than messaging (it spawns a fresh,
	// fully-capable agent), so revocation must cut it too — otherwise revoke
	// leaves the loudest channel open.
	caller, ok := b.requireManager(w, r, "worker", "")
	if !ok {
		return WorkerSpec{}, false
	}
	if err := decodeBody(w, r, jailBodyLimit, req); err != nil {
		b.audit("worker", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return WorkerSpec{}, false
	}
	spec, ok := b.workerSpec(worker())
	if !ok {
		b.audit("worker", caller, "deny", "unknown worker: "+worker())
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

// bootstrapFor mints a one-use enrolment ticket for cn and wraps it in the
// envelope lever-agent boot consumes: the ONE construction site for a
// Bootstrap the broker issues (manager and worker alike).
func (b *Broker) bootstrapFor(cn string) (wire.Bootstrap, error) {
	ticket, err := b.tickets.Issue(cn, b.ticketTTL)
	if err != nil {
		return wire.Bootstrap{}, fmt.Errorf("ticket: %w", err)
	}
	return wire.Bootstrap{Ticket: ticket, BrokerCA: b.brokerCAPEM, BrokerURL: b.brokerURL, AgentCN: cn}, nil
}

// stageFreshTicket mints a one-use enrolment ticket for the MANAGER (cn) and
// stages a fresh bootstrap.json under dir — <tree>/.lever, inside the tree
// the manager itself mounts, which is fine for the manager's own ticket —
// via the shared wire.Stage, the same host authority `lever up` uses. Called
// by the auto-re-enrol healer. Workers never take this path: see
// stageWorkerTicket.
func (b *Broker) stageFreshTicket(cn, dir string) error {
	bs, err := b.bootstrapFor(cn)
	if err != nil {
		return err
	}
	root, rel, err := b.treePath(dir)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	if err := wire.Stage(root, rel, bs); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	return nil
}

// stageWorkerTicket mints a one-use enrolment ticket for spec and stages the
// envelope in the GUEST, at spec.TicketDir, through the configured
// TicketStager — never in the instance tree. The tree is the manager's
// whole-tree mount, so a ticket staged there (the pre-0.22 layout,
// <tree>/<dir>/.lever/bootstrap.json) sat readable by the manager between
// staging and the worker's boot, and a manager that redeemed it first held
// the worker's identity: its obtain: grants, its inbox, the operator
// directives addressed to it. The guest runtime dir is visible to no
// container by default; the worker's own container mounts exactly its
// directory (workerTicketVolumes). Called on the fresh-start and resume
// dispatch paths, by the healer and by the admin /worker-ticket route.
func (b *Broker) stageWorkerTicket(ctx context.Context, spec WorkerSpec) error {
	if b.ticketStager == nil {
		return fmt.Errorf("stage: no worker ticket channel is wired (DispatchConfig.Tickets)")
	}
	if spec.TicketDir == "" {
		return fmt.Errorf("stage: worker %q has no ticket directory", spec.Name)
	}
	bs, err := b.bootstrapFor(spec.Name)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("stage: marshal: %w", err)
	}
	if err := b.ticketStager.StageWorkerTicket(ctx, spec.Name, raw); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	return nil
}

// workerTicketVolumes is the container-side half of the channel: the
// worker's guest ticket directory mounted read-only at workerTicketMount,
// and LEVER_BOOTSTRAP pointing lever-agent boot at the file. Create-time
// material (scion keeps a record's volumes and env across resumes).
func workerTicketVolumes(spec WorkerSpec) ([]scion.VolumeMount, map[string]string) {
	return []scion.VolumeMount{{Source: spec.TicketDir, Target: workerTicketMount, ReadOnly: true}},
		map[string]string{workerTicketEnv: workerTicketPath}
}

// treePath splits an absolute directory under the instance tree into the
// confinement anchor the host-side writes need (wire.Stage, ensureWorkspaceDir)
// and the path below it. The anchor is the instance tree: it is the mount
// point, so an agent cannot replace it, while everything under it is
// agent-writable.
//
// With no tree wired (a Broker built directly in a test) it falls back to the
// directory's parent. That still refuses a symlink planted at `.lever` itself
// — the reachable attack, since that is the name inside an agent's own
// workspace — but not one planted at an ancestor. Production always sets Tree
// (brokerctl.decorateConfig), which is what closes the ancestor case too.
func (b *Broker) treePath(dir string) (root, rel string, err error) {
	if b.tree == "" {
		return filepath.Dir(dir), filepath.Base(dir), nil
	}
	rel, err = filepath.Rel(b.tree, dir)
	if err != nil {
		return "", "", fmt.Errorf("dir %q is not under the instance tree %q: %w", dir, b.tree, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("dir %q escapes the instance tree %q", dir, b.tree)
	}
	return b.tree, rel, nil
}

// ensureWorkspaceDir creates spec.HostWorkspace (scion's guard requires the
// --workspace subdir to exist before start) confined to the instance tree.
// The path is agent-writable and the broker runs as the operator: a plain
// os.MkdirAll would follow a `<tree>/workers` the manager replaced with a
// symlink to an absolute host path and create directories there. The walk
// (refuseEscapingDir) names the refusal with fsutil.ErrEscapesTree; the
// mkdir itself goes through an os.Root at the tree, which re-checks every
// component at the syscall, so a swap between the walk and the mkdir is
// refused rather than followed. A symlink that stays inside the tree (an
// operator's own layout) is followed, as before.
func (b *Broker) ensureWorkspaceDir(spec WorkerSpec) error {
	root, rel, err := b.treePath(spec.HostWorkspace)
	if err != nil {
		return err
	}
	if b.tree == "" {
		// No tree wired (a Broker built directly in a test): the workspace's
		// parent may not exist yet either, so anchor at the nearest existing
		// ancestor and create everything below it.
		root, rel, err = existingAnchor(spec.HostWorkspace)
		if err != nil {
			return err
		}
	}
	if err := refuseEscapingDir(root, rel); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.MkdirAll(rel, 0o755)
}

// existingAnchor splits dir at its nearest existing PROPER ancestor: root
// exists, rel (at least dir's base name) is what lies below it.
func existingAnchor(dir string) (root, rel string, err error) {
	root = filepath.Dir(dir)
	for {
		if _, err := os.Stat(root); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", "", fmt.Errorf("no existing ancestor for %q", dir)
		}
		root = parent
	}
	if rel, err = filepath.Rel(root, dir); err != nil {
		return "", "", err
	}
	return root, rel, nil
}

// refuseEscapingDir walks rel below root one component at a time (the
// directory-shaped counterpart of fsutil's confineInTree): a symlink
// component may resolve only inside the real root, else — or when it
// dangles — the walk fails with fsutil.ErrEscapesTree; an existing component
// that is not a directory is a plain error. An absent component ends the walk
// (MkdirAll creates the rest).
func refuseEscapingDir(root, rel string) error {
	if rel == "" || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("%q: %w", rel, fsutil.ErrEscapesTree)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	cur := root
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: dangling symbolic link: %w", cur, fsutil.ErrEscapesTree)
			}
			if err != nil {
				return err
			}
			if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
				return fmt.Errorf("%s: symbolic link to %s: %w", cur, resolved, fsutil.ErrEscapesTree)
			}
			if fi, err = os.Stat(cur); err != nil {
				return err
			}
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: not a directory", cur)
		}
	}
	return nil
}

func (b *Broker) handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.requireManagerWorker(w, r, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	// A task past scion's argv cap can never start a worker: scion inlines it
	// into a tmux command capped at 16 KiB (lever#30), and the container
	// would exit 1 with "command too long" behind a generic runtime error.
	// Refuse it here by name, whatever the worker's phase — a running worker
	// would ignore the task, but a manager sending one this size has a bug it
	// needs to hear about, not a silent 200.
	//
	// A flag-shaped task (one that begins with "-") is refused the same way
	// with 400: it is never a task, and lever's start argv keeps it behind
	// `--` regardless — this is the layer that says so by name.
	if err := scion.CheckTask(req.Task); err != nil {
		b.audit("worker", b.manager, "deny", "start "+spec.Name+": "+err.Error())
		status := http.StatusBadRequest
		var tooLong *scion.TaskTooLongError
		if errors.As(err, &tooLong) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
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
	// staging anything (see DispatchConfig.VerifyAgentRole).
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
	if err := b.stageWorkerTicket(ctx, spec); err != nil {
		b.audit("worker", b.manager, "error", "resume "+err.Error())
		http.Error(w, "stage error", http.StatusInternalServerError)
		return
	}
	resume := b.runtime.Resume
	if phase == scion.PhaseError {
		// Only resume --force (scion#895) recovers an error-phase record.
		resume = b.runtime.ResumeForce
	}
	if err := resume(ctx, spec.Name, b.instanceProject); err != nil {
		b.audit("worker", b.manager, "error", "resume "+spec.Name+": "+err.Error())
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

// startFreshWorker provisions an absent worker: mint a one-use ticket, stage it
// in the guest for that worker alone, then scion start with the ticket
// directory mounted into the new container.
func (b *Broker) startFreshWorker(w http.ResponseWriter, r *http.Request, spec WorkerSpec, task string) {
	ctx := r.Context()
	// Read the standing instructions BEFORE any side effect (ticket, env,
	// workspace dir): a config that names a file the host cannot read is an
	// operator error, and it must fail with nothing staged.
	instructions, err := readWorkerInstructions(spec)
	if err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
		http.Error(w, "instructions error", http.StatusInternalServerError)
		return
	}
	// The workspace dir comes BEFORE the ticket: a refused (out-of-tree)
	// workspace must not spend one. Creating the directory is idempotent and
	// harmless on its own.
	if err := b.ensureWorkspaceDir(spec); err != nil {
		if errors.Is(err, fsutil.ErrEscapesTree) {
			b.audit("worker", b.manager, "deny", "start "+spec.Name+": workspace dir: "+err.Error())
			http.Error(w, "forbidden: worker workspace escapes the instance tree", http.StatusForbidden)
			return
		}
		b.audit("worker", b.manager, "error", "start "+spec.Name+": workspace dir: "+err.Error())
		http.Error(w, "workspace error", http.StatusInternalServerError)
		return
	}
	if err := b.stageWorkerTicket(ctx, spec); err != nil {
		b.audit("worker", b.manager, "error", "start "+err.Error())
		http.Error(w, "stage error", http.StatusInternalServerError)
		return
	}
	if spec.APIKey {
		if err := b.runtime.EnvSet(ctx, b.instanceProject, "LEVER_LLM_AUTH", "api-key"); err != nil {
			b.audit("worker", b.manager, "error", "env set: "+err.Error())
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
	}
	volumes, env := workerTicketVolumes(spec)
	if err := b.runtime.Start(ctx, scion.StartOpts{
		Worker: spec.Name, Task: task, Harness: "claude",
		Project: b.instanceProject, WorkspaceSubdir: spec.WorkspaceSubdir,
		Image: spec.Image, Model: spec.Model, APIKey: spec.APIKey,
		Instructions: instructions, Volumes: volumes, Env: env,
	}); err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
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

// readWorkerInstructions loads the worker's standing instructions from the
// host path config resolved for it (WorkerSpec.InstructionsPath), or "" when
// it has none. The content goes to scion on stdin as inline agent config, so
// the file only has to be readable host-side.
func readWorkerInstructions(spec WorkerSpec) (string, error) {
	if spec.InstructionsPath == "" {
		return "", nil
	}
	b, err := os.ReadFile(spec.InstructionsPath)
	if err != nil {
		return "", fmt.Errorf("reading instructions %s: %w", spec.InstructionsPath, err)
	}
	if err := scion.CheckInstructions(string(b)); err != nil {
		return "", fmt.Errorf("instructions %s: %w", spec.InstructionsPath, err)
	}
	return string(b), nil
}

// defaultLiveAttempts/defaultLiveInterval bound waitWorkerLive's post-start
// poll (Broker.liveAttempts/liveInterval; tests shrink them per instance).
const (
	defaultLiveAttempts = 20
	defaultLiveInterval = 500 * time.Millisecond
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
	}, spec.Name, scion.LiveBudget{Attempts: b.liveAttempts, Interval: b.liveInterval, Settle: b.liveSettle})
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
	spec, ok := b.requireManagerWorker(w, r, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	if err := do(r.Context(), spec); err != nil {
		b.audit("worker", b.manager, "error", r.URL.Path+" "+spec.Name+": "+err.Error())
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
		b.audit("worker", b.manager, "error", "list: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	writeJSON(w, wire.WorkerListResponse[scion.Agent]{Agents: agents})
}
