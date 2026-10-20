package apply

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/imageref"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wait"
)

// ManagerIntent is what a caller wants of the manager. EnsureManager reads
// the manager once, decides from the intent and that reading alone
// (planManager), and runs the actions.
type ManagerIntent int

const (
	// ManagerKeep brings the manager up on its conversation: an absent record
	// is created, a running one kept, a suspended or stopped one resumed, an
	// error one resumed with --force. A running record whose container podman
	// shows gone or exited is suspended and resumed.
	ManagerKeep ManagerIntent = iota + 1
	// ManagerFresh discards the record, after the conversation backup, and
	// creates the manager anew.
	ManagerFresh
	// ManagerRestore suspends a running manager, runs the caller's swap of
	// its conversation (ManagerRequest.WhileSuspended) and resumes it on
	// what its home now holds.
	ManagerRestore
	// ManagerNewSession ends the manager's session and starts it again in a
	// new one on the same record: suspend, the caller's backup
	// (WhileSuspended), stop, and a resume with the boot task. The hub
	// resumes a suspended record with claude --continue and a stopped one in
	// a fresh session; a real stop, not a second claude process, ends the
	// old session, so the shared SessionEnd hook then reports a phase that is
	// true.
	ManagerNewSession
	// ManagerPark suspends the manager for a power-off. A record the hub
	// reads stopped while its claude still runs is reported running first
	// (HealAgentSession), or the suspend would not keep its conversation.
	// The suspend's own error is returned as scion gave it.
	ManagerPark
	// ManagerReenrol bounces the manager so its boot re-enrols with a fresh
	// ticket (ManagerRequest.StageTicket): a running record is suspended and
	// resumed, a suspended or stopped one resumed, an error one resumed with
	// --force. The broker's certificate heal; it never creates or deletes.
	ManagerReenrol
	// ManagerTokenReset resets a running manager's hub token (scion
	// reset-auth): a token of the record's own stored role, behind the
	// pre-role record guard. The broker's token watch.
	ManagerTokenReset
)

// ErrManagerRefused marks a change a check refused before or after it acted:
// the record's stored role, or the manager's tree mounts. A refusal after
// the resume suspended the manager (or says it could not).
var ErrManagerRefused = errors.New("the manager's checks refused the change")

func refused(err error) error { return markedError{err, ErrManagerRefused} }

// The refusals of ManagerNewSession a caller tells apart. Nothing was
// changed.
var (
	// ErrManagerBusy: the manager reports a turn in progress.
	ErrManagerBusy = errors.New("the manager is in the middle of a turn")
	// ErrManagerNotRunning: RunningOnly found the manager not running.
	ErrManagerNotRunning = errors.New("the manager is not running")
)

// ErrSessionEnded marks a ManagerNewSession failure after the stop: the old
// session can no longer be continued, and nothing resumes the manager.
var ErrSessionEnded = errors.New("the manager's old session has ended")

// ErrStopUnconfirmed marks a ManagerNewSession whose stop did not read in
// time: lever cannot tell whether the old session ended, and started
// nothing.
var ErrStopUnconfirmed = errors.New("the manager's stop is unconfirmed")

// markedError is an error that Is its mark, with its own message.
type markedError struct {
	err, mark error
}

func (e markedError) Error() string        { return e.err.Error() }
func (e markedError) Unwrap() error        { return e.err }
func (e markedError) Is(target error) bool { return target == e.mark }

// ManagerBusy reads the hub's activity for a turn in progress. The activity
// is the agent's own report: an agent can hold off a new session this way,
// never bring one about.
func ManagerBusy(activity string) bool {
	switch activity {
	case scion.ActivityWorking, "thinking", "executing":
		return true
	}
	return false
}

func (i ManagerIntent) String() string {
	switch i {
	case ManagerKeep:
		return "keep"
	case ManagerFresh:
		return "fresh"
	case ManagerRestore:
		return "restore"
	case ManagerNewSession:
		return "new-session"
	case ManagerPark:
		return "park"
	case ManagerReenrol:
		return "reenrol"
	case ManagerTokenReset:
		return "token-reset"
	}
	return fmt.Sprintf("intent(%d)", int(i))
}

// ManagerRequest is one call of EnsureManager.
type ManagerRequest struct {
	Intent ManagerIntent
	// Subject prefixes the messages ("start-manager", "up").
	Subject string
	// What names the caller to a command the lifecycle lock turns away.
	What string

	// Attach: the gate for a manager this call creates ends once claude's
	// pane is alive (PlanOpts.Attach).
	Attach bool
	// FreshWithoutConversation: PlanOpts.FreshWithoutConversation.
	FreshWithoutConversation bool
	// TicketStaged: a fresh enrolment ticket was staged for this run, so no
	// re-arm is needed before a create or resume.
	TicketStaged bool
	// TicketDeferred: the mint step left the ticket to the create, which mints
	// it once the discarded manager's container is gone (--fresh).
	TicketDeferred bool
	// BrokerOnline: the runtime broker was seen online since the hub last
	// started, so no wait for it is needed.
	BrokerOnline bool

	// Task (NewSession) is the boot prompt the new session runs; "" leaves
	// the hub's stored task.
	Task string
	// Force (NewSession) interrupts a turn the manager is in the middle of.
	Force bool
	// RunningOnly (NewSession) leaves a manager that is not running alone.
	RunningOnly bool
	// BeginSession (NewSession) runs once the old session has ended and
	// returns the commit that records the new one once it started.
	BeginSession func() (commit func() error, err error)

	// UnderLock runs once the lifecycle lock is held, before anything is
	// read; its error is returned as it is and nothing is changed.
	UnderLock func() error
	// StageTicket (Reenrol) stages a fresh enrolment ticket, after the
	// checks before the resume.
	StageTicket func(ctx context.Context) error

	// HealBudget (Park) bounds the session heal before the suspend, so a slow
	// one cannot cost the manager its suspend. Zero means parkHealBudget.
	HealBudget time.Duration

	// WhileSuspended (Restore, NewSession) runs once the hub reads the manager suspended
	// with no live container, so no agent process runs while its files
	// change. An error resumes a manager this call suspended, on its own
	// conversation; after it returns nil nothing resumes it but the plan.
	WhileSuspended func(ctx context.Context) error
	// ResumeHow names the conversation a resume after a failure continues
	// ("on its own conversation"); nil leaves it out.
	ResumeHow func() string
}

func (q ManagerRequest) subject() string {
	if q.Subject != "" {
		return q.Subject
	}
	return "start-manager"
}

// ManagerResult says what EnsureManager did.
type ManagerResult struct {
	// Verb names the change made to the record ("" when none).
	Verb string
	// Created: this call created the record.
	Created bool
	// MountsUnverified: the manager was resumed and gated, but its tree
	// mounts could not be read back (a warning was logged).
	MountsUnverified bool
}

// ManagerLock takes the lifecycle lock for what, without waiting, and
// returns its release. An error means another lever command holds it.
type ManagerLock func(what string) (release func(), err error)

// HeldManagerLock is the ManagerLock of a command that took the lifecycle
// lock before it built its Deps and holds it until it returns.
func HeldManagerLock(string) (func(), error) { return func() {}, nil }

// ManagerScion is scion's verbs on the manager's record. *scion.Client is
// the production one.
type ManagerScion interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Start(ctx context.Context, o scion.StartOpts) error
	Resume(ctx context.Context, worker, project string) error
	ResumeForce(ctx context.Context, worker, project string) error
	Suspend(ctx context.Context, worker, project string) error
	Delete(ctx context.Context, worker, project string) error
	ResetAuth(ctx context.Context, agent, project string) error
	ResumeTask(ctx context.Context, worker, project, task string) error
	Stop(ctx context.Context, worker, project string) error
}

func (d Deps) managerScion() ManagerScion {
	if d.ManagerVerbs != nil {
		return d.ManagerVerbs
	}
	if d.Scion == nil {
		return nil
	}
	return d.Scion
}

// EnsureManager is the one lifecycle of the manager: every caller that
// creates, resumes, suspends or recreates it goes through here. It takes the
// lifecycle lock (Deps.ManagerLock) and refuses without one.
func EnsureManager(ctx context.Context, app *config.App, d Deps, q ManagerRequest) (ManagerResult, error) {
	subject := q.subject()
	if d.ManagerLock == nil {
		return ManagerResult{}, fmt.Errorf("%s: internal error: no lifecycle lock is wired, so lever does not change the manager", subject)
	}
	if d.managerScion() == nil || d.Log == nil {
		return ManagerResult{}, fmt.Errorf("%s: internal error: the manager's lifecycle is not wired", subject)
	}
	if (q.Intent == ManagerKeep || q.Intent == ManagerFresh) && (d.WaitBrokerReady == nil || d.RearmBootstrap == nil || d.VerifyAgentRole == nil) {
		return ManagerResult{}, fmt.Errorf("%s: internal error: the manager's lifecycle is not wired", subject)
	}
	unlock, err := d.ManagerLock(cmp.Or(q.What, subject))
	if err != nil {
		return ManagerResult{}, err
	}
	release := sync.OnceFunc(unlock)
	defer release()
	if q.UnderLock != nil {
		if err := q.UnderLock(); err != nil {
			return ManagerResult{}, err
		}
	}
	r := &run{app: app, d: d,
		brokerStart:  d.BrokerStartRetry.or(defaultBrokerStartRetry),
		managerLive:  d.ManagerLiveRetry.or(defaultManagerLiveRetry),
		phaseSettle:  d.PhaseSettleRetry.or(defaultPhaseSettleRetry),
		fresh:        q.Intent == ManagerFresh,
		attach:       q.Attach,
		freshNoConv:  q.FreshWithoutConversation,
		minted:       q.TicketStaged,
		deferMint:    q.TicketDeferred,
		brokerOnline: q.BrokerOnline,
		subject:      subject,
	}
	jp := JailPath(app.Tree, app.Tree, d.JailMount)
	switch q.Intent {
	case ManagerKeep, ManagerFresh:
		return r.ensureUp(ctx, jp, q)
	case ManagerRestore:
		if q.WhileSuspended == nil || d.VerifyAgentRole == nil {
			return ManagerResult{}, fmt.Errorf("%s: internal error: the manager's lifecycle is not wired", subject)
		}
		return r.swapConversation(ctx, jp, q)
	case ManagerNewSession:
		if q.WhileSuspended == nil || q.BeginSession == nil || d.VerifyAgentRole == nil {
			return ManagerResult{}, fmt.Errorf("%s: internal error: the manager's lifecycle is not wired", subject)
		}
		return r.newSession(ctx, jp, q)
	case ManagerPark:
		return r.park(ctx, jp, q)
	case ManagerReenrol:
		if q.StageTicket == nil || d.VerifyAgentRole == nil {
			return ManagerResult{}, fmt.Errorf("%s: internal error: the manager's lifecycle is not wired", subject)
		}
		return r.reenrol(ctx, jp, q, release)
	case ManagerTokenReset:
		if d.VerifyAgentRole == nil {
			return ManagerResult{}, refused(errors.New("no pre-role record guard is wired"))
		}
		return r.resetToken(ctx, jp, q)
	}
	return ManagerResult{}, fmt.Errorf("%s: internal error: unknown manager intent %s", subject, q.Intent)
}

// managerObservation is the one reading of the manager a plan is made from.
type managerObservation struct {
	present                    bool
	phase, container, activity string
	// restarted: the record reads running, the guest booted anew since the
	// manager last ran live, and podman shows its container not running.
	restarted bool
	// containerDown: the record reads running and podman shows its container
	// gone, exited or stopped, in this boot.
	containerDown bool
	// recovered: this call already suspended the record for one of the two.
	recovered bool
}

func observationOf(rec *scion.Agent) managerObservation {
	if rec == nil {
		return managerObservation{}
	}
	return managerObservation{present: true, phase: rec.Phase, container: rec.ContainerStatus, activity: rec.Activity}
}

// managerVerb is the one change a plan makes to the record.
type managerVerb int

const (
	verbNone managerVerb = iota
	verbCreate
	verbResume
	verbResumeForce
	verbDeleteCreate
	// verbResumeTask resumes a stopped record in a new session with a task.
	verbResumeTask
	verbSuspend
	verbResetAuth
)

func (v managerVerb) String() string {
	switch v {
	case verbCreate:
		return "create"
	case verbResume:
		return "resume"
	case verbResumeForce:
		return "resume --force"
	case verbDeleteCreate:
		return "delete+create"
	case verbResumeTask:
		return "stop+resume"
	case verbSuspend:
		return "suspend"
	case verbResetAuth:
		return "reset-auth"
	}
	return ""
}

// managerPlan is planManager's answer.
type managerPlan struct {
	// recover: suspend the record first, read it again and plan again.
	recover bool
	// suspend: suspend the record and wait until it reads suspended.
	suspend bool
	// stop: stop the suspended record and wait until it reads stopped.
	stop bool
	// heal: report a stopped record's live session running first.
	heal bool
	verb managerVerb
	// mustCreate: a create that finds a record fails rather than keeps it.
	mustCreate bool
}

// planManager decides what to do with the manager from the intent and one
// observation, and nothing else. Every refusal leaves the record as it is.
func planManager(q ManagerRequest, name string, o managerObservation) (managerPlan, error) {
	switch q.Intent {
	case ManagerFresh:
		if !o.present {
			// An "already exists" answer means the observation missed a live
			// record: that is the session the operator asked to discard.
			return managerPlan{verb: verbCreate, mustCreate: true}, nil
		}
		return managerPlan{verb: verbDeleteCreate}, nil
	case ManagerKeep:
		switch {
		case !o.present:
			return managerPlan{verb: verbCreate}, nil
		case o.phase == scion.PhaseRunning:
			if (o.restarted || o.containerDown) && !o.recovered {
				return managerPlan{recover: true}, nil
			}
			return managerPlan{verb: verbNone}, nil
		case o.phase == scion.PhaseSuspended || o.phase == scion.PhaseStopped:
			return managerPlan{verb: verbResume}, nil
		case o.phase == scion.PhaseError:
			return managerPlan{verb: verbResumeForce}, nil
		}
		// A transitional phase was waited out before (settleManagerPhase), so
		// this is a phase lever does not know: a newer scion enum or a hostile
		// hub string. The operator's discard is up --fresh, which needs no
		// phase.
		return managerPlan{}, fmt.Errorf("%s: manager %q is in phase %s, which lever does not recognise; nothing was changed. Retry once it settles, or run `lever up --fresh` to discard the session and start over",
			q.subject(), name, scion.BoundedQuote(o.phase))
	case ManagerRestore:
		// The phase alone decides: a running record is suspended whatever its
		// container shows.
		switch {
		case !o.present:
			return managerPlan{}, errors.New("there is no manager record to restore into; run `lever up --no-attach` first")
		case o.phase == scion.PhaseRunning:
			return managerPlan{suspend: true, verb: verbResume}, nil
		case o.phase == scion.PhaseSuspended:
			return managerPlan{verb: verbResume}, nil
		}
		return managerPlan{}, fmt.Errorf("manager %q is in phase %s; a restore needs it running or suspended (`lever up --no-attach` first)", name, scion.BoundedQuote(o.phase))
	case ManagerNewSession:
		// The phase and the agent's activity decide; the container column
		// does not.
		switch {
		case !o.present:
			return managerPlan{}, errors.New("there is no manager record; run `lever up` first")
		case o.phase == scion.PhaseRunning:
			if !q.Force && ManagerBusy(o.activity) {
				return managerPlan{}, ErrManagerBusy
			}
			return managerPlan{suspend: true, stop: true, verb: verbResumeTask}, nil
		case q.RunningOnly:
			return managerPlan{}, ErrManagerNotRunning
		case o.phase == scion.PhaseSuspended:
			return managerPlan{stop: true, verb: verbResumeTask}, nil
		}
		return managerPlan{}, fmt.Errorf("manager %q is in phase %s; a new conversation needs it running or suspended (`lever up` first)", name, scion.BoundedQuote(o.phase))
	case ManagerPark:
		// The suspend is sent whatever the reading: a power-off follows, and
		// scion's answer is what the caller reports.
		return managerPlan{heal: o.present && o.phase == scion.PhaseStopped, verb: verbSuspend}, nil
	case ManagerReenrol:
		switch o.phase {
		case scion.PhaseRunning:
			return managerPlan{suspend: true, verb: verbResume}, nil
		case scion.PhaseSuspended, scion.PhaseStopped:
			return managerPlan{verb: verbResume}, nil
		case scion.PhaseError:
			return managerPlan{verb: verbResumeForce}, nil
		}
		return managerPlan{}, fmt.Errorf("manager %q is in phase %s, which the broker does not bounce; run `lever up`", name, scion.BoundedQuote(o.phase))
	case ManagerTokenReset:
		// Read again under the lock: a manager lever suspended or stopped
		// meanwhile has no token to reset.
		if o.phase == scion.PhaseRunning && scion.ContainerLive(o.container) {
			return managerPlan{verb: verbResetAuth}, nil
		}
		return managerPlan{verb: verbNone}, nil
	}
	return managerPlan{}, fmt.Errorf("%s: internal error: unknown manager intent %s", q.subject(), q.Intent)
}

// observeUp is the reading ManagerKeep and ManagerFresh plan from: the
// record settled and role-checked (observeManager); for Keep its session
// healed and, for a running record, podman's word on its container.
func (r *run) observeUp(ctx context.Context, jp string) (*scion.Agent, managerObservation, error) {
	rec, err := r.observeManager(ctx, jp)
	if r.fresh && errors.Is(err, scion.ErrAgentListTooLarge) {
		// The delete and the suspend before the backup name the manager and
		// need no list, so --fresh still discards a record the list hides.
		r.d.Log("%s: --fresh: the hub's agent list is too large to read; treating manager %q as present and deleting it by name", r.subjectName(), r.app.Name)
		r.unlisted = true
		return nil, managerObservation{present: true}, nil
	}
	if err != nil {
		return nil, managerObservation{}, err
	}
	if r.fresh {
		return rec, observationOf(rec), nil
	}
	// A stopped record whose claude still runs must read running again, or
	// the resume restarts it fresh.
	rec = HealAgentSession(ctx, r.d.sessionHealer(), jp, rec)
	o := observationOf(rec)
	o.restarted = guestRestartedUnder(ctx, r.d, jp, r.app.Name, rec)
	if !o.restarted {
		o.containerDown = containerGoneUnder(ctx, r.d, jp, r.app.Name, rec)
	}
	return rec, o, nil
}

// containerGoneUnder reports a running record whose container podman shows
// gone, exited or stopped. Only podman's answer counts, never the hub's
// container column. A container podman lists as created is a start under way,
// unless podman made it before the guest's current boot: a guest killed
// between podman's create and its start leaves one that nothing will start,
// and the hub still reads the record running, so every up waited on it and
// failed. Any read that fails answers no.
func containerGoneUnder(ctx context.Context, d Deps, project, name string, rec *scion.Agent) bool {
	if rec == nil || rec.Phase != scion.PhaseRunning || d.StaleContainer == nil {
		return false
	}
	ref := jail.ContainerName(path.Base(project), name)
	st, err := d.StaleContainer.State(ctx, ref)
	switch {
	case errors.Is(err, jail.ErrNoContainer):
		return true
	case err != nil, st.Running, st.Paused:
		return false
	case st.Status == "created":
		if d.ContainerCreatedBeforeBoot == nil {
			return false
		}
		before, err := d.ContainerCreatedBeforeBoot(ctx, ref)
		return err == nil && before
	}
	return st.Status == "exited" || st.Status == "stopped"
}

// suspendToRecover suspends a running record over a dead container so the
// plan made from the next reading resumes it with its conversation. A failed
// suspend is only logged: the next reading decides.
func (r *run) suspendToRecover(ctx context.Context, jp string, rec *scion.Agent, o managerObservation) {
	if o.restarted {
		suspendAfterGuestRestart(ctx, r.d, r.subjectName(), jp, r.app.Name, rec)
		return
	}
	r.d.logf("%s: manager %q: the hub reads it running, but podman shows its container gone, exited, or created in an earlier guest boot and never started; suspending and resuming it (the conversation is kept)", r.subjectName(), r.app.Name)
	if err := r.d.managerScion().Suspend(ctx, r.app.Name, jp); err != nil {
		r.d.logf("%s: warning: suspend of manager %q failed: %s", r.subjectName(), r.app.Name, scion.ErrSummary(err))
	}
}

// ensureUp brings the manager up for ManagerKeep and ManagerFresh. Only
// ManagerFresh, and the fresh start of a manager proven to hold no
// conversation (startFreshWithoutConversation), delete a record.
func (r *run) ensureUp(ctx context.Context, jp string, q ManagerRequest) (ManagerResult, error) {
	// Read the prompt before any waiting: a missing or unreadable prompt file is a
	// config error and should fail fast.
	task, err := ManagerTask(r.app)
	if err != nil {
		return ManagerResult{}, err
	}
	instructions, err := r.managerInstructions()
	if err != nil {
		return ManagerResult{}, err
	}
	// The read_only paths must be real directories reached through no symlink
	// before anything is acted on: under --fresh the delete runs before the create,
	// and refusing only at the create would discard the old session and start
	// none. Checked on resume too: a failure means the tree changed under a manager
	// that may hold it writable. startManagerCreate checks again right before the
	// create, once the old manager (which may hold the paths writable) is gone.
	if err := r.app.PrepareManagerReadOnlyHost(); err != nil {
		return ManagerResult{}, fmt.Errorf("%s: %w", r.subjectName(), err)
	}
	// The daemon registers its runtime broker asynchronously after its Hub API
	// is up. Fail-soft; the broker-unavailable retry backstops it.
	if !r.brokerOnline {
		if _, err := r.d.WaitBrokerReady(ctx, jp); err != nil {
			return ManagerResult{}, fmt.Errorf("%s: waiting for runtime broker: %w", r.subjectName(), err)
		}
	}
	opts, err := r.managerStartOpts(ctx, jp, task, instructions)
	if err != nil {
		return ManagerResult{}, err
	}
	if opts.Image == "" {
		// A create would make a record with no image, which refuseUnqualifiedRecord
		// then refuses for good; before observeUp, so a --fresh deletes nothing.
		return ManagerResult{}, fmt.Errorf("%s: manager.image is not set in %s, so the manager would run a default image name the hub rewrites to its image registry; nothing was changed. Set it (e.g. %s/lever-claude)",
			r.subjectName(), config.CanonicalName, imageref.Registry)
	}
	rec, o, err := r.observeUp(ctx, jp)
	if err != nil {
		return ManagerResult{}, err
	}
	if !r.fresh {
		if err := r.refuseUnqualifiedRecord(rec); err != nil {
			return ManagerResult{}, err
		}
	}
	p, err := planManager(q, r.app.Name, o)
	if err == nil && p.recover {
		r.suspendToRecover(ctx, jp, rec, o)
		if rec, err = r.observeManager(ctx, jp); err != nil {
			return ManagerResult{}, err
		}
		o = observationOf(rec)
		o.recovered = true
		p, err = planManager(q, r.app.Name, o)
	}
	if err != nil {
		return ManagerResult{}, err
	}
	if rec != nil && !r.fresh {
		if err := r.checkStaleTreeMounts(ctx, jp, p.verb == verbNone); err != nil {
			return ManagerResult{}, err
		}
	}
	// A container start, the pre-start hook, claude's boot and the settle window
	// can run over a minute: report progress about every 15 s until the gate ends.
	r.progress = StartProgress(ctx, r.d, r.subjectName(), r.app.Name, "asking scion to start or resume the container")
	// The post-live probes run alongside the settle window, from the moment the
	// gate has seen claude.
	probes := r.startPostLive(ctx, jp)
	err = r.actUp(ctx, jp, rec, p, opts)
	acted := p.verb != verbNone
	// A resume recreated the container from the record's mounts: check them as
	// soon as it runs, and again once it is live if it was not running yet.
	resumed := err == nil && (p.verb == verbResume || p.verb == verbResumeForce) && !r.managerCreated
	verified := false
	if resumed {
		verified, err = r.verifyResumedTreeMounts(ctx, jp)
	}
	if err == nil {
		err = r.waitManagerLive(ctx, jp, acted, probes.start)
	}
	// A failed resume or gate can still leave a recreated container running: read
	// its mounts back before the failure is reported, so a swapped mount is
	// suspended rather than left running unchecked. A container that is not
	// running reads as unverified, which says nothing here.
	var mountErr error
	if err != nil && !verified && (p.verb == verbResume || p.verb == verbResumeForce) && !r.managerCreated {
		probes.wait()
		if _, mountErr = r.verifyResumedTreeMounts(ctx, jp); mountErr != nil {
			err = errors.Join(err, mountErr)
		}
	}
	// A resume whose claude found no conversation to continue: under lever up, a
	// home proven to hold none is started fresh. Never after a failed mount check
	// (that manager is suspended and its operator decides), never for lever apply.
	// A container that is not running reads as unverified there, not failed: its
	// mounts go with the delete, and the new container's come from the config.
	if err != nil && resumed && mountErr == nil && r.freshNoConv && !r.fresh && errors.Is(err, ErrNoConversation) {
		probes.abandon()
		probes = r.startPostLive(ctx, jp)
		err = r.startFreshWithoutConversation(ctx, jp, opts, err, probes.start)
		resumed = false
	}
	if err == nil && resumed && !verified {
		// The post-live probes read the same mounts: let them end first so the two
		// never race. The manager is not reported up until this check passes; a
		// mismatch suspends it and the probes' warnings are dropped.
		probes.wait()
		if verified, err = r.verifyResumedTreeMounts(ctx, jp); err == nil && !verified {
			r.warnUnverifiedResume()
		}
	}
	r.progress.Stop()
	r.progress = nil
	res := ManagerResult{Verb: p.verb.String(), Created: r.managerCreated}
	if err != nil {
		probes.abandon()
		return res, err
	}
	recordGuestBoot(ctx, r.d)
	probes.finish()
	return res, nil
}

// refuseUnqualifiedRecord refuses to keep or resume a manager record whose
// image the hub can rewrite. scion keeps a record's image for life and
// rewrites an unqualified one to the hub's image registry at every start, a
// setting any agent can change between applies (to a registry it controls,
// which scion pulls from). A record with no image, created without one, runs
// the template's or harness's default, which is rewritten the same way. Only a
// fresh create moves the record to a qualified name.
func (r *run) refuseUnqualifiedRecord(rec *scion.Agent) error {
	if rec == nil || imageref.Fixed(rec.Image) {
		return nil
	}
	what := "was created on the unqualified image name " + termsafe.Sanitize(rec.Image)
	if rec.Image == "" {
		what = "has no image on its record (it was created without one), so it runs a default image name"
	}
	fix := "Run `lever up --fresh --keep-conversation` to recreate it on " + termsafe.Sanitize(r.app.ManagerAgentImage()) + " and keep its conversation"
	if r.app.ManagerAgentImage() == "" {
		fix = "Set manager.image in " + config.CanonicalName + ", then run `lever up --fresh --keep-conversation` to recreate it on that image and keep its conversation"
	}
	return fmt.Errorf("%s: manager %q %s, which the hub rewrites to its image registry at every start, a setting any agent can change; nothing was changed. %s",
		r.subjectName(), r.app.Name, what, fix)
}

// actUp makes the plan's change to the record; the gate after it confirms
// the container is up, so a kept running record over a dead container fails
// loudly there.
func (r *run) actUp(ctx context.Context, jp string, rec *scion.Agent, p managerPlan, opts scion.StartOpts) error {
	switch p.verb {
	case verbCreate:
		return r.startManagerCreate(ctx, opts, p.mustCreate)
	case verbDeleteCreate:
		// --fresh discards the session whatever the record's phase. Decided here,
		// after the hub is up, because after lever stop the record is invisible
		// until this step. The create that follows is what puts a changed
		// manager.image into effect (a resume keeps the record's image).
		if err := r.backupBeforeDelete(ctx, jp, rec); err != nil && r.unlisted {
			// Unseen, the record may already be gone (an earlier --fresh deleted
			// it), and with it the home the backup reads.
			return fmt.Errorf("%s: --fresh: backing up manager %q's conversation failed, so lever deleted nothing: %w. If an earlier `lever up --fresh` already deleted the manager's record, there is no conversation left to back up: run `lever up --fresh --no-backup`. Otherwise fix the cause and run `lever up --fresh` again", r.subjectName(), r.app.Name, err)
		} else if err != nil {
			return fmt.Errorf("%s: --fresh: backing up manager %q's conversation failed, so the manager was NOT deleted (its record and conversation are kept): %w. Fix the cause and run `lever up --fresh` again, or run `lever up --fresh --no-backup` to discard the conversation without a backup", r.subjectName(), r.app.Name, err)
		}
		phase := "unread"
		if rec != nil {
			phase = scion.BoundedQuote(rec.Phase)
		}
		return r.recoverDeleteAndCreate(ctx, jp, opts,
			fmt.Sprintf("%s: --fresh — deleting manager %q (phase %s) and starting FRESH (previous session discarded)", r.subjectName(), r.app.Name, phase),
			"--fresh delete")
	case verbResume:
		if rec.Phase == scion.PhaseStopped {
			// scion's hub resumes a stopped record with a fresh harness session
			// (it passes --continue for a suspended one only), so say so: the old
			// conversation is still in the agent home.
			r.d.Log("%s: manager %q is in phase stopped; scion resumes a stopped record in a new claude session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)", r.subjectName(), r.app.Name)
		}
		return r.resumeOrRecover(ctx, jp, resumeVerbPlain(func() error {
			return r.d.managerScion().Resume(ctx, r.app.Name, jp)
		}))
	case verbResumeForce:
		// A fresh ticket is staged, as for a suspended record, since the leaf may
		// have lapsed while wedged. A forced resume that fails keeps the record,
		// like any failed resume, so a container state corrupted by a VM reboot
		// never costs the conversation.
		return r.resumeOrRecover(ctx, jp, resumeVerbForce(func() error {
			return r.d.managerScion().ResumeForce(ctx, r.app.Name, jp)
		}))
	}
	return nil
}

// swapConversation is ManagerRestore: the record is suspended if it runs,
// the caller's swap runs with no agent process, and the manager is resumed
// through the resume checks and gated. Up to the end of the swap a failure
// resumes a manager this call suspended; after it the manager is resumed
// only by the plan, and a failure leaves it suspended.
func (r *run) swapConversation(ctx context.Context, jp string, q ManagerRequest) (res ManagerResult, err error) {
	sc := r.d.managerScion()
	agents, err := sc.List(ctx, jp)
	if err != nil {
		return res, fmt.Errorf("the hub does not answer (%s); bring the instance up first (`lever up --no-attach`)", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	if err := r.refuseUnqualifiedRecord(scion.FindAgent(agents, r.app.Name)); err != nil {
		return res, err
	}
	p, err := planManager(q, r.app.Name, observationOf(scion.FindAgent(agents, r.app.Name)))
	if err != nil {
		return res, err
	}
	// suspended: this call suspended the manager; swapped: the caller's swap
	// is done, so a failure no longer resumes it as it was.
	suspended, swapped := false, false
	if p.suspend {
		r.d.Log("conversation: suspending manager %q to swap its transcripts", r.app.Name)
		if err := sc.Suspend(ctx, r.app.Name, jp); err != nil {
			return res, fmt.Errorf("suspending the manager failed, nothing was restored: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		}
		suspended = true
	}
	defer func() {
		if err != nil && suspended && !swapped {
			how := ""
			if q.ResumeHow != nil {
				how = q.ResumeHow()
			}
			err = r.resumeAfterFailure(ctx, jp, err, how)
		}
	}()
	if last, werr := r.waitPhase(ctx, jp, scion.PhaseSuspended); werr != nil {
		if errors.Is(werr, wait.ErrTimeout) {
			return res, fmt.Errorf("the manager did not suspend (last seen %s); nothing was restored", scion.BoundedQuote(last))
		}
		return res, werr
	}
	if err := q.WhileSuspended(ctx); err != nil {
		return res, err
	}
	swapped = true
	if err := r.d.VerifyAgentRole(ctx, path.Base(jp), r.app.Name); err != nil {
		return res, err
	}
	guard, err := beginManagerResume(ctx, r.d, r.app, jp, r.subjectName())
	if err != nil {
		return res, fmt.Errorf("%w; the conversation is restored, but the manager stays suspended: fix that, then run `lever up`", err)
	}
	res.Verb = p.verb.String()
	if err := r.resumeClearing(ctx, jp, func() error { return sc.Resume(ctx, r.app.Name, jp) }); err != nil {
		return res, guard.AfterFailure(ctx, resumeError("resume", err))
	}
	if err := guard.Resumed(ctx); err != nil {
		return res, err
	}
	if err := r.gateResumed(ctx, jp); err != nil {
		return res, guard.AfterFailure(ctx, err)
	}
	if err := guard.Live(ctx); err != nil {
		return res, err
	}
	recordGuestBoot(ctx, r.d)
	return res, nil
}

// resumeClearing runs one resume through the hub's stop-claim recovery and
// the runtime-broker-race retry.
func (r *run) resumeClearing(ctx context.Context, jp string, resume func() error) error {
	return resumeClearingStopClaim(ctx, r.d, r.app.Name, jp, r.subjectName(), func() error {
		return r.retryOnBrokerUnavailable(ctx, resume)
	})
}

// gateResumed is the full liveness gate for a manager this call resumed: a
// resume's claude --continue is waited on to its session start.
func (r *run) gateResumed(ctx context.Context, jp string) error {
	err := gateManagerLive(ctx, r.d, r.managerLive, r.app.Name, jp, r.subjectName(), nil, gateOpts{})
	return withHarnessDeathHint(ctx, r.d, err, jp, r.app.Name)
}

// waitPhase waits until the hub reads the manager in phase with no live
// container (Deps.ManagerPhaseWait). last is the phase/container status last
// seen; an exhausted wait Is wait.ErrTimeout.
func (r *run) waitPhase(ctx context.Context, jp, phase string) (last string, err error) {
	s := wait.ManagerStopped
	if b := r.d.ManagerPhaseWait; b.Attempts > 0 {
		s = wait.Attempts(s.Name, b.Attempts, b.Interval)
	}
	err = wait.For(ctx, s, func(ctx context.Context) wait.Result {
		agents, err := r.d.managerScion().List(ctx, jp)
		if errors.Is(err, scion.ErrAgentListTooLarge) {
			return wait.Fatal(err)
		}
		if err != nil {
			return wait.NotYetErr(err)
		}
		rec := scion.FindAgent(agents, r.app.Name)
		if rec == nil {
			return wait.Fatal(errors.New("the manager record disappeared"))
		}
		last = rec.Phase + "/" + rec.ContainerStatus
		if rec.Phase == phase && !scion.ContainerLive(rec.ContainerStatus) {
			return wait.Ready()
		}
		return wait.NotYet(last)
	})
	return last, err
}

// resumeAfterFailureTimeout bounds resumeAfterFailure, which runs on its own
// context: the caller's may be what was cancelled.
const resumeAfterFailureTimeout = 60 * time.Second

// resumeAfterFailure resumes a manager this call suspended, after cause,
// through the pre-role record guard and the resume checks. how names the
// conversation it continues ("" leaves it out). No liveness gate follows:
// the checks wait until the container runs.
func (r *run) resumeAfterFailure(ctx context.Context, jp string, cause error, how string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeAfterFailureTimeout)
	defer cancel()
	if gerr := r.d.VerifyAgentRole(rctx, path.Base(jp), r.app.Name); gerr != nil {
		return fmt.Errorf("%w; the manager stays suspended: %w", cause, gerr)
	}
	guard, gerr := beginManagerResume(rctx, r.d, r.app, jp, r.subjectName())
	if gerr != nil {
		return fmt.Errorf("%w; the manager stays suspended: %w", cause, gerr)
	}
	if rerr := r.d.managerScion().Resume(rctx, r.app.Name, jp); rerr != nil {
		err := fmt.Errorf("%w; the manager is suspended and resuming it failed (%s): run `lever up`", cause, termsafe.Sanitize(scion.ErrSummary(rerr)))
		return guard.AfterFailure(rctx, err)
	}
	verr := guard.Resumed(rctx)
	if verr == nil {
		verr = guard.Settle(rctx)
	}
	if verr != nil {
		return fmt.Errorf("%w; resuming the manager after that failed its mount check: %w", cause, verr)
	}
	if how != "" {
		how = " " + how
	}
	return fmt.Errorf("%w; the manager was resumed%s", cause, how)
}

// backupSuspendTimeout bounds the suspend before a backup: a hung one costs
// only a copy taken while the manager runs.
const backupSuspendTimeout = 30 * time.Second

// backupBeforeDelete runs Deps.BackupManagerConversation before a delete. A
// running record is suspended first, so the copy is the conversation as it
// ends; a failed backup resumes a record it suspended, which is then kept. An
// unlisted record (rec nil) is suspended too, and is left suspended on a
// failed backup.
func (r *run) backupBeforeDelete(ctx context.Context, jp string, rec *scion.Agent) error {
	if r.d.BackupManagerConversation == nil {
		return nil
	}
	suspended := false
	if (rec != nil && rec.Phase == scion.PhaseRunning) || r.unlisted {
		sctx, cancel := context.WithTimeout(ctx, backupSuspendTimeout)
		if err := r.d.managerScion().Suspend(sctx, r.app.Name, jp); err != nil {
			how := "copying it while it runs"
			if r.unlisted {
				how = "it may not be running; copying it as it is"
			}
			r.d.Log("conversation: could not suspend manager %q before the backup (%s); %s", r.app.Name, termsafe.Sanitize(scion.ErrSummary(err)), how)
		} else {
			suspended = true
		}
		cancel()
	}
	err := r.d.BackupManagerConversation(ctx, rec)
	switch {
	case err != nil && suspended && r.unlisted:
		// Unseen, the record may have been suspended already: a resume could
		// start a manager that was not running.
		err = fmt.Errorf("%w; manager %q was suspended for the backup and stays suspended", err, r.app.Name)
	case err != nil && suspended:
		err = r.resumeAfterFailure(ctx, jp, err, "")
	}
	return err
}

// newSession is ManagerNewSession. Up to the stop every failure resumes the
// manager, when this call suspended it, on its own conversation. After the
// stop no resume can continue the old session, so a failure leaves the
// manager stopped, marked ErrSessionEnded.
func (r *run) newSession(ctx context.Context, jp string, q ManagerRequest) (res ManagerResult, err error) {
	sc := r.d.managerScion()
	agents, err := sc.List(ctx, jp)
	if err != nil {
		return res, fmt.Errorf("the hub does not answer (%s); nothing was changed", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	if err := r.refuseUnqualifiedRecord(scion.FindAgent(agents, r.app.Name)); err != nil {
		return res, err
	}
	p, err := planManager(q, r.app.Name, observationOf(scion.FindAgent(agents, r.app.Name)))
	if err != nil {
		return res, err
	}
	suspended, stopped := false, false
	if p.suspend {
		r.d.Log("conversation: suspending manager %q to back up its conversation", r.app.Name)
		if err := sc.Suspend(ctx, r.app.Name, jp); err != nil {
			return res, fmt.Errorf("suspending the manager failed, nothing was changed: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		}
		suspended = true
	}
	defer func() {
		if err != nil && suspended && !stopped {
			err = r.resumeAfterFailure(ctx, jp, err, "on its own conversation")
		}
	}()
	if last, err := r.waitPhase(ctx, jp, scion.PhaseSuspended); errors.Is(err, scion.ErrAgentListTooLarge) {
		return res, fmt.Errorf("could not see whether the manager suspended; nothing was changed: %w", err)
	} else if err != nil {
		return res, fmt.Errorf("the manager did not suspend (last seen %s); nothing was changed", scion.BoundedQuote(last))
	}
	if err := q.WhileSuspended(ctx); err != nil {
		return res, err
	}
	r.d.Log("conversation: stopping manager %q to end its session", r.app.Name)
	if serr := sc.Stop(ctx, r.app.Name, jp); serr != nil && r.stillSuspended(ctx, jp) {
		return res, fmt.Errorf("stopping the manager failed, so its session was not ended: %s", termsafe.Sanitize(scion.ErrSummary(serr)))
	}
	if last, werr := r.waitPhase(ctx, jp, scion.PhaseStopped); werr != nil {
		if errors.Is(werr, scion.ErrAgentListTooLarge) {
			stopped = true
			return res, markedError{fmt.Errorf("lever cannot tell whether the manager's session ended and started nothing: %w", werr), ErrStopUnconfirmed}
		}
		if r.stillSuspended(ctx, jp) {
			return res, fmt.Errorf("the manager did not stop (last seen %s), so its session was not ended", scion.BoundedQuote(last))
		}
		stopped = true
		return res, markedError{fmt.Errorf("the manager did not stop in time (last seen %s), so lever cannot tell whether its session ended and started nothing", scion.BoundedQuote(last)), ErrStopUnconfirmed}
	}
	stopped = true
	ended := func(err error) error { return markedError{err, ErrSessionEnded} }
	commit, err := q.BeginSession()
	if err != nil {
		return res, ended(fmt.Errorf("recording the new session failed: %w", err))
	}
	if err := r.d.VerifyAgentRole(ctx, path.Base(jp), r.app.Name); err != nil {
		return res, ended(err)
	}
	guard, err := beginManagerResume(ctx, r.d, r.app, jp, r.subjectName())
	if err != nil {
		return res, ended(err)
	}
	r.d.Log("conversation: starting manager %q in a new session", r.app.Name)
	res.Verb = p.verb.String()
	// The task is the current manager.prompt_file, which the hub also stores
	// as the record's task. With none the hub falls back to the task it
	// stored at the last fresh create or new conversation; it offers no way to
	// clear that.
	resume := func() error { return sc.ResumeTask(ctx, r.app.Name, jp, q.Task) }
	if q.Task == "" {
		resume = func() error { return sc.Resume(ctx, r.app.Name, jp) }
	}
	if err := r.resumeClearing(ctx, jp, resume); err != nil {
		// Not ResumeFailed: its exit code and its "conversation kept" are not
		// true once the old session has ended.
		return res, ended(guard.AfterFailure(ctx, fmt.Errorf("starting the manager failed: %s", termsafe.Sanitize(scion.ErrSummary(err)))))
	}
	if err := guard.Resumed(ctx); err != nil {
		return res, ended(err)
	}
	if err := commit(); err != nil {
		r.d.Log("conversation: warning: the new session was not recorded on a skill (%v); contacts cannot post to the manager until it next starts fresh", err)
	}
	if err := r.gateResumed(ctx, jp); err != nil {
		return res, ended(guard.AfterFailure(ctx, err))
	}
	if err := guard.Live(ctx); err != nil {
		return res, ended(err)
	}
	recordGuestBoot(ctx, r.d)
	return res, nil
}

// stillSuspended reports whether the hub reads the manager suspended: a stop
// that did not land left the old session resumable.
func (r *run) stillSuspended(ctx context.Context, jp string) bool {
	agents, err := r.d.managerScion().List(ctx, jp)
	if err != nil {
		return false
	}
	rec := scion.FindAgent(agents, r.app.Name)
	return rec != nil && rec.Phase == scion.PhaseSuspended
}

// parkHealBudget and parkSuspendTimeout bound ManagerPark's two steps
// apart: a hung hub must not keep the operator from powering off.
const (
	parkHealBudget     = 20 * time.Second
	parkSuspendTimeout = 30 * time.Second
)

// park is ManagerPark. A list that fails reads as no record: the suspend
// that follows reports a hub that cannot answer.
func (r *run) park(ctx context.Context, jp string, q ManagerRequest) (ManagerResult, error) {
	sc := r.d.managerScion()
	budget := q.HealBudget
	if budget <= 0 {
		budget = parkHealBudget
	}
	hctx, cancel := context.WithTimeout(ctx, budget)
	var rec *scion.Agent
	if agents, err := sc.List(hctx, jp); err == nil {
		rec = scion.FindAgent(agents, r.app.Name)
	}
	p, err := planManager(q, r.app.Name, observationOf(rec))
	if err == nil && p.heal {
		HealAgentSession(hctx, r.d.sessionHealer(), jp, rec)
	}
	cancel()
	if err != nil {
		return ManagerResult{}, err
	}
	sctx, scancel := context.WithTimeout(ctx, parkSuspendTimeout)
	defer scancel()
	return ManagerResult{Verb: p.verb.String()}, sc.Suspend(sctx, r.app.Name, jp)
}

// reenrol is ManagerReenrol. Its suspend is not waited on: the resume that
// follows is scion's answer to it. A stop claim is cleared only on the
// manager's own container (resumeClearingStopClaim), which only the
// manager's own lapsed certificate brings about.
//
// The lock is released once the resumed container's mounts were read back,
// before the liveness gate, which can wait minutes on claude: the broker's
// heal must not keep the operator's commands out that long. What runs after
// it only reads the manager, and suspends it on a mount mismatch, and only
// while podman shows the container the heal resumed: a command that took the
// lock since may have replaced it. Without podman's id of that container the
// lock is held to the end.
func (r *run) reenrol(ctx context.Context, jp string, q ManagerRequest, release func()) (res ManagerResult, err error) {
	sc := r.d.managerScion()
	agents, err := sc.List(ctx, jp)
	if err != nil {
		return res, fmt.Errorf("listing the agents: %s", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	rec := scion.FindAgent(agents, r.app.Name)
	if err := r.refuseUnqualifiedRecord(rec); err != nil {
		return res, refused(err)
	}
	// A resume takes no --role: a record whose stored role the installed
	// scion reads as full is never bounced, before any ticket is staged.
	if err := r.d.VerifyAgentRole(ctx, path.Base(jp), r.app.Name); err != nil {
		return res, refused(err)
	}
	p, err := planManager(q, r.app.Name, observationOf(rec))
	if err != nil {
		return res, err
	}
	guard, err := beginManagerResume(ctx, r.d, r.app, jp, r.subjectName())
	if err != nil {
		return res, refused(err)
	}
	if err := q.StageTicket(ctx); err != nil {
		return res, err
	}
	res.Verb = p.verb.String()
	if p.suspend {
		res.Verb = "suspend+" + res.Verb
		if err := sc.Suspend(ctx, r.app.Name, jp); err != nil {
			return res, guard.AfterFailure(ctx, fmt.Errorf("suspend: %s", termsafe.Sanitize(scion.ErrSummary(err))))
		}
	}
	resume := func() error { return sc.Resume(ctx, r.app.Name, jp) }
	if p.verb == verbResumeForce {
		resume = func() error { return sc.ResumeForce(ctx, r.app.Name, jp) }
	}
	if err := r.resumeClearing(ctx, jp, resume); err != nil {
		return res, guard.AfterFailure(ctx, fmt.Errorf("%s failed: %w", res.Verb, err))
	}
	if err := guard.Resumed(ctx); err != nil {
		return res, refused(err)
	}
	// The container runs in this boot; the record is written under the lock.
	recordGuestBoot(ctx, r.d)
	ref := jail.ContainerName(path.Base(jp), r.app.Name)
	id := r.containerID(ctx, ref)
	if id != "" {
		release()
	}
	gerr := r.gateResumed(ctx, jp)
	if id != "" {
		if err := r.stillContainer(ctx, ref, id); err != nil {
			return res, errors.Join(gerr, err)
		}
	}
	if gerr != nil {
		return res, guard.AfterFailure(ctx, gerr)
	}
	if err := guard.Live(ctx); err != nil {
		return res, refused(err)
	}
	res.MountsUnverified = !guard.Verified()
	return res, nil
}

// containerID is podman's id of the container ref, or "" when podman cannot
// say.
func (r *run) containerID(ctx context.Context, ref string) string {
	if r.d.StaleContainer == nil {
		return ""
	}
	st, err := r.d.StaleContainer.State(ctx, ref)
	if err != nil {
		return ""
	}
	return st.ID
}

// stillContainer fails unless podman shows ref as the container id.
func (r *run) stillContainer(ctx context.Context, ref, id string) error {
	switch got := r.containerID(ctx, ref); got {
	case id:
		return nil
	case "":
		return fmt.Errorf("podman could not show manager %q's container after the wait for claude, so lever does not act on it: another command may have replaced it since the lock was released", r.app.Name)
	default:
		return fmt.Errorf("manager %q's container was replaced while lever waited for claude (another command took the lock), so lever leaves the new one alone", r.app.Name)
	}
}

// resetToken is ManagerTokenReset. The hub mints the new token from the
// record's stored role, which on an old record can read as full, so the
// pre-role record guard runs first. Verb is "" when the manager no longer
// runs.
func (r *run) resetToken(ctx context.Context, jp string, q ManagerRequest) (ManagerResult, error) {
	sc := r.d.managerScion()
	agents, err := sc.List(ctx, jp)
	if err != nil {
		return ManagerResult{}, fmt.Errorf("listing the agents: %s", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	p, err := planManager(q, r.app.Name, observationOf(scion.FindAgent(agents, r.app.Name)))
	if err != nil || p.verb == verbNone {
		return ManagerResult{}, err
	}
	if err := r.d.VerifyAgentRole(ctx, path.Base(jp), r.app.Name); err != nil {
		return ManagerResult{}, refused(err)
	}
	if err := sc.ResetAuth(ctx, r.app.Name, jp); err != nil {
		return ManagerResult{}, err
	}
	return ManagerResult{Verb: p.verb.String()}, nil
}
