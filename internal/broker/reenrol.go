package broker

import (
	"context"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/scion"
)

// Natural-lapse auto-re-enrol (#22): the mTLS listener's LapseFunc observes a
// client cert that is OUR CA's, otherwise-valid, merely expired (ca.LapseFunc
// — the classification is cryptographic, so a forged or foreign cert can
// never reach here as a "lapse"). The healer then restores the agent without
// operator action: it re-stages a fresh one-use enrolment ticket (the same
// host-minted authority `lever up` uses) and bounces the agent so its boot
// re-enrols; `claude --continue` restores the conversation. Broker-side
// policy still gates every heal: the CN must be a CONFIGURED identity, not
// revoked, and permitted by the auto_reenrol mode. Attempts are bounded by a
// per-CN cooldown and lifetime cap so a persistent failure can never
// heal-storm.
const (
	// autoReenrol* mirror config.AutoReenrolAll/Manager/Off by hand: the
	// broker package must not import config, so brokerctl passes the resolved
	// mode as a string (string(app.EffectiveAutoReenrol())). Keep the two
	// sets in step.
	autoReenrolAll     = "all"
	autoReenrolManager = "manager"
	autoReenrolOff     = "off"

	reenrolCooldown    = 10 * time.Minute
	reenrolMaxAttempts = 3 // per CN within a burst; resets on success or after reenrolResetAfter of quiet
	reenrolResetAfter  = time.Hour
	reenrolQueueDepth  = 16
)

// lapseFunc returns the hook the mTLS listener installs, or nil when the
// healer is disabled (mode off, or no runtime to drive). The hook runs on
// handshake goroutines, so it only enqueues — a full queue drops the event
// (the next failing handshake re-emits it).
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

// runHealer drains lapse events for the life of ctx. Started by Serve.
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

// healLapse is one auto-re-enrol attempt for cn. Synchronous; called only
// from the healer goroutine (and directly by tests). The steps, in order:
// policy gate, throttle, revocation, ticket re-stage, bounce.
func (b *Broker) healLapse(ctx context.Context, cn string) {
	dir, slug, ok := b.healTarget(cn)
	if !ok {
		return
	}
	if !b.admitReenrol(cn) {
		return
	}
	// Revocation makes expiry a real kill-switch — `lever revoke` wins over
	// the healer. Checked right before ticket mint to keep the window between
	// check and use minimal.
	if b.isRevoked(cn) {
		b.audit("reenrol", cn, "deny", "revoked identity presented an expired leaf — not healing")
		return
	}
	if dir == "" {
		b.audit("reenrol", cn, "error", "no bootstrap dir configured for this identity")
		return
	}
	// Re-stage a fresh one-use ticket (host authority, same as `lever up`). The
	// helper's "ticket:"/"stage:" wrap prefixes name the failed step in the
	// audit line.
	if err := b.stageFreshTicket(cn, dir); err != nil {
		b.audit("reenrol", cn, "error", err.Error())
		return
	}
	verb, ok := b.bounceForReenrol(ctx, cn, slug)
	if !ok {
		return
	}
	// Success resets the cap so the NEXT independent lapse (weeks later) can
	// heal again; the cap only bounds consecutive failures.
	b.reenrolMu.Lock()
	b.reenrolTries[cn] = 0
	b.reenrolMu.Unlock()
	b.audit("reenrol", cn, "allow", "natural lapse: ticket re-staged, healed via "+verb)
}

// healTarget applies the policy gates (mode, configured identity) and
// resolves where the heal acts: the bootstrap dir to re-stage into and the
// scion slug to bounce, which differ between manager and worker. ok is false
// when cn is not healable at all; dir may be "" for a configured identity
// with no bootstrap dir, which the caller audits.
func (b *Broker) healTarget(cn string) (dir, slug string, ok bool) {
	switch b.autoReenrol {
	case autoReenrolOff:
		return "", "", false
	case autoReenrolManager:
		if cn != b.manager {
			return "", "", false
		}
	}
	spec, isWorker := b.workerSpec(cn)
	if cn != b.manager && !isWorker {
		return "", "", false
	}
	if isWorker {
		return spec.BootstrapDir, spec.Name, true
	}
	return b.managerBootstrapDir, b.managerSlug, true
}

// admitReenrol is the per-CN cooldown + attempt cap, and records the attempt
// it admits. Deliberately BEFORE the revoked check in healLapse so the
// revoked deny-audit is throttled to the same cadence as every other outcome
// — a revoked cert hammering handshakes must not write one audit line per
// attempt.
func (b *Broker) admitReenrol(cn string) bool {
	now := b.reenrolNow()
	b.reenrolMu.Lock()
	defer b.reenrolMu.Unlock()
	if last, ok := b.reenrolLast[cn]; ok {
		if now.Sub(last) < reenrolCooldown {
			return false
		}
		// A long-quiet CN starts a fresh burst: without this, 3 failed attempts
		// would disable healing for that CN until the broker restarts.
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

// bounceForReenrol restarts agent slug by its observed phase so boot
// re-enrols with the staged ticket. It audits every failure itself and
// returns the verb it used for the caller's success line; ok is false when
// the heal must stop here.
func (b *Broker) bounceForReenrol(ctx context.Context, cn, slug string) (verb string, ok bool) {
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		b.audit("reenrol", cn, "error", "list: "+err.Error())
		return "", false
	}
	phase := ""
	for _, a := range agents {
		if a.Slug == slug {
			phase = a.Phase
			break
		}
	}
	// The healer bounces an agent through resume, so it meets the same pre-role
	// record hazard as an operator-driven resume (see DispatchConfig.VerifyAgentRole).
	// Abandoning the heal is the safe answer: a lapsed leaf costs that agent its
	// brokered tools, while healing it into full hub authority costs the
	// instance its containment.
	if err = b.checkAgentRole(ctx, slug); err != nil {
		b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": "+err.Error())
		return "", false
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
		b.audit("reenrol", cn, "error", "natural lapse: "+verb+" failed: "+err.Error())
		return "", false
	}
	return verb, true
}
