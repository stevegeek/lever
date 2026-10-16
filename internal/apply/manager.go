package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/imageref"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
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
)

func (i ManagerIntent) String() string {
	switch i {
	case ManagerKeep:
		return "keep"
	case ManagerFresh:
		return "fresh"
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
	// BrokerOnline: the runtime broker was seen online since the hub last
	// started, so no wait for it is needed.
	BrokerOnline bool
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
	release, err := d.ManagerLock(cmpOr(q.What, subject))
	if err != nil {
		return ManagerResult{}, err
	}
	defer release()
	r := &run{app: app, d: d,
		brokerStart:  d.BrokerStartRetry.or(defaultBrokerStartRetry),
		managerLive:  d.ManagerLiveRetry.or(defaultManagerLiveRetry),
		phaseSettle:  d.PhaseSettleRetry.or(defaultPhaseSettleRetry),
		fresh:        q.Intent == ManagerFresh,
		attach:       q.Attach,
		freshNoConv:  q.FreshWithoutConversation,
		minted:       q.TicketStaged,
		brokerOnline: q.BrokerOnline,
		subject:      subject,
	}
	jp := JailPath(app.Tree, app.Tree, d.JailMount)
	switch q.Intent {
	case ManagerKeep, ManagerFresh:
		return r.ensureUp(ctx, jp, q)
	}
	return ManagerResult{}, fmt.Errorf("%s: internal error: unknown manager intent %s", subject, q.Intent)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// managerObservation is the one reading of the manager a plan is made from.
type managerObservation struct {
	present          bool
	phase, container string
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
	return managerObservation{present: true, phase: rec.Phase, container: rec.ContainerStatus}
}

// managerVerb is the one change a plan makes to the record.
type managerVerb int

const (
	verbNone managerVerb = iota
	verbCreate
	verbResume
	verbResumeForce
	verbDeleteCreate
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
	}
	return ""
}

// managerPlan is planManager's answer.
type managerPlan struct {
	// recover: suspend the record first, read it again and plan again.
	recover bool
	verb    managerVerb
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
	}
	return managerPlan{}, fmt.Errorf("%s: internal error: unknown manager intent %s", q.subject(), q.Intent)
}

// observeUp is the reading ManagerKeep and ManagerFresh plan from: the
// record settled and role-checked (observeManager); for Keep its session
// healed and, for a running record, podman's word on its container.
func (r *run) observeUp(ctx context.Context, jp string) (*scion.Agent, managerObservation, error) {
	rec, err := r.observeManager(ctx, jp)
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
// container column, and a container podman lists as created (a start under
// way) does not. Any read that fails answers no.
func containerGoneUnder(ctx context.Context, d Deps, project, name string, rec *scion.Agent) bool {
	if rec == nil || rec.Phase != scion.PhaseRunning || d.StaleContainer == nil {
		return false
	}
	st, err := d.StaleContainer.State(ctx, jail.ContainerName(path.Base(project), name))
	switch {
	case errors.Is(err, jail.ErrNoContainer):
		return true
	case err != nil, st.Running, st.Paused:
		return false
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
	r.d.logf("%s: manager %q: the hub reads it running, but podman shows its container gone or exited; suspending and resuming it (the conversation is kept)", r.subjectName(), r.app.Name)
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
	rec, o, err := r.observeUp(ctx, jp)
	if err != nil {
		return ManagerResult{}, err
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
	RecordGuestBoot(ctx, r.d)
	probes.finish()
	return res, nil
}

// actUp makes the plan's change to the record; the gate after it confirms
// the container is up, so a kept running record over a dead container fails
// loudly there.
func (r *run) actUp(ctx context.Context, jp string, rec *scion.Agent, p managerPlan, opts scion.StartOpts) error {
	if rec != nil && !r.fresh && rec.Image != "" && !strings.Contains(rec.Image, "@") && !imageref.Qualified(rec.Image) {
		// scion keeps the image a record was created with and rewrites an
		// unqualified one to the hub's image registry at every start, which an
		// agent can set between applies.
		r.d.Log("%s: WARNING: manager %q was created on the unqualified image name %s, which the hub rewrites to its image registry at every start, a setting any agent can change; recreate it on %s with `lever up --fresh --keep-conversation`",
			r.subjectName(), r.app.Name, termsafe.Sanitize(rec.Image), opts.Image)
	}
	switch p.verb {
	case verbCreate:
		return r.startManagerCreate(ctx, opts, p.mustCreate)
	case verbDeleteCreate:
		// --fresh discards the session whatever the record's phase. Decided here,
		// after the hub is up, because after lever stop the record is invisible
		// until this step. The create that follows is what puts a changed
		// manager.image into effect (a resume keeps the record's image).
		if r.d.BackupManagerConversation != nil {
			if err := r.d.BackupManagerConversation(ctx, rec); err != nil {
				return fmt.Errorf("%s: --fresh: backing up manager %q's conversation failed, so the manager was NOT deleted (its record and conversation are kept): %w. Fix the cause and run `lever up --fresh` again, or run `lever up --fresh --no-backup` to discard the conversation without a backup", r.subjectName(), r.app.Name, err)
			}
		}
		return r.recoverDeleteAndCreate(ctx, jp, opts,
			fmt.Sprintf("%s: --fresh — deleting manager %q (phase %s) and starting FRESH (previous session discarded)", r.subjectName(), r.app.Name, scion.BoundedQuote(rec.Phase)),
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
