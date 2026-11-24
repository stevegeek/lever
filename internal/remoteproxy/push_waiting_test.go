package remoteproxy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitingEnv is a trigger env whose broker registry the test sets: one
// spawned agent, s-abc234, with its WaitingSince.
type waitingEnv struct {
	*triggerEnv
	mu   sync.Mutex
	list []SpawnedAgent
}

func newWaitingEnv(t *testing.T) *waitingEnv {
	t.Helper()
	e := &waitingEnv{triggerEnv: newTriggerEnv(t, nil)}
	e.g.cfg.Spawned = func(context.Context) ([]SpawnedAgent, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return append([]SpawnedAgent(nil), e.list...), nil
	}
	e.p.store.Add(chatOp, sub("op"))
	e.p.store.Add("c@x", sub("contact"))
	return e
}

func (e *waitingEnv) set(agents ...SpawnedAgent) {
	e.mu.Lock()
	e.list = agents
	e.mu.Unlock()
	e.g.spawnedList.mu.Lock()
	e.g.spawnedList.at = time.Time{} // read again
	e.g.spawnedList.mu.Unlock()
}

func (e *waitingEnv) sent() []string {
	e.fs.mu.Lock()
	defer e.fs.mu.Unlock()
	out := append([]string(nil), e.fs.sent...)
	e.fs.sent = nil
	return out
}

func spawnedAt(name string, since time.Time) SpawnedAgent {
	return SpawnedAgent{Name: name, Template: "session", State: SpawnedActive, WaitingSince: since}
}

// The first read only learns what already waits; a new wait pushes once,
// to the operator only, with kind "waiting" and no text; the same wait
// does not push again, and a later wait does.
func TestWaitingPush(t *testing.T) {
	e := newWaitingEnv(t)
	ctx := context.Background()
	e.set(spawnedAt("s-old111", t0.Add(-time.Hour)))
	seen := e.p.waitingPass(ctx, nil)
	if s := e.sent(); len(s) != 0 {
		t.Fatalf("a wait from before the proxy started pushed: %v", s)
	}
	e.set(spawnedAt("s-old111", t0.Add(-time.Hour)), spawnedAt("s-abc234", t0))
	seen = e.p.waitingPass(ctx, seen)
	s := e.sent()
	if len(s) != 1 || !strings.HasSuffix(s[0], `/op {"v":1,"agent":"s-abc234","kind":"waiting"}`) {
		t.Fatalf("sent %v", s)
	}
	seen = e.p.waitingPass(ctx, seen)
	if s := e.sent(); len(s) != 0 {
		t.Fatalf("the same wait pushed again: %v", s)
	}
	// Working (no wait), then a new wait, past the slot.
	e.set(spawnedAt("s-abc234", time.Time{}))
	seen = e.p.waitingPass(ctx, seen)
	e.now = e.now.Add(pushEvery + time.Second)
	e.set(spawnedAt("s-abc234", t0.Add(5*time.Minute)))
	e.p.waitingPass(ctx, seen)
	if s := e.sent(); len(s) != 1 {
		t.Fatalf("a new wait: %v", s)
	}
}

// A message push for the agent a moment ago already said "your turn": the
// waiting push gives way to it.
func TestWaitingPushGivesWayToAMessagePush(t *testing.T) {
	e := newWaitingEnv(t)
	ctx := context.Background()
	seen := e.p.waitingPass(ctx, nil)
	if wait, _ := e.p.takeSlot(chatOp, "s-abc234"); wait != 0 {
		t.Fatal("slot")
	}
	e.set(spawnedAt("s-abc234", t0))
	e.p.waitingPass(ctx, seen)
	if s := e.sent(); len(s) != 0 {
		t.Fatalf("pushed inside the message push's slot: %v", s)
	}
}

// An agent that is not active, or whose template is gone, pushes nothing.
func TestWaitingPushOnlyForLiveAgents(t *testing.T) {
	e := newWaitingEnv(t)
	ctx := context.Background()
	seen := e.p.waitingPass(ctx, nil)
	gone := spawnedAt("s-gone11", t0)
	gone.State = "destroying"
	orphan := spawnedAt("s-orph11", t0)
	orphan.Orphaned = true
	e.set(gone, orphan)
	e.p.waitingPass(ctx, seen)
	if s := e.sent(); len(s) != 0 {
		t.Fatalf("sent %v", s)
	}
}
