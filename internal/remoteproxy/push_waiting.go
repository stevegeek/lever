package remoteproxy

import (
	"context"
	"time"
)

// The "waiting for you" push: when a spawned agent starts waiting at its
// prompt (the broker's agents loop sets WaitingSince), every login with a
// push subscription that may message it gets one content-free push, kind
// "waiting". Only operator logins may message a spawned agent (viewerFor),
// so a contact never gets one. Waiting is what the agent reports to the
// hub, so an agent can make this fire; each push is the same few bytes as a
// message push, and the per-login, per-agent slot (pushEvery) bounds them.

// pushWaitingEvery is how often the registry is read for agents that began
// waiting.
const pushWaitingEvery = 30 * time.Second

// pushKindWaiting is the waiting push's kind; a message push has none.
const pushKindWaiting = "waiting"

func (p *Push) watchWaiting(ctx context.Context) {
	every := p.waitingEvery
	if every <= 0 {
		every = pushWaitingEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	// seen holds each agent's WaitingSince when it was last pushed or
	// first seen: the first read only learns what already waits, so a proxy
	// restart pushes no stale waits.
	var seen map[string]time.Time
	for {
		seen = p.waitingPass(ctx, seen)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// waitingPass is one read: it pushes for each agent whose WaitingSince is
// new since the last pass, and returns the new seen set.
func (p *Push) waitingPass(ctx context.Context, seen map[string]time.Time) map[string]time.Time {
	next := map[string]time.Time{}
	var fresh []string
	for _, a := range p.g.liveSpawned(ctx) {
		if a.WaitingSince.IsZero() {
			continue
		}
		next[a.Name] = a.WaitingSince
		if prev, ok := seen[a.Name]; seen != nil && (!ok || !prev.Equal(a.WaitingSince)) {
			fresh = append(fresh, a.Name)
		}
	}
	if len(fresh) == 0 {
		return next
	}
	for _, login := range p.store.Logins() {
		v := p.g.viewerFor(ctx, login)
		for _, agent := range fresh {
			if !v.mayMessage(agent) {
				continue
			}
			// A message push for this agent just went out: that one
			// already said "your turn".
			if wait, _ := p.takeSlot(login, agent); wait > 0 {
				continue
			}
			p.notifyKind(context.WithoutCancel(ctx), login, agent, pushKindWaiting)
		}
	}
	return next
}
