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

// DirectiveStatus is a directive record's lifecycle state. The persisted and
// wire form is the bare lowercase word.
type DirectiveStatus string

const (
	DirectiveActive      DirectiveStatus = "active"
	DirectiveConsumed    DirectiveStatus = "consumed"
	DirectiveRevoked     DirectiveStatus = "revoked"
	DirectiveInvalidated DirectiveStatus = "invalidated"
	// DirectiveExpired is never stored: List/Check report an active record
	// past ExpiresAt as expired (effectiveStatus).
	DirectiveExpired DirectiveStatus = "expired"
)

// DirectiveRecord is one operator directive in host-side persistent state.
// Statement holds the EXACT signed bytes; nothing acted-on lives outside it.
type DirectiveRecord struct {
	ID         string          `json:"id"`
	State      DirectiveStatus `json:"state"`
	Statement  []byte          `json:"statement,omitempty"`
	Signature  []byte          `json:"signature,omitempty"`
	TargetCN   string          `json:"target_cn"`
	TargetGen  int             `json:"target_gen"`
	Kind       string          `json:"kind"`
	NotBefore  time.Time       `json:"not_before"`
	ExpiresAt  time.Time       `json:"expires_at"`
	ConsumedAt time.Time       `json:"consumed_at,omitzero"`
	// Previews counts the target's successful non-consuming reads (Preview).
	// It is the only thing a preview changes; it never affects State.
	Previews int `json:"previews,omitempty"`
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

// persistLocked snapshots and writes through, returning any write-through
// error. Caller holds s.mu. Callers that hand out a durability-sensitive
// outcome (Submit, Consume) MUST roll back their in-memory mutation and
// propagate this error rather than report success on an un-persisted state —
// see the fail-closed rationale on each caller. Always logged regardless.
func (s *DirectiveStore) persistLocked() error {
	if s.persist == nil {
		return nil
	}
	err := s.persist(s.snapshotLocked())
	if err != nil {
		s.log.Error("directive.persist", "err", err.Error())
	}
	return err
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
	rec.State = DirectiveActive
	cp := rec
	s.recs = append(s.recs, &cp)
	if err := s.persistLocked(); err != nil {
		// Fail closed: an un-persisted submission must not exist in memory
		// either, or a restart before the next successful persist would
		// silently drop it while callers believe it was accepted.
		s.recs = s.recs[:len(s.recs)-1]
		return fmt.Errorf("persist directive %q: %w", rec.ID, err)
	}
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
	if r == nil || r.State != DirectiveActive ||
		r.TargetCN != callerCN || r.TargetGen != s.gens[callerCN] ||
		now.Before(r.NotBefore) || !now.Before(r.ExpiresAt) {
		return DirectiveRecord{}, false
	}
	r.State = DirectiveConsumed
	r.ConsumedAt = now
	if err := s.persistLocked(); err != nil {
		// Fail closed: never hand out an action whose consumed-ness isn't
		// durable — a restart before the next successful persist would
		// replay it. Roll back to active so the operator can re-send and a
		// later retry can still succeed.
		r.State = DirectiveActive
		r.ConsumedAt = time.Time{}
		return DirectiveRecord{}, false
	}
	return *r, true
}

// PreviewOutcome is the result of DirectiveStore.Preview.
type PreviewOutcome int

const (
	// PreviewMiss covers every case Consume would refuse (and a persist
	// failure). Callers must answer it with the same opaque error as a
	// consume miss.
	PreviewMiss PreviewOutcome = iota
	// PreviewOK: the record is returned and its preview count went up by one.
	PreviewOK
	// PreviewCapped: the caller IS the target of an active, in-window
	// directive, but it used all its previews. Nothing is returned or counted.
	PreviewCapped
)

// Preview is the non-consuming read of a pending directive (#17). Its gate is
// EXACTLY Consume's — active, target CN, current generation, inside the time
// window — so a preview can never show a directive that a consume at the same
// instant would refuse, and it is no wider an oracle than consume is. It does
// not flip State: the single-use compare-and-swap stays with Consume alone,
// and a previewed directive is still consumable exactly once. The one mutation
// is the persisted Previews count, capped at limit so the route cannot be
// polled without bound.
func (s *DirectiveStore) Preview(id, callerCN string, now time.Time, limit int) (DirectiveRecord, PreviewOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.State != DirectiveActive ||
		r.TargetCN != callerCN || r.TargetGen != s.gens[callerCN] ||
		now.Before(r.NotBefore) || !now.Before(r.ExpiresAt) {
		return DirectiveRecord{}, PreviewMiss
	}
	if r.Previews >= limit {
		return DirectiveRecord{}, PreviewCapped
	}
	r.Previews++
	if err := s.persistLocked(); err != nil {
		// Fail closed, like Consume: content must not leave the broker on a
		// count that is not durable, or a restart would reset the cap.
		r.Previews--
		return DirectiveRecord{}, PreviewMiss
	}
	return *r, PreviewOK
}

// Check reports the directive's state, but ONLY to its target at the current
// generation — everyone else gets the same ("", false) as a missing id.
func (s *DirectiveStore) Check(id, callerCN string, now time.Time) (DirectiveStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.TargetCN != callerCN || r.TargetGen != s.gens[callerCN] {
		return "", false
	}
	return effectiveStatus(r, now), true
}

// RevokeDirective marks an active directive revoked (tombstone retained).
// persistErr is non-nil when the revocation could not be written to disk: it
// then holds only in memory, and a broker restart inside the directive's
// lifetime would bring the directive back active. The caller must say so.
func (s *DirectiveStore) RevokeDirective(id string) (revoked bool, persistErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findLocked(id)
	if r == nil || r.State != DirectiveActive {
		return false, nil
	}
	r.State = DirectiveRevoked
	// Apply in memory regardless of persist outcome: refusing to revoke on a
	// disk error would fail OPEN — an operator trying to invalidate a
	// directive must not be told "still active" because the write-through
	// failed.
	return true, s.persistLocked()
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
		cp.State = effectiveStatus(r, now)
		out = append(out, cp)
	}
	return out
}

func effectiveStatus(r *DirectiveRecord, now time.Time) DirectiveStatus {
	if r.State == DirectiveActive && !now.Before(r.ExpiresAt) {
		return DirectiveExpired
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
		if r.TargetCN == cn && r.State == DirectiveActive {
			r.State = DirectiveInvalidated
		}
	}
	// Apply in memory regardless of persist outcome (error already logged by
	// persistLocked): refusing to bump/invalidate on a disk error would fail
	// OPEN — a recycled slug re-enrolling must not retain a predecessor's
	// still-active directive because the write-through failed.
	_ = s.persistLocked()
}

// Generation returns cn's current enrolment generation (0 = never enrolled).
func (s *DirectiveStore) Generation(cn string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gens[cn]
}

// EnsureGeneration establishes cn's generation at 1 if it has none yet (0),
// WITHOUT bumping an existing one. Called on /renew: an agent that restarts
// with a persisted cert (or whose cert predates this feature) refreshes via
// /renew and never re-hits /enrol, so without this its generation stays 0 and
// no operator directive can ever target it. Bumping is reserved for genuine
// re-enrolment (BumpGeneration) — renew is the same identity at the same
// enrolment epoch, so bumping here would invalidate the agent's own active
// directives on every 12h refresh.
func (s *DirectiveStore) EnsureGeneration(cn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gens[cn] != 0 {
		return
	}
	s.gens[cn] = 1
	// Apply in memory regardless of persist outcome (error already logged): a
	// transient disk error must not leave the agent unable to receive directives.
	_ = s.persistLocked()
}
