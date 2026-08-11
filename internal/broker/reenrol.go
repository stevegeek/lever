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
	// autoReenrol* mirror config.AutoReenrolMode values (the broker package
	// stays decoupled from config; brokerctl passes the resolved string).
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
// from the healer goroutine (and directly by tests).
func (b *Broker) healLapse(ctx context.Context, cn string) {
	// Mode gate.
	switch b.autoReenrol {
	case autoReenrolOff:
		return
	case autoReenrolManager:
		if cn != b.manager {
			return
		}
	}

	// Identity gate: only configured identities are healable.
	spec, isWorker := b.workers[cn]
	if cn != b.manager && !isWorker {
		return
	}

	// Cooldown + cap. Deliberately BEFORE the revoked check so the revoked
	// deny-audit below is throttled to the same cadence as every other
	// outcome — a revoked cert hammering handshakes must not write one audit
	// line per attempt.
	now := b.reenrolNow()
	b.reenrolMu.Lock()
	if last, ok := b.reenrolLast[cn]; ok {
		if now.Sub(last) < reenrolCooldown {
			b.reenrolMu.Unlock()
			return
		}
		// A long-quiet CN starts a fresh burst: without this, 3 failed attempts
		// would disable healing for that CN until the broker restarts.
		if now.Sub(last) >= reenrolResetAfter {
			b.reenrolTries[cn] = 0
		}
	}
	if b.reenrolTries[cn] >= reenrolMaxAttempts {
		b.reenrolMu.Unlock()
		return
	}
	b.reenrolLast[cn] = now
	b.reenrolTries[cn]++
	b.reenrolMu.Unlock()

	// Revocation makes expiry a real kill-switch — `lever revoke` wins over
	// the healer. Checked right before ticket mint to keep the window between
	// check and use minimal.
	if b.isRevoked(cn) {
		b.audit("reenrol", cn, "deny", "revoked identity presented an expired leaf — not healing")
		return
	}

	// Target: bootstrap dir + scion slug differ between manager and worker.
	dir, slug := b.managerBootstrapDir, b.managerSlug
	if isWorker {
		dir, slug = spec.BootstrapDir, spec.Name
	}
	if dir == "" {
		b.audit("reenrol", cn, "error", "no bootstrap dir configured for this identity")
		return
	}

	// Re-stage a fresh one-use ticket (host authority, same as `lever up`). The
	// helper's step-discriminated wrap ("ticket:"/"stage:") reproduces the two
	// audit lines this path emitted before the shared extraction.
	if err := b.stageFreshTicket(cn, dir); err != nil {
		b.audit("reenrol", cn, "error", err.Error())
		return
	}

	// Bounce by observed phase so boot re-enrols with the staged ticket.
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		b.audit("reenrol", cn, "error", "list: "+err.Error())
		return
	}
	phase := ""
	for _, a := range agents {
		if a.Slug == slug {
			phase = a.Phase
			break
		}
	}
	// The healer bounces an agent through resume, so it meets the same pre-role
	// record hazard as an operator-driven resume (see Config.VerifyAgentRole).
	// Abandoning the heal is the safe answer: a lapsed leaf costs that agent its
	// brokered tools, while healing it into full hub authority costs the
	// instance its containment.
	if err = b.checkAgentRole(ctx, slug); err != nil {
		b.audit("reenrol", cn, "deny", "natural lapse: refusing to bounce "+slug+": "+err.Error())
		return
	}
	var verb string
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
		return
	}
	if err != nil {
		b.audit("reenrol", cn, "error", "natural lapse: "+verb+" failed: "+err.Error())
		return
	}
	// Success resets the cap so the NEXT independent lapse (weeks later) can
	// heal again; the cap only bounds consecutive failures.
	b.reenrolMu.Lock()
	b.reenrolTries[cn] = 0
	b.reenrolMu.Unlock()
	b.audit("reenrol", cn, "allow", "natural lapse: ticket re-staged, healed via "+verb)
}
