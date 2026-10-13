package broker

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/scion"
)

// Natural-lapse auto-re-enrol: the mTLS listener reports a client cert that
// is our CA's and otherwise valid but expired (ca.LapseFunc; the check is
// cryptographic, so a forged or foreign cert never counts as a lapse). The
// healer re-stages a fresh one-use ticket and bounces the agent so its boot
// re-enrols. Every heal is still gated: the CN must be configured, not
// revoked, and allowed by auto_reenrol, and a per-CN cooldown and attempt cap
// prevent a heal storm.
const (
	// autoReenrol* mirror config.AutoReenrolAll/Manager/Off by hand (the
	// broker must not import config). Keep the two sets in step.
	autoReenrolAll     = "all"
	autoReenrolManager = "manager"
	autoReenrolOff     = "off"

	reenrolCooldown    = 10 * time.Minute
	reenrolMaxAttempts = 3 // per CN within a burst; resets on success or after reenrolResetAfter of quiet
	reenrolResetAfter  = time.Hour
	reenrolQueueDepth  = 16
)

// lapseFunc returns nil when the healer is disabled. The hook runs on
// handshake goroutines, so it only enqueues; a full queue drops the event and
// the next failing handshake re-emits it.
func (b *Broker) lapseFunc() ca.LapseFunc {
	if b.autoReenrol == autoReenrolOff || b.runtime == nil {
		return nil
	}
	return func(cn string) {
		select {
		case b.reenrolEvents <- cn:
		default:
		}
	}
}

const reenrolLockWaitDefault = 2 * time.Second

func (b *Broker) runHealer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cn := <-b.reenrolEvents:
			b.healLapse(ctx, cn)
		}
	}
}

const reenrolHealTimeout = 5 * time.Minute

func (b *Broker) healLapse(ctx context.Context, cn string) {
	stage, slug, ok := b.healTarget(cn)
	if !ok {
		return
	}
	if !b.admitReenrol(cn) {
		return
	}
	// `lever revoke` wins over the healer, so expiry stays a kill-switch.
	if b.isRevoked(cn) {
		b.audit("reenrol", cn, "deny", "revoked identity presented an expired leaf — not healing")
		return
	}
	// Before a ticket is staged: a refused heal must not leave a fresh
	// ticket behind. bounceForReenrol checks again before the resume.
	if spec, isWorker := b.workerSpec(cn); isWorker {
		if err := b.verifyStrictWorkspace(spec); err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+spec.Name+": workspace dir: "+err.Error())
			return
		}
		if err := b.checkSharedAccess(ctx, spec, true); err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+spec.Name+": "+err.Error())
			return
		}
	}
	// A worker's heal holds its lifecycle lock from the stage to the end of
	// the bounce, so it cannot undo a stop or race a wake. The wait is
	// bounded since this goroutine heals every agent; a busy lock skips the
	// heal without counting it, and the next lapse retries.
	if _, isWorker := b.workerSpec(cn); isWorker {
		lctx, cancel := context.WithTimeout(ctx, cmp.Or(b.reenrolLockWait, reenrolLockWaitDefault))
		unlock, err := b.lockWorker(lctx, cn)
		cancel()
		if err != nil {
			// Not below zero: a resume that held the lock may have reset it.
			b.reenrolMu.Lock()
			b.reenrolTries[cn] = max(0, b.reenrolTries[cn]-1)
			b.reenrolMu.Unlock()
			b.audit("reenrol", cn, "error", "natural lapse: worker busy (another start, resume, wake, stop or suspend of it is under way); heal skipped, the next lapse retries")
			return
		}
		defer unlock()
		// A revoke that landed during the lock wait must still win.
		if b.isRevoked(cn) {
			b.audit("reenrol", cn, "deny", "revoked identity presented an expired leaf — not healing")
			return
		}
	}
	// Bounded: the scion CLI has no timeout, and a hung hub would otherwise
	// hold the worker's lifecycle lock for good.
	hctx, hcancel := context.WithTimeout(ctx, reenrolHealTimeout)
	defer hcancel()
	// The bounce is a resume, so it meets the pre-role record hazard
	// (DispatchConfig.VerifyAgentRole). Abandoning the heal is safe: a lapsed
	// leaf costs the agent its tools, full hub authority costs containment.
	// Before the stage, so a refusal leaves no ticket behind.
	if err := b.checkAgentRole(hctx, slug); err != nil {
		b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": "+err.Error())
		return
	}
	// The manager's bounce gets apply's resume checks, the first before the
	// stage.
	var afterResume ManagerResumeCheck
	if _, isWorker := b.workerSpec(cn); !isWorker {
		if b.managerResumeGuard == nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": no check of the manager's tree mounts is wired; run `lever up`")
			return
		}
		after, err := b.managerResumeGuard(hctx)
		if err == nil && after == nil {
			err = errors.New("no post-resume check")
		}
		if err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": "+err.Error())
			return
		}
		afterResume = after
	}
	if err := stage(hctx); err != nil {
		b.audit("reenrol", cn, "error", err.Error())
		return
	}
	verb, ok := b.bounceForReenrol(hctx, cn, slug, afterResume)
	if !ok {
		return
	}
	if afterResume != nil && !afterResume.Verified() {
		verb += "; warning: the manager's tree mounts could not be checked after the resume, run `lever doctor` (row \"manager read-only paths\")"
	}
	// The cap only bounds consecutive failures.
	b.resetReenrolTries(cn)
	b.audit("reenrol", cn, "allow", "natural lapse: ticket re-staged, healed via "+verb)
}

// resetReenrolTries is also called by any start or resume that saw the worker
// live, so earlier failed heals do not use up the next lapse's cap.
func (b *Broker) resetReenrolTries(cn string) {
	b.reenrolMu.Lock()
	b.reenrolTries[cn] = 0
	b.reenrolMu.Unlock()
}

// healTarget applies the policy gates (mode, configured identity) and returns
// the re-stage step and slug to bounce. A worker's ticket goes to the guest,
// never the tree.
func (b *Broker) healTarget(cn string) (stage func(context.Context) error, slug string, ok bool) {
	switch b.autoReenrol {
	case autoReenrolOff:
		return nil, "", false
	case autoReenrolManager:
		if cn != b.manager {
			return nil, "", false
		}
	}
	spec, isWorker := b.workerSpec(cn)
	if cn != b.manager && !isWorker {
		return nil, "", false
	}
	if isWorker {
		return func(ctx context.Context) error { return b.stageWorkerTicket(ctx, spec) }, spec.Name, true
	}
	return func(context.Context) error {
		if b.managerBootstrapDir == "" {
			return errors.New("no bootstrap dir configured for this identity")
		}
		return b.stageFreshTicket(cn, b.managerBootstrapDir)
	}, b.managerSlug, true
}

// admitReenrol is the per-CN cooldown and attempt cap. It runs BEFORE the
// revoked check so a revoked cert hammering handshakes cannot write one audit
// line per attempt.
func (b *Broker) admitReenrol(cn string) bool {
	now := b.reenrolNow()
	b.reenrolMu.Lock()
	defer b.reenrolMu.Unlock()
	if last, ok := b.reenrolLast[cn]; ok {
		if now.Sub(last) < reenrolCooldown {
			return false
		}
		// A long-quiet CN starts a fresh burst, or the cap would last until
		// restart.
		if now.Sub(last) >= reenrolResetAfter {
			b.reenrolTries[cn] = 0
		}
	}
	if b.reenrolTries[cn] >= reenrolMaxAttempts {
		return false
	}
	b.reenrolLast[cn] = now
	b.reenrolTries[cn]++
	return true
}

// bounceForReenrol restarts slug by its observed phase so boot re-enrols with
// the staged ticket. It audits every failure itself.
func (b *Broker) bounceForReenrol(ctx context.Context, cn, slug string, afterResume ManagerResumeCheck) (verb string, ok bool) {
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		b.audit("reenrol", cn, "error", "list: "+err.Error())
		return "", false
	}
	phase := ""
	for _, a := range agents {
		if a.Slug == slug {
			phase = scion.PhaseLabel(a.Phase)
			break
		}
	}
	// A resume re-resolves the workspace bind source (as in resumeRecord).
	if spec, isWorker := b.workers[slug]; isWorker {
		if err = b.verifyStrictWorkspace(spec); err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": workspace dir: "+err.Error())
			return "", false
		}
		if err = b.checkSharedAccess(ctx, spec, true); err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": "+err.Error())
			return "", false
		}
	}
	// Resuming a non-running worker takes a slot; a running one holds its
	// own.
	resumes := phase == scion.PhaseSuspended || phase == scion.PhaseStopped || phase == scion.PhaseError
	if _, isWorker := b.workers[slug]; isWorker && resumes {
		release, err := b.admitWorker(ctx, slug)
		if err != nil {
			outcome := "error"
			if errors.Is(err, ErrWorkerCap) {
				outcome = "deny"
			}
			b.audit("reenrol", cn, outcome, "natural lapse: not resuming "+slug+": "+err.Error())
			return "", false
		}
		defer release()
	}
	switch phase {
	case scion.PhaseRunning:
		verb = "suspend+resume"
		if err = b.runtime.Suspend(ctx, slug, b.instanceProject); err == nil {
			err = b.runtime.Resume(ctx, slug, b.instanceProject)
		}
	case scion.PhaseSuspended, scion.PhaseStopped:
		verb = "resume"
		err = b.runtime.Resume(ctx, slug, b.instanceProject)
	case scion.PhaseError:
		verb = "resume --force"
		err = b.runtime.ResumeForce(ctx, slug, b.instanceProject)
	default:
		b.audit("reenrol", cn, "error", "natural lapse detected but agent "+slug+" is in phase "+phase+" — not bounceable, run `lever up`")
		return "", false
	}
	if err != nil {
		if afterResume != nil {
			err = afterResume.AfterFailure(ctx, err)
		}
		b.audit("reenrol", cn, "error", "natural lapse: "+verb+" failed: "+err.Error())
		return "", false
	}
	if afterResume != nil {
		if err := afterResume.After(ctx); err != nil {
			b.audit("reenrol", cn, "deny", "natural lapse: "+verb+" done, then the manager's tree mounts failed their check: "+err.Error())
			return "", false
		}
	}
	return verb, true
}
