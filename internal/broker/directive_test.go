package broker

import (
	"sync"
	"testing"
	"time"
)

func rec(id, cn string, gen int, now time.Time) DirectiveRecord {
	return DirectiveRecord{
		ID: id, State: "active", Statement: []byte("{}"), TargetCN: cn, TargetGen: gen,
		Kind: "instruction", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute),
	}
}

func newStore(persist func(DirectiveState) error) *DirectiveStore {
	return newDirectiveStore(DirectiveState{}, persist, nil)
}

func TestConsumeCASHappyPathAndReplay(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr") // generation 1
	if err := s.Submit(rec("d1", "mgr", 1, now), now); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Consume("d1", "mgr", now)
	if !ok || got.Kind != "instruction" {
		t.Fatalf("consume failed: %v %v", got, ok)
	}
	if _, ok := s.Consume("d1", "mgr", now); ok {
		t.Fatal("double consume succeeded")
	}
}

func TestConsumeDeniesWrongCallerWrongGenExpiredRevoked(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	if _, ok := s.Consume("d1", "worker-a", now); ok {
		t.Fatal("cross-agent consume succeeded")
	}
	// Re-enrolment bumps generation -> old directive invalidated.
	s.BumpGeneration("mgr")
	if _, ok := s.Consume("d1", "mgr", now); ok {
		t.Fatal("stale-generation consume succeeded")
	}
	// Expired.
	_ = s.Submit(rec("d2", "mgr", 2, now), now)
	if _, ok := s.Consume("d2", "mgr", now.Add(11*time.Minute)); ok {
		t.Fatal("expired consume succeeded")
	}
	// Revoked.
	_ = s.Submit(rec("d3", "mgr", 2, now), now)
	if !s.RevokeDirective("d3") {
		t.Fatal("revoke failed")
	}
	if _, ok := s.Consume("d3", "mgr", now); ok {
		t.Fatal("revoked consume succeeded")
	}
}

func TestConsumeConcurrentSingleWinner(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	var wg sync.WaitGroup
	wins := make(chan struct{}, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.Consume("d1", "mgr", now); ok {
				wins <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for range wins {
		n++
	}
	if n != 1 {
		t.Fatalf("want exactly 1 winning consume, got %d", n)
	}
}

func TestSubmitRejectsDuplicateIDEvenAfterConsume(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	_, _ = s.Consume("d1", "mgr", now)
	if err := s.Submit(rec("d1", "mgr", 1, now), now); err == nil {
		t.Fatal("replayed id accepted after consume (tombstone missing)")
	}
}

func TestTombstonePruneOnlyPastMargin(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	_, _ = s.Consume("d1", "mgr", now)
	// Within margin: still a tombstone.
	if err := s.Submit(rec("d1", "mgr", 1, now.Add(20*time.Hour)), now.Add(20*time.Hour)); err == nil {
		t.Fatal("tombstone pruned inside margin")
	}
	// Past ExpiresAt+48h: pruned, but a replayed statement would fail expiry
	// validation upstream anyway; here Submit succeeds at store level.
	later := now.Add(10*time.Minute + 49*time.Hour)
	if err := s.Submit(rec("d1", "mgr", 1, later), later); err != nil {
		t.Fatalf("prune past margin did not happen: %v", err)
	}
}

func TestPersistCalledOnEveryMutationAndRoundTrips(t *testing.T) {
	now := time.Now()
	var saved DirectiveState
	calls := 0
	persist := func(st DirectiveState) error { saved = st; calls++; return nil }
	s := newStore(persist)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	_, _ = s.Consume("d1", "mgr", now)
	if calls != 3 {
		t.Fatalf("want 3 persist calls (bump, submit, consume), got %d", calls)
	}
	// Reload from the persisted snapshot: consumed state and generation survive.
	s2 := newDirectiveStore(saved, nil, nil)
	if s2.Generation("mgr") != 1 {
		t.Fatal("generation lost across reload")
	}
	if _, ok := s2.Consume("d1", "mgr", now); ok {
		t.Fatal("consumed directive re-consumable after reload (B2 violation)")
	}
	if err := s2.Submit(rec("d1", "mgr", 1, now), now); err == nil {
		t.Fatal("tombstone lost across reload (B2 violation)")
	}
}

func TestListReportsExpiredAndOmitsStatementBytes(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	_ = s.Submit(rec("d1", "mgr", 1, now), now)
	l := s.List(now.Add(11 * time.Minute))
	if len(l) != 1 || l[0].State != "expired" || l[0].Statement != nil {
		t.Fatalf("bad list: %+v", l)
	}
}
