package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// WorkerRuntime is the subset of scion.Client the broker uses to drive worker
// agents host-side.
type WorkerRuntime interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Start(ctx context.Context, o scion.StartOpts) error
	Resume(ctx context.Context, worker, project string) error
	// ResumeForce recovers an error-phase record (scion resume --force); it
	// refuses phase running.
	ResumeForce(ctx context.Context, worker, project string) error
	Stop(ctx context.Context, worker, project string) error
	Suspend(ctx context.Context, worker, project string) error
	EnvSet(ctx context.Context, projectDir, key, value string) error
	Message(ctx context.Context, o scion.MsgOpts) error
	Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error)
}

// TicketStager delivers a worker's enrolment envelope to the guest location
// only that worker's container mounts. It is the only way a worker ticket
// leaves the broker process.
type TicketStager interface {
	StageWorkerTicket(ctx context.Context, worker string, payload []byte) error
}

// Inside a worker's container its guest ticket dir is mounted read-only at
// workerTicketMount, and workerTicketEnv points lever-agent boot at it. The
// manager's container has no such mount.
const (
	workerTicketMount = "/run/lever"
	workerTicketPath  = workerTicketMount + "/bootstrap.json"
	workerTicketEnv   = "LEVER_BOOTSTRAP"
)

// WorkerSpec describes one worker. It comes from config only; the broker
// never accepts any of it from the manager.
type WorkerSpec struct {
	Name            string // cert CN == scion agent slug
	WorkspaceSubdir string // --workspace, relative to the project root; scion mounts it at /workspace
	HostWorkspace   string // host path of the same subdir; must exist before start (scion's guard)
	// TicketDir is the guest path of this worker's ticket dir, outside every
	// container's default view; only this worker's container mounts it.
	// Empty ⇒ the worker cannot be dispatched.
	TicketDir string
	Image     string
	Model     string // empty ⇒ no --model, scion decides
	// InstructionsPath is the host path of the standing instructions ("" =
	// none). The manager cannot choose it; its content is read at each fresh
	// start so an edit needs no broker restart.
	InstructionsPath string
	APIKey           bool // true ⇒ api-key LLM mode for this worker
	// Recyclable lets the manager discard this worker's record and start it
	// fresh.
	Recyclable bool
	// Claude is staged in the worker's envelope before every start and
	// resume. nil = none.
	Claude *wire.Claude
	// Shared is mounted at create and checked on the host before every
	// start and resume.
	Shared []SharedMount
	// Resources is sent at a fresh start only; scion keeps it in the record
	// for resumes. Zero = scion's defaults.
	Resources scion.Resources
}

func (b *Broker) workerSpec(name string) (WorkerSpec, bool) {
	s, ok := b.workers[name]
	return s, ok
}

// identity resolves an agent name to its (cert CN, scion slug) pair. The
// manager answers to either; a worker's CN is its slug. A caller that needs a
// strict CN compares the returned cn with its input.
func (b *Broker) identity(name string) (cn, slug string, isManager, ok bool) {
	if name == b.manager || name == b.managerSlug {
		return b.manager, b.managerSlug, true, true
	}
	if spec, ok := b.workerSpec(name); ok {
		return spec.Name, spec.Name, false, true
	}
	return "", "", false, false
}

// runtimeReady writes a 502 when no scion runtime is wired. It MUST be called
// after authn/authz: it audits b.manager, and an unauthenticated caller must
// get 403, not 502.
func (b *Broker) runtimeReady(w http.ResponseWriter) bool {
	if b.runtime == nil {
		b.audit("worker", b.manager, "error", "runtime not wired")
		refuse(w, ReasonRuntimeUnavailable, "worker dispatch unavailable", http.StatusBadGateway)
		return false
	}
	return true
}

// requireManagerWorker authenticates the manager BEFORE decoding the body
// (an unauthenticated caller gets 403, never 400), then authorizes the named
// worker against config and checks the runtime. A revoked manager is refused:
// dispatch spawns a fully capable agent, so revoke must cut it.
func (b *Broker) requireManagerWorker(w http.ResponseWriter, r *http.Request, req any, worker func() string) (WorkerSpec, bool) {
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
	if !b.runtimeReady(w) {
		return WorkerSpec{}, false
	}
	return spec, true
}

// lockWorker takes name's lifecycle lock, waiting as long as ctx allows. The
// holder reads the phase and acts on it with no other lifecycle action of
// that worker in between.
func (b *Broker) lockWorker(ctx context.Context, name string) (func(), error) {
	sem := b.workerSem(name)
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	if b.onWorkerLockWait != nil {
		b.onWorkerLockWait(name)
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Broker) tryLockWorker(name string) (func(), bool) {
	sem := b.workerSem(name)
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

func (b *Broker) workerSem(name string) chan struct{} {
	b.workerLocksMu.Lock()
	defer b.workerLocksMu.Unlock()
	if b.workerLocks == nil {
		b.workerLocks = map[string]chan struct{}{}
	}
	sem := b.workerLocks[name]
	if sem == nil {
		sem = make(chan struct{}, 1)
		b.workerLocks[name] = sem
	}
	return sem
}

func (b *Broker) lockWorkerOrRefuse(w http.ResponseWriter, ctx context.Context, actor, verb, name string) (func(), bool) {
	unlock, err := b.lockWorker(ctx, name)
	if err != nil {
		b.audit("worker", actor, "error", verb+" "+name+": gave up waiting for another start/resume of it: "+err.Error())
		refuse(w, ReasonBusy, "busy: another start or resume of "+name+" is under way", http.StatusServiceUnavailable)
		return nil, false
	}
	return unlock, true
}

func (b *Broker) phaseOf(ctx context.Context, spec WorkerSpec) (string, error) {
	phase, _, err := b.recordOf(ctx, spec)
	return phase, err
}

// recordOf also returns the raw container status. It is agent-settable hub
// text: callers may test it but never echo or audit it.
func (b *Broker) recordOf(ctx context.Context, spec WorkerSpec) (phase, containerStatus string, err error) {
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		return "", "", err
	}
	for _, a := range agents {
		if a.Slug == spec.Name {
			// The hub's phase is agent-settable text: only a known label
			// leaves this function, so no caller echoes the agent's words.
			phase := scion.PhaseLabel(a.Phase)
			if scion.RunningContainerDown(phase, a.ContainerStatus) {
				// The hub keeps a record "running" for minutes after its
				// container died; in that window a resume is a no-op and a
				// message goes nowhere. A blank or "created" status stays
				// running (scion briefly reports "created" for a running
				// record).
				return phaseRunningContainerDown, a.ContainerStatus, nil
			}
			return phase, a.ContainerStatus, nil
		}
	}
	return "", "", nil
}

// phaseRunningContainerDown is not a scion phase: it fails every comparison
// with scion.PhaseRunning, so a message is refused, and resume treats it like
// the error phase it is about to become.
const phaseRunningContainerDown = "running, container down"

// bootstrapFor mints a one-use enrolment ticket for cn and wraps it in the
// envelope lever-agent boot consumes. It is the only place the broker builds a
// Bootstrap.
func (b *Broker) bootstrapFor(cn string, claude *wire.Claude) (wire.Bootstrap, error) {
	ticket, err := b.tickets.Issue(cn, b.ticketTTL)
	if err != nil {
		return wire.Bootstrap{}, fmt.Errorf("ticket: %w", err)
	}
	return wire.Bootstrap{Ticket: ticket, BrokerCA: b.brokerCAPEM, BrokerURL: b.brokerURL, AgentCN: cn, Claude: claude}, nil
}

// stageFreshTicket stages a fresh MANAGER bootstrap.json under dir
// (<tree>/.lever, inside the manager's own mount). Workers never take this
// path: see stageWorkerTicket.
func (b *Broker) stageFreshTicket(cn, dir string) error {
	bs, err := b.bootstrapFor(cn, b.managerClaude)
	if err != nil {
		return err
	}
	root, rel, err := b.treePath(dir)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	raw, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("stage: marshal: %w", err)
	}
	// The manager can write everything below root: the write refuses any
	// symlink on rel and replaces bootstrap.json by rename, never opening
	// the hard link or FIFO the manager may have left there.
	if err := fsutil.ReplaceInTreeNoLinks(root, path.Join(filepath.ToSlash(rel), "bootstrap.json"), raw, 0o700, 0o600); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	return nil
}

// stageWorkerTicket stages a fresh worker envelope in the GUEST at
// spec.TicketDir, never in the instance tree: the manager mounts the whole
// tree, and a manager that redeemed a worker's ticket first would hold that
// worker's identity (its grants, inbox and directives). Only the worker's own
// container mounts its ticket dir.
func (b *Broker) stageWorkerTicket(ctx context.Context, spec WorkerSpec) error {
	if b.ticketStager == nil {
		return fmt.Errorf("stage: no worker ticket channel is wired (DispatchConfig.Tickets)")
	}
	if spec.TicketDir == "" {
		return fmt.Errorf("stage: worker %q has no ticket directory", spec.Name)
	}
	bs, err := b.bootstrapFor(spec.Name, spec.Claude)
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

// workerTicketVolumes is create-time material: scion keeps a record's
// volumes and env across resumes.
func workerTicketVolumes(spec WorkerSpec) ([]scion.VolumeMount, map[string]string) {
	vols := []scion.VolumeMount{{Source: spec.TicketDir, Target: workerTicketMount, ReadOnly: true}}
	for _, m := range spec.Shared {
		vols = append(vols, m.Volume)
	}
	return vols, map[string]string{workerTicketEnv: workerTicketPath}
}

// treePath splits dir into the confinement anchor for host-side writes and
// the path below it. The anchor is the instance tree: it is the mount point,
// so an agent cannot replace it, while everything under it is agent-writable.
//
// With no tree wired (tests) it falls back to the parent, which still refuses
// a symlink at `.lever` itself but not at an ancestor. Production always sets
// Tree, which closes the ancestor case.
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

// ensureWorkspaceDir creates spec.HostWorkspace confined to the instance
// tree. The path is agent-writable and the broker runs as the operator, so a
// plain MkdirAll would follow a symlink the manager planted to a host path.
// The walk names the refusal; the mkdir goes through an os.Root, which
// re-checks every component at the syscall so a swap after the walk is
// refused. An in-tree symlink is followed unless strict.
func (b *Broker) ensureWorkspaceDir(spec WorkerSpec) error {
	root, rel, err := b.treePath(spec.HostWorkspace)
	if err != nil {
		return err
	}
	if b.tree == "" {
		// No tree wired (tests): the parent may not exist yet either.
		root, rel, err = existingAnchor(spec.HostWorkspace)
		if err != nil {
			return err
		}
	}
	strict := len(b.readOnlyDirs) > 0
	if strict && b.tree != "" {
		if err := b.refuseReadOnlyOverlap(spec.Name, rel); err != nil {
			return err
		}
	}
	if err := refuseEscapingDir(root, rel, strict); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if !strict || b.tree == "" {
		return nil
	}
	if b.afterWorkspaceMkdir != nil {
		b.afterWorkspaceMkdir()
	}
	// Strict: walk again now the directory exists, so a swap between the
	// first walk and the mkdir is caught, then check the real path.
	return b.verifyStrictWorkspace(spec)
}

// verifyStrictWorkspace is the manager.read_only check of an existing worker
// workspace: no symlink on its path below the tree, and its real path does
// not overlap a read_only entry. The bind source is re-resolved on every
// container start, so this must run on every resume too. No-op when
// read_only is unset or no tree is wired.
func (b *Broker) verifyStrictWorkspace(spec WorkerSpec) error {
	if len(b.readOnlyDirs) == 0 || b.tree == "" {
		return nil
	}
	root, rel, err := b.treePath(spec.HostWorkspace)
	if err != nil {
		return err
	}
	if err := refuseEscapingDir(root, rel, true); err != nil {
		return err
	}
	realTree, err := filepath.EvalSymlinks(b.tree)
	if err != nil {
		return err
	}
	realWs, err := filepath.EvalSymlinks(spec.HostWorkspace)
	if err != nil {
		return err
	}
	realRel, err := filepath.Rel(realTree, realWs)
	if err != nil || !filepath.IsLocal(realRel) {
		return fmt.Errorf("%s resolves to %s, outside the tree: %w", spec.HostWorkspace, realWs, fsutil.ErrEscapesTree)
	}
	if err := b.refuseReadOnlyOverlap(spec.Name, realRel); err != nil {
		return err
	}
	return b.verifySharedSources(spec)
}

// ErrReadOnlyOverlap refuses a worker workspace that is, contains or lies
// inside a protected directory, which the worker would mount read-write.
// This is the broker's on-disk check, on top of config validation.
var ErrReadOnlyOverlap = errors.New("worker workspace overlaps a protected directory (manager.read_only or a shared folder)")

// refuseReadOnlyOverlap compares case-folded: the host filesystem is usually
// case-insensitive.
func (b *Broker) refuseReadOnlyOverlap(worker, rel string) error {
	for _, ro := range b.readOnlyDirs {
		if fsutil.RelOverlapFold(rel, ro) {
			return fmt.Errorf("worker %q workspace %q and protected directory %q: %w", worker, rel, ro, ErrReadOnlyOverlap)
		}
	}
	return nil
}

// existingAnchor splits dir at its nearest existing proper ancestor.
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

// refuseEscapingDir walks rel below root: a symlink component must resolve
// inside the real root (a dangling one fails too) or the walk fails with
// fsutil.ErrEscapesTree. An absent component ends the walk. noLinks refuses
// every symlink: an in-tree link to a protected directory would hand a worker
// that directory read-write.
func refuseEscapingDir(root, rel string, noLinks bool) error {
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
		if fi.Mode()&fs.ModeSymlink != 0 && noLinks {
			return fmt.Errorf("%s: symbolic link refused (manager.read_only or a shared folder is configured, so this path must be reached through real directories only): %w", cur, fsutil.ErrEscapesTree)
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
	// scion inlines the task into a tmux command capped at 16 KiB, so a
	// longer one fails the container with a generic error: refuse it by name,
	// whatever the phase. A flag-shaped task is refused too (the start argv
	// keeps it behind `--` regardless).
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
	unlock, ok := b.lockWorkerOrRefuse(w, r.Context(), b.manager, "start", spec.Name)
	if !ok {
		return
	}
	defer unlock()
	phase, err := b.phaseOf(r.Context(), spec)
	if err != nil {
		b.audit("worker", b.manager, "error", "phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	switch {
	case phase == scion.PhaseRunning:
		// Already running: a no-op. A new task is ignored; a running worker's
		// task is fixed too.
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
	case phase != "":
		b.resumeExistingWorker(w, r, spec, phase, req.Task)
	default:
		b.startFreshWorker(w, r, spec, req.Task)
	}
}

func (b *Broker) resumeExistingWorker(w http.ResponseWriter, r *http.Request, spec WorkerSpec, phase, task string) {
	ctx := r.Context()
	// scion pins the task at creation and Resume takes none, so a re-dispatch
	// with a new task must not silently resume the old one.
	if strings.TrimSpace(task) != "" {
		b.audit("worker", b.manager, "deny", "start "+spec.Name+": task given but worker exists (phase "+phase+")")
		fresh := "Run `lever worker purge " + spec.Name + "` to start it fresh with a new task"
		if spec.Recyclable {
			fresh = "Run `lever-manager agent recycle " + spec.Name + " --task \"…\"` to start it fresh with a new task (its old conversation is discarded)"
		}
		http.Error(w, "worker "+spec.Name+" already exists (phase "+phase+"); its task is fixed at creation. "+fresh+", or dispatch with no task to resume.", http.StatusConflict)
		return
	}
	// Before staging anything (DispatchConfig.VerifyAgentRole).
	if err := b.checkAgentRole(ctx, spec.Name); err != nil {
		b.audit("worker", b.manager, "deny", "resume "+spec.Name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	b.resumeRecord(ctx, w, spec, phase, b.manager)
}

// resumeRecord brings a non-running worker record back up and answers only
// once the worker is live. It never deletes a record: a purge is the
// operator's decision. actor names the caller in the audit lines.
func (b *Broker) resumeRecord(ctx context.Context, w http.ResponseWriter, spec WorkerSpec, phase, actor string) {
	if comingUp(phase) {
		// Already coming up: a fresh ticket would replace the one the booting
		// worker is about to spend. Only wait.
		if err := b.waitWorkerLive(ctx, spec); err != nil {
			b.audit("worker", actor, "error", "resume "+spec.Name+" (already "+phase+"): "+err.Error())
			refuse(w, ReasonNotLive, err.Error()+". "+workerPurgeHint(spec.Name), http.StatusBadGateway)
			return
		}
		b.audit("worker", actor, "allow", "resume "+spec.Name+" (already "+phase+")")
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
		return
	}
	// Before anything is staged, so a refusal changes nothing.
	release, ok := b.admitOrRefuse(ctx, w, actor, "resume", spec.Name)
	if !ok {
		return
	}
	defer release()
	// A resume re-resolves the workspace bind source, so a worker dir the
	// manager swapped for a link would hand the worker whatever it names.
	if err := b.verifyStrictWorkspace(spec); err != nil {
		if errors.Is(err, ErrSharedFolder) {
			b.audit("worker", actor, "deny", "resume "+spec.Name+": "+err.Error())
			refuse(w, ReasonWorkspaceUnsafe, "forbidden: a shared folder of worker "+spec.Name+" is missing on the host or reached through a symbolic link; the record was kept", http.StatusForbidden)
			return
		}
		if errors.Is(err, fsutil.ErrEscapesTree) || errors.Is(err, ErrReadOnlyOverlap) {
			b.audit("worker", actor, "deny", "resume "+spec.Name+": workspace dir: "+err.Error())
			refuse(w, ReasonWorkspaceUnsafe, "forbidden: worker workspace is reached through a symbolic link or overlaps a protected directory (manager.read_only or a shared folder); the record was kept", http.StatusForbidden)
			return
		}
		b.audit("worker", actor, "error", "resume "+spec.Name+": workspace dir: "+err.Error())
		if errors.Is(err, fs.ErrNotExist) {
			fix := "Ask the operator to restore the directory, or to run `lever worker purge " + spec.Name + " --force` and dispatch it again"
			if actor == operatorActor {
				fix = "Restore the directory, or run `lever worker purge " + spec.Name + " --force` and have the manager start it again"
			}
			refuse(w, ReasonWorkspaceMissing, "worker "+spec.Name+"'s workspace directory "+spec.WorkspaceSubdir+" no longer exists on the host (the operator removed or moved it); the record was kept. "+fix, http.StatusConflict)
			return
		}
		refuse(w, ReasonRuntimeError, "workspace error", http.StatusInternalServerError)
		return
	}
	// A resume recreates the container from the record's volumes, so a
	// record holding access the config has withdrawn would get it back.
	if err := b.checkSharedAccess(ctx, spec, true); err != nil {
		b.audit("worker", actor, "deny", "resume "+spec.Name+": "+err.Error())
		refuse(w, ReasonSharedAccess, "forbidden: "+sharedAccessHint(spec.Name, err)+"; the record was kept", http.StatusForbidden)
		return
	}
	// Stage a fresh ticket BEFORE resuming: a worker past its leaf lifetime
	// re-enrols on boot and the old ticket is spent, and the ticket dir is
	// tmpfs, so after a jail restart podman refuses the missing mount source.
	// Harmless when the leaf is still valid: the ticket ages out unspent.
	if err := b.stageWorkerTicket(ctx, spec); err != nil {
		b.audit("worker", actor, "error", "resume "+err.Error())
		refuse(w, ReasonRuntimeError, "stage error", http.StatusInternalServerError)
		return
	}
	resume := b.runtime.Resume
	if phase == scion.PhaseError || phase == phaseRunningContainerDown {
		// Only resume --force recovers an error-phase record (a plain resume
		// answers 409). A running record with a dead container is the same
		// case early; if the hub refuses, the manager is told to retry.
		resume = b.runtime.ResumeForce
	}
	if err := resume(ctx, spec.Name, b.instanceProject); err != nil {
		b.audit("worker", actor, "error", "resume "+spec.Name+" (phase "+phase+"): "+err.Error())
		// Generic wire bodies: the scion error text can echo the container
		// env. The detail stays in the audit log.
		if scion.IsRefusedByHub(err) {
			// scion folds an authorization denial and a transitional phase
			// into one 409, so this says nothing about the worker's state.
			next := "The record may be in a transitional phase: check `lever-manager agent list` and try again. " +
				"If the refusal persists, the operator should run `lever doctor`."
			if actor == operatorActor {
				next = "The record may be in a transitional phase: try again shortly. If the refusal persists, run `lever doctor`."
			}
			refusePhase(w, ReasonHubRefused, phase, "the hub refused to resume worker "+spec.Name+" (phase "+phase+"); its record was kept, nothing was deleted. "+next, http.StatusConflict)
			return
		}
		refusePhase(w, ReasonRuntimeError, phase, "runtime error: worker "+spec.Name+" (phase "+phase+") did not resume. "+workerPurgeHint(spec.Name), http.StatusBadGateway)
		return
	}
	if err := b.waitWorkerLive(ctx, spec); err != nil {
		b.audit("worker", actor, "error", "resume "+spec.Name+": "+err.Error())
		refuse(w, ReasonNotLive, err.Error()+". "+workerPurgeHint(spec.Name), http.StatusBadGateway)
		return
	}
	b.resetReenrolTries(spec.Name)
	b.audit("worker", actor, "allow", "resume "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
}

// comingUp reports scion's interim phases on the way to running; "resumed"
// is reported by the CLI but absent from the hub's enum.
func comingUp(phase string) bool {
	switch phase {
	case "created", "provisioning", "cloning", "starting", "resumed":
		return true
	}
	return false
}

// workerPurgeHint only names the recovery: deleting a record is never
// automatic.
func workerPurgeHint(worker string) string {
	return "If it does not recover, the operator can run `lever worker purge " + worker + "` on the host, then start the worker fresh."
}

func (b *Broker) startFreshWorker(w http.ResponseWriter, r *http.Request, spec WorkerSpec, task string) {
	ctx := r.Context()
	// Before any side effect.
	release, ok := b.admitOrRefuse(ctx, w, b.manager, "start", spec.Name)
	if !ok {
		return
	}
	defer release()
	// Before any side effect: an unreadable file must fail with nothing
	// staged.
	instructions, err := readWorkerInstructions(spec)
	if err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
		http.Error(w, "instructions error", http.StatusInternalServerError)
		return
	}
	// Before the ticket: a refused workspace must not spend one.
	if err := b.ensureWorkspaceDir(spec); err != nil {
		if errors.Is(err, ErrSharedFolder) {
			b.audit("worker", b.manager, "deny", "start "+spec.Name+": "+err.Error())
			http.Error(w, "forbidden: a shared folder of worker "+spec.Name+" is missing on the host or reached through a symbolic link", http.StatusForbidden)
			return
		}
		if errors.Is(err, fsutil.ErrEscapesTree) {
			b.audit("worker", b.manager, "deny", "start "+spec.Name+": workspace dir: "+err.Error())
			http.Error(w, "forbidden: worker workspace escapes the instance tree", http.StatusForbidden)
			return
		}
		if errors.Is(err, ErrReadOnlyOverlap) {
			b.audit("worker", b.manager, "deny", "start "+spec.Name+": workspace dir: "+err.Error())
			http.Error(w, "forbidden: worker workspace overlaps a protected directory (manager.read_only or a shared folder)", http.StatusForbidden)
			return
		}
		b.audit("worker", b.manager, "error", "start "+spec.Name+": workspace dir: "+err.Error())
		http.Error(w, "workspace error", http.StatusInternalServerError)
		return
	}
	if err := b.checkSharedAccess(ctx, spec, false); err != nil {
		b.audit("worker", b.manager, "deny", "start "+spec.Name+": "+err.Error())
		http.Error(w, "forbidden: "+sharedAccessHint(spec.Name, err), http.StatusForbidden)
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
	var commit func() error
	if b.beginSession != nil {
		commit = b.beginSession(spec.Name)
	}
	if err := b.runtime.Start(ctx, scion.StartOpts{
		Worker: spec.Name, Task: task, Harness: "claude",
		Project: b.instanceProject, WorkspaceSubdir: spec.WorkspaceSubdir,
		Image: spec.Image, Model: spec.Model, APIKey: spec.APIKey,
		Instructions: instructions, Volumes: volumes, Env: env,
		Resources: spec.Resources,
	}); err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	// A failed session record only keeps contacts out.
	if commit != nil {
		if err := commit(); err != nil {
			b.audit("worker", b.manager, "error", "start "+spec.Name+": session record: "+err.Error())
		}
	}
	if err := b.waitWorkerLive(ctx, spec); err != nil {
		b.audit("worker", b.manager, "error", "start "+spec.Name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	b.resetReenrolTries(spec.Name)
	b.audit("worker", b.manager, "allow", "start "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
}

// readWorkerInstructions reads the file host-side; scion gets the content on
// stdin as inline agent config.
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

const (
	defaultLiveAttempts = 20
	defaultLiveInterval = 500 * time.Millisecond
)

// waitWorkerLive waits for phase running with a live container. scion's own
// start/resume success can lie (it reports "resumed" for a harness that dies
// moments later), so the observed record decides, and a crash-looping worker
// surfaces as an error.
func (b *Broker) waitWorkerLive(ctx context.Context, spec WorkerSpec) error {
	err := scion.WaitAgentLive(ctx, func(c context.Context) ([]scion.Agent, error) {
		return b.runtime.List(c, b.instanceProject)
	}, spec.Name, scion.LiveBudget{Attempts: b.liveAttempts, Interval: b.liveInterval, Settle: b.liveSettle})
	if err == nil {
		return nil
	}
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
	unlock, ok := b.lockWorkerOrRefuse(w, r.Context(), b.manager, r.URL.Path, spec.Name)
	if !ok {
		return
	}
	defer unlock()
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
	// Not workerVerb: this verb has refusals that are not runtime errors, and
	// it answers only once the worker is live.
	var req wire.WorkerRequest
	spec, ok := b.requireManagerWorker(w, r, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	b.resumeWorker(r.Context(), w, spec, b.manager)
}

// resumeWorker is the resume verb after its caller is authorized: the
// manager's /worker/resume and the operator's `lever worker resume` share it,
// so both take the worker's lock and the same checks. actor names the caller
// in the audit lines and picks the no-record hint.
func (b *Broker) resumeWorker(ctx context.Context, w http.ResponseWriter, spec WorkerSpec, actor string) {
	unlock, ok := b.lockWorkerOrRefuse(w, ctx, actor, "resume", spec.Name)
	if !ok {
		return
	}
	defer unlock()
	if b.isRevoked(spec.Name) {
		// Under the lock, for every caller: a revoke that landed while this
		// resume waited for another must not get the worker a fresh ticket.
		b.audit("worker", actor, "deny", "resume "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	if err := b.checkAgentRole(ctx, spec.Name); err != nil {
		b.audit("worker", actor, "deny", "resume "+spec.Name+": "+err.Error())
		refuse(w, ReasonRoleMismatch, err.Error(), http.StatusConflict)
		return
	}
	// scion's resume of a missing record CREATES the agent from the project
	// template and scion's default harness, not the worker's configured
	// harness, image, model or role. Refuse it and name start instead.
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", actor, "error", "resume "+spec.Name+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == "" {
		b.audit("worker", actor, "deny", "resume "+spec.Name+": no record")
		hint := "start it instead: `lever-manager agent start " + spec.Name + " --task \"…\"`"
		if actor == operatorActor {
			hint = "ask the manager to start it (`lever-manager agent start " + spec.Name + " --task \"…\"` in the manager's session)"
		}
		refuse(w, ReasonNoRecord, "worker "+spec.Name+" has no record on the hub (never started, purged, or a new jail); "+hint, http.StatusNotFound)
		return
	}
	if phase == scion.PhaseRunning {
		b.audit("worker", actor, "allow", "resume "+spec.Name+": already running")
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
		return
	}
	b.resumeRecord(ctx, w, spec, phase, actor)
}

func (b *Broker) checkAgentRole(ctx context.Context, agent string) error {
	if b.verifyRole == nil {
		return nil
	}
	return b.verifyRole(ctx, agent)
}

func (b *Broker) handleWorkerList(w http.ResponseWriter, r *http.Request) {
	// A revoked manager cannot enumerate the fleet either.
	if _, ok := b.requireManager(w, r, "worker", "list: "); !ok {
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	agents, err := b.runtime.List(r.Context(), b.instanceProject)
	if err != nil {
		b.audit("worker", b.manager, "error", "list: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	// Phase, activity and container status are agent-settable text and the
	// manager reads this list: only known labels go out, so a worker cannot
	// write to the manager through its own status.
	for i := range agents {
		agents[i].Phase = scion.PhaseLabel(agents[i].Phase)
		agents[i].Activity = scion.ActivityLabel(agents[i].Activity)
		agents[i].ContainerStatus = scion.ContainerLabel(agents[i].ContainerStatus)
	}
	writeJSON(w, wire.WorkerListResponse[scion.Agent]{Agents: agents})
}
