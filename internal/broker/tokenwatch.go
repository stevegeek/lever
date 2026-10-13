package broker

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// HubTokenHealer reads whether a running agent's hub token expired and resets
// it. sciontool refreshes the token with a Go timer that stands still while
// the host sleeps, and an expired token cannot refresh itself: the agent then
// gets 401 on every hub call while everything else looks healthy.
type HubTokenHealer interface {
	TokenExpired(ctx context.Context, agent string) (bool, error)
	ResetAuth(ctx context.Context, agent string) error
}

const (
	tokenWatchInterval = 5 * time.Minute
	// tokenHealCooldown: a reset that did not take, or an agent that keeps
	// claiming expiry, costs one hub mint per cooldown, never one per tick.
	tokenHealCooldown = 15 * time.Minute
)

// tokenWatchAgents is who the watch may heal, by the auto_reenrol mode.
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

func (b *Broker) tokenWatchEnabled() bool {
	return b.hubTokens != nil && b.runtime != nil && len(b.tokenWatchAgents()) > 0
}

// runTokenWatch runs one pass at once (a restart often follows the wake that
// caused the lapse), then one per interval.
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

// healHubTokens is one pass. A revoked identity is not healed: `lever revoke`
// stays the kill-switch.
//
// The expiry is read from the agent's own container, so an agent can claim an
// expired token. That buys only a reset of its own token from its stored role
// and scopes: nothing widens, no other agent is touched. Audits use lever's
// words only.
func (b *Broker) healHubTokens(ctx context.Context) {
	agents := b.tokenWatchAgents()
	if b.hubTokens == nil || b.runtime == nil || len(agents) == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, tokenWatchListTimeout)
	recs, err := b.runtime.List(lctx, b.instanceProject)
	cancel()
	if err != nil {
		return
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
		// A per-agent deadline: the read execs into a container the agent
		// controls, and one that hangs must not cost the others their heal.
		actx, cancel := context.WithTimeout(ctx, tokenWatchAgentTimeout)
		expired, err := b.hubTokens.TokenExpired(actx, slug)
		if err == nil && expired {
			b.healOneHubToken(actx, cn, slug)
		}
		cancel()
	}
}

// healOneHubToken is gated like every hub mint for an agent: cooldown, the
// worker's lifecycle lock (busy is skipped), revocation after the lock wait,
// and the pre-role record guard. The hub mints from the record's stored role,
// which on an old record can resolve to full, so the guard must refuse it as
// the resume paths do; with no guard wired the reset is refused.
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
			// Not an attempt: the next tick may try again.
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

const tokenWatchListTimeout = 30 * time.Second

// tokenWatchAgentTimeout is a var so a test can shrink it.
var tokenWatchAgentTimeout = 90 * time.Second
