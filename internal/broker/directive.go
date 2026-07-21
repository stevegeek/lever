package broker

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// tombstoneMargin: consumed/expired/revoked records are retained as replay
// tombstones until this long PAST their expiry — never pruned on any other
// clock condition, so a replay window cannot reopen under skew.
const tombstoneMargin = 48 * time.Hour

// DirectiveRecord is one operator directive in host-side persistent state.
// Statement holds the EXACT signed bytes; nothing acted-on lives outside it.
type DirectiveRecord struct {
	ID         string    `json:"id"`
	State      string    `json:"state"` // active | consumed | revoked | invalidated
	Statement  []byte    `json:"statement,omitempty"`
	Signature  []byte    `json:"signature,omitempty"`
	TargetCN   string    `json:"target_cn"`
	TargetGen  int       `json:"target_gen"`
	Kind       string    `json:"kind"`
	NotBefore  time.Time `json:"not_before"`
	ExpiresAt  time.Time `json:"expires_at"`
	ConsumedAt time.Time `json:"consumed_at,omitzero"`
}

// DirectiveState is the persisted directive store snapshot: per-CN enrolment
// generations plus all live directives and replay tombstones.
type DirectiveState struct {
	Generations map[string]int     `json:"generations"`
	Directives  []*DirectiveRecord `json:"directives"`
}

// DirectiveStore owns directive state under one mutex; every mutation is
// written through to persist (the directives.json hook) before returning,
// mirroring the broker's revocation persistence.
type DirectiveStore struct {
	mu      sync.Mutex
	gens    map[string]int
	recs    []*DirectiveRecord
	persist func(DirectiveState) error
	log     *slog.Logger
}

func newDirectiveStore(st DirectiveState, persist func(DirectiveState) error, log *slog.Logger) *DirectiveStore {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	gens := make(map[string]int, len(st.Generations))
	for k, v := range st.Generations {
		gens[k] = v
	}
	recs := make([]*DirectiveRecord, 0, len(st.Directives))
	for _, r := range st.Directives {
		cp := *r
		recs = append(recs, &cp)
	}
	return &DirectiveStore{gens: gens, recs: recs, persist: persist, log: log}
}

// persistLocked snapshots and writes through. Caller holds s.mu.
func (s *DirectiveStore) persistLocked() {
	if s.persist == nil {
		return
	}
	if err := s.persist(s.snapshotLocked()); err != nil {
		s.log.Error("directive.persist", "err", err.Error())
	}
}

func (s *DirectiveStore) snapshotLocked() DirectiveState {
	gens := make(map[string]int, len(s.gens))
	for k, v := range s.gens {
		gens[k] = v
	}
	recs := make([]*DirectiveRecord, 0, len(s.recs))
	for _, r := range s.recs {
		cp := *r
		recs = append(recs, &cp)
	}
	return DirectiveState{Generations: gens, Directives: recs}
}

// pruneLocked drops records past ExpiresAt+tombstoneMargin. Caller holds s.mu.
func (s *DirectiveStore) pruneLocked(now time.Time) {
	kept := s.recs[:0]
	for _, r := range s.recs {
		if now.After(r.ExpiresAt.Add(tombstoneMargin)) {
			continue
		}
		kept = append(kept, r)
	}
	s.recs = kept
}

func (s *DirectiveStore) findLocked(id string) *DirectiveRecord {
	for _, r := range s.recs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Submit stores a verified directive as active. The id must be unseen across
// ALL records including tombstones (replay defence).
func (s *DirectiveStore) Submit(rec DirectiveRecord, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if s.findLocked(rec.ID) != nil {
		return fmt.Errorf("directive %q already seen", rec.ID)
	}
	rec.State = "active"
	cp := rec
	s.recs = append(s.recs, &cp)
	s.persistLocked()
	return nil
}

// Consume is the atomic compare-and-swap: exactly one caller can flip an
// active, in-window directive targeted at (callerCN, current generation) to
// consumed. EVERY failure mode returns (zero, false) — callers must emit an
// identical opaque error for all of them; detail goes to the audit log only.
func (s *DirectiveStore) Consume(id, callerCN string, now time.Time) (DirectiveRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.State != "active" ||
		r.TargetCN != callerCN || r.TargetGen != s.gens[callerCN] ||
		now.Before(r.NotBefore) || !now.Before(r.ExpiresAt) {
		return DirectiveRecord{}, false
	}
	r.State = "consumed"
	r.ConsumedAt = now
	s.persistLocked()
	return *r, true
}

// Check reports the directive's state, but ONLY to its target at the current
// generation — everyone else gets the same ("", false) as a missing id.
func (s *DirectiveStore) Check(id, callerCN string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.TargetCN != callerCN || r.TargetGen != s.gens[callerCN] {
		return "", false
	}
	return effectiveState(r, now), true
}

// RevokeDirective marks an active directive revoked (tombstone retained).
func (s *DirectiveStore) RevokeDirective(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.State != "active" {
		return false
	}
	r.State = "revoked"
	s.persistLocked()
	return true
}

// List returns operator-facing copies with the statement/signature bytes
// omitted and active-but-expired reported as "expired".
func (s *DirectiveStore) List(now time.Time) []DirectiveRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DirectiveRecord, 0, len(s.recs))
	for _, r := range s.recs {
		cp := *r
		cp.Statement, cp.Signature = nil, nil
		cp.State = effectiveState(r, now)
		out = append(out, cp)
	}
	return out
}

func effectiveState(r *DirectiveRecord, now time.Time) string {
	if r.State == "active" && !now.Before(r.ExpiresAt) {
		return "expired"
	}
	return r.State
}

// BumpGeneration advances cn's enrolment generation (called on every
// successful /enrol) and invalidates cn's still-active directives — a
// recycled slug can never receive a predecessor's directive.
func (s *DirectiveStore) BumpGeneration(cn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gens[cn]++
	for _, r := range s.recs {
		if r.TargetCN == cn && r.State == "active" {
			r.State = "invalidated"
		}
	}
	s.persistLocked()
}

// Generation returns cn's current enrolment generation (0 = never enrolled).
func (s *DirectiveStore) Generation(cn string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gens[cn]
}
