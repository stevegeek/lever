package broker

import (
	"context"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// The agents loop. Once a minute it reads the hub's records of the spawned
// agents and, for each active one:
//
//  1. notes when it began waiting at its prompt (WaitingSince in the agent
//     list, which the remote proxy turns into a content-free "waiting for
//     you" push to operator logins);
//  2. suspends it once it has waited longer than its template's
//     idle_suspend;
//  3. once an hour asks DailyBackup to back up its conversation if it ran
//     since its last backup (DailyBackup takes one when the newest is a day
//     old).
//
// Waiting is what the agent reports to the hub, so it is only ever a hint:
// the loop's one action is suspend, which keeps the conversation and which
// any resume or wake undoes. It never stops, destroys or starts an agent.

// agentLoopListTimeout bounds the hub read of one pass.
const agentLoopListTimeout = 30 * time.Second

// dailyBackupEvery is how often the loop offers DailyBackup the agents that
// ran; DailyBackup itself decides whether a backup is due.
const dailyBackupEvery = time.Hour

// maxWaitingAge bounds how far back a hub's last-activity time is believed
// for WaitingSince: an older one (a clock fault, or a record an agent wrote)
// is read as "since this pass".
const maxWaitingAge = 7 * 24 * time.Hour

type agentWatch struct {
	mu           sync.Mutex
	waitingSince map[string]time.Time
	// ran: agents seen running since their last daily backup.
	ran        map[string]bool
	lastBackup time.Time
}

func (w *agentWatch) waiting(name string) *time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.waitingSince[name]
	if !ok {
		return nil
	}
	return &t
}

func (b *Broker) agentLoopEnabled() bool {
	return b.runtime != nil && len(b.workers.templateList()) > 0
}

func (b *Broker) runAgentLoop(ctx context.Context) {
	t := time.NewTicker(b.agentLoopEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.agentsPass(ctx)
		}
	}
}

// agentsPass is one pass of the loop.
func (b *Broker) agentsPass(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, agentLoopListTimeout)
	agents, err := b.runtime.List(lctx, b.instanceProject)
	cancel()
	if err != nil {
		return // the next pass reads again; nothing acts on a failed read
	}
	bySlug := make(map[string]scion.Agent, len(agents))
	for _, a := range agents {
		bySlug[a.Slug] = a
	}
	now := b.now()
	var suspend []string
	w := &b.agentWatch
	w.mu.Lock()
	if w.waitingSince == nil {
		w.waitingSince, w.ran = map[string]time.Time{}, map[string]bool{}
	}
	active := map[string]bool{}
	for _, rec := range b.workers.recordList() {
		if rec.State != AgentActive {
			continue
		}
		active[rec.Name] = true
		a, found := bySlug[rec.Name]
		phase := scion.PhaseLabel(a.Phase)
		if !found || scion.RunningContainerDown(phase, a.ContainerStatus) {
			delete(w.waitingSince, rec.Name)
			continue
		}
		if phase == scion.PhaseRunning {
			w.ran[rec.Name] = true
		}
		idle := idleAtPrompt(phase, scion.ActivityLabel(a.Activity))
		if phase != scion.PhaseRunning || idle == nil || !*idle {
			delete(w.waitingSince, rec.Name)
			continue
		}
		since, had := w.waitingSince[rec.Name]
		if !had {
			since = now
			if lae := a.LastActivityEvent; !lae.IsZero() && !lae.After(now) && now.Sub(lae) < maxWaitingAge {
				since = lae.UTC()
			}
			w.waitingSince[rec.Name] = since
		}
		if t, ok := b.workers.templateOfRecord(rec.Name); ok && t.IdleSuspend > 0 && now.Sub(since) >= t.IdleSuspend {
			suspend = append(suspend, rec.Name)
		}
	}
	// Forget agents that are no longer active (destroyed, or a new jail).
	for name := range w.waitingSince {
		if !active[name] {
			delete(w.waitingSince, name)
		}
	}
	for name := range w.ran {
		if !active[name] {
			delete(w.ran, name)
		}
	}
	var backup []string
	if b.dailyBackup != nil && now.Sub(w.lastBackup) >= dailyBackupEvery {
		w.lastBackup = now
		for name := range w.ran {
			backup = append(backup, name)
		}
	}
	w.mu.Unlock()

	for _, name := range suspend {
		b.idleSuspend(ctx, name)
	}
	for _, name := range backup {
		b.dailyBackupOf(ctx, name)
	}
}

// idleSuspend suspends name if, under its lock and on a fresh read, it is
// still an active spawned agent running and waiting at its prompt. A busy
// lock (a verb under way) skips it until the next pass.
func (b *Broker) idleSuspend(ctx context.Context, name string) {
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		return
	}
	defer unlock()
	if rec, ok := b.workers.record(name); !ok || rec.State != AgentActive {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, agentLoopListTimeout)
	agents, err := b.runtime.List(lctx, b.instanceProject)
	cancel()
	if err != nil {
		return
	}
	for _, a := range agents {
		if a.Slug != name {
			continue
		}
		phase := scion.PhaseLabel(a.Phase)
		idle := idleAtPrompt(phase, scion.ActivityLabel(a.Activity))
		if phase != scion.PhaseRunning || scion.RunningContainerDown(phase, a.ContainerStatus) || idle == nil || !*idle {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, agentLoopListTimeout)
		err := b.runtime.Suspend(sctx, name, b.instanceProject)
		cancel()
		if err != nil {
			// scion's text can echo the container env: it stays in the
			// audit log, which is the operator's.
			b.audit("agent", "broker", "error", "idle suspend "+name+": "+err.Error())
			return
		}
		b.agentWatch.mu.Lock()
		delete(b.agentWatch.waitingSince, name)
		b.agentWatch.mu.Unlock()
		b.audit("agent", "broker", "allow", "idle suspend "+name+": waited at its prompt past its template's idle_suspend")
		return
	}
}

// dailyBackupOf asks DailyBackup for name under its lock, so no destroy or
// other verb runs during the capture; a busy lock skips it until the next
// round.
func (b *Broker) dailyBackupOf(ctx context.Context, name string) {
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		return
	}
	defer unlock()
	if rec, ok := b.workers.record(name); !ok || rec.State != AgentActive {
		return
	}
	// A capture under way (a destroy's, or another's) skips this one until
	// the next round: the daily backup never queues.
	if b.captureSem != nil {
		select {
		case b.captureSem <- struct{}{}:
		default:
			return
		}
		defer func() { <-b.captureSem }()
	}
	id, err := b.dailyBackup(ctx, name)
	if err != nil {
		b.audit("agent", "broker", "error", "daily backup of "+name+": "+err.Error())
		return
	}
	if id == "" {
		return // not due yet: it stays marked as having run
	}
	b.agentWatch.mu.Lock()
	delete(b.agentWatch.ran, name)
	b.agentWatch.mu.Unlock()
	b.audit("agent", "broker", "allow", "daily backup of "+name+": "+id)
}

// capture runs one conversation capture when no other runs, waiting for its
// turn as long as ctx allows.
func (b *Broker) capture(ctx context.Context, do func() (string, error)) (string, error) {
	if b.captureSem == nil {
		return do()
	}
	select {
	case b.captureSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.captureSem }()
	return do()
}
