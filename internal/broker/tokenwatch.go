package broker

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// HubTokenHealer reads whether a running agent's hub token expired and gives
// it a new one (brokerctl: jail.AgentProbe over the jail runner, and the
// host scion client's ResetAuth under the controller PAT). An agent's hub
// token is scion's, not lever's: sciontool refreshes it 2 h before its 10 h
// expiry with a Go timer, which stands still while the host sleeps, and an
// expired token cannot refresh itself — every reply, status update and
// heartbeat of that agent then fails with 401 while its container, phase and
// broker certificate all look healthy.
type HubTokenHealer interface {
	TokenExpired(ctx context.Context, agent string) (bool, error)
	ResetAuth(ctx context.Context, agent string) error
}

const (
	// tokenWatchInterval is how often the watch reads the agents' hub
	// tokens. The broker runs on the host, so after a sleep the next tick
	// comes within this long of the wake — the moment a stalled refresh
	// has let a token lapse.
	tokenWatchInterval = 5 * time.Minute
	// tokenHealCooldown spaces two resets of one agent: a reset that did
	// not take (or a token file the agent keeps rewriting as expired) costs
	// one hub mint per cooldown, never one per tick.
	tokenHealCooldown = 15 * time.Minute
)

// tokenWatchAgents is who the watch may heal, by the auto_reenrol mode the
// healer of broker certificates uses (reenrol.go): all agents, the manager
// only, or none. Slugs, in a stable order: the manager first, then workers.
func (b *Broker) tokenWatchAgents() []string {
	switch b.autoReenrol {
	case autoReenrolOff:
		return nil
	case autoReenrolManager:
		return []string{b.managerSlug}
	}
	out := []string{b.managerSlug}
	names := make([]string, 0, len(b.workers))
	for name := range b.workers {
		names = append(names, name)
	}
	slices.Sort(names)
	return append(out, names...)
}

// tokenWatchEnabled reports whether Serve starts the watch: a healer and a
// runtime are wired, and the auto_reenrol mode covers at least one agent.
func (b *Broker) tokenWatchEnabled() bool {
	return b.hubTokens != nil && b.runtime != nil && len(b.tokenWatchAgents()) > 0
}

// runTokenWatch heals expired agent hub tokens for the life of ctx: one pass
// at once (a broker restart often follows the wake that caused the lapse),
// then one per tokenWatchInterval. Started by Serve when a healer and a
// runtime are wired and the mode is not off.
func (b *Broker) runTokenWatch(ctx context.Context) {
	b.healHubTokens(ctx)
	t := time.NewTicker(b.tokenWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.healHubTokens(ctx)
		}
	}
}

// healHubTokens is one pass: list the instance's agents once, and for every
// agent the mode covers whose record is running over a live container and
// whose token reads expired, run a reset (at most once per cooldown). A
// revoked identity is not healed: `lever revoke` stays the kill-switch.
//
// The expiry is read from a file in the agent's own container, so an agent
// can claim an expired token. All that buys it is a reset of its own token:
// the hub mints the new one from the agent's stored role and scopes (scion
// DispatchAgentResetAuth), so nothing widens and no other agent is touched.
// Every outcome is audited with lever's words only.
func (b *Broker) healHubTokens(ctx context.Context) {
	agents := b.tokenWatchAgents()
	if b.hubTokens == nil || b.runtime == nil || len(agents) == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, tokenWatchListTimeout)
	recs, err := b.runtime.List(lctx, b.instanceProject)
	cancel()
	if err != nil {
		return // a hub or jail that cannot answer is not this watch's finding
	}
	for _, slug := range agents {
		a := scion.FindAgent(recs, slug)
		if a == nil || scion.PhaseLabel(a.Phase) != scion.PhaseRunning || !scion.ContainerLive(a.ContainerStatus) {
			continue
		}
		cn := slug
		if slug == b.managerSlug {
			cn = b.manager
		}
		if b.isRevoked(cn) {
			continue
		}
		// Each agent under its own deadline, from the watch's context and
		// not a shared pass budget: the read is an exec into a container
		// the agent controls, and one that hangs must not cost the other
		// agents their heal.
		actx, cancel := context.WithTimeout(ctx, tokenWatchAgentTimeout)
		expired, err := b.hubTokens.TokenExpired(actx, slug)
		if err == nil && expired {
			b.healOneHubToken(actx, cn, slug)
		}
		cancel()
	}
}

// healOneHubToken resets one agent's expired token, gated like every other
// broker action that has the hub mint for an agent: cooldown, the worker's
// lifecycle lock (a busy worker is skipped until the next tick, so a reset
// never interleaves with a start, resume, wake, stop or suspend of it),
// revocation again after the lock wait (a `lever revoke` that landed during
// it wins), and the pre-role record guard. The hub mints the new token from
// the record's stored role, and a record created before scion#1089 carries
// a role scion's migration grandfathered to full (or, on older pins, none,
// which resolved to full): the guard (hubapi.VerifyAgentRole) refuses both,
// as the resume paths do. With no guard wired at all the reset is refused.
func (b *Broker) healOneHubToken(ctx context.Context, cn, slug string) {
	now := b.reenrolNow()
	b.reenrolMu.Lock()
	last, seen := b.tokenHealLast[slug]
	if seen && now.Sub(last) < tokenHealCooldown {
		b.reenrolMu.Unlock()
		return
	}
	b.tokenHealLast[slug] = now
	b.reenrolMu.Unlock()
	if _, isWorker := b.workerSpec(slug); isWorker {
		lctx, cancel := context.WithTimeout(ctx, cmp.Or(b.reenrolLockWait, reenrolLockWaitDefault))
		unlock, err := b.lockWorker(lctx, slug)
		cancel()
		if err != nil {
			// Not an attempt: the next tick may try again at once.
			b.reenrolMu.Lock()
			delete(b.tokenHealLast, slug)
			b.reenrolMu.Unlock()
			return
		}
		defer unlock()
	}
	if b.isRevoked(cn) {
		return
	}
	if b.verifyRole == nil {
		b.audit("hub-token", cn, "deny", "expired agent hub token NOT reset: no pre-role record guard is wired")
		return
	}
	if err := b.verifyRole(ctx, slug); err != nil {
		b.audit("hub-token", cn, "deny", "expired agent hub token NOT reset: "+termsafe.Sanitize(err.Error()))
		return
	}
	if err := b.hubTokens.ResetAuth(ctx, slug); err != nil {
		b.audit("hub-token", cn, "error", "expired agent hub token: scion reset-auth failed: "+termsafe.Sanitize(scion.ErrSummary(err)))
		return
	}
	b.audit("hub-token", cn, "allow", "expired agent hub token reset (scion reset-auth, the agent's own role)")
}

// tokenWatchListTimeout bounds a pass's one agent listing, and
// tokenWatchAgentTimeout one agent's token read, lock wait, guard and reset
// (the read itself is bounded tighter, by jail.BoundAgentExec).
const tokenWatchListTimeout = 30 * time.Second

// tokenWatchAgentTimeout is the per-agent bound; a var so a test can
// shrink it.
var tokenWatchAgentTimeout = 90 * time.Second
