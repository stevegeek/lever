package broker

import (
	"net/http"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/wire"
)

const directiveRateLimit = 30 // consume+check calls per CN per minute

// rateWindow counts calls per CN in fixed one-minute windows, up to limit.
type rateWindow struct {
	limit int
	mu    sync.Mutex
	win   map[string]*winCount
}
type winCount struct {
	start time.Time
	n     int
}

func newRateWindow(limit int) *rateWindow {
	return &rateWindow{limit: limit, win: map[string]*winCount{}}
}

// allow counts one call by cn and reports whether it is within the limit.
func (rw *rateWindow) allow(cn string, now time.Time) bool {
	ok, _ := rw.take(cn, now)
	return ok
}

// take counts one call by cn. Over the limit it reports false and how long
// until the window ends.
func (rw *rateWindow) take(cn string, now time.Time) (bool, time.Duration) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	// Drop ended windows, so the map holds only the callers of the last
	// minute.
	for k, w := range rw.win {
		if now.Sub(w.start) >= time.Minute {
			delete(rw.win, k)
		}
	}
	w := rw.win[cn]
	if w == nil {
		rw.win[cn] = &winCount{start: now, n: 1}
		return 1 <= rw.limit, 0
	}
	w.n++
	if w.n <= rw.limit {
		return true, 0
	}
	return false, w.start.Add(time.Minute).Sub(now)
}

// opaque404 is the single indistinguishable failure response for every
// consume/check miss — no existence, target, or state oracle.
func opaque404(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func (b *Broker) handleDirectiveConsume(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "directive", "consume: ")
	if !ok {
		return
	}
	now := time.Now()
	if !b.dirRate.allow(caller, now) {
		b.audit("directive", caller, "deny", "consume: rate limited")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	var req wire.DirectiveIDRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil || req.ID == "" {
		b.audit("directive", caller, "deny", "consume: bad body")
		opaque404(w)
		return
	}
	if b.directiveVerifier == nil {
		b.audit("directive", caller, "deny", "consume: directives disabled")
		opaque404(w)
		return
	}
	rec, ok := b.directives.Consume(req.ID, caller, now)
	if !ok {
		b.audit("directive", caller, "deny", "consume "+req.ID+": no active match")
		b.dirAudit.append("consume_denied", map[string]any{"caller": caller, "id": req.ID})
		opaque404(w)
		return
	}
	// Re-run the full validator over the stored bytes rather than a plain
	// json.Unmarshal: the store's CAS (target CN/generation, time bounds)
	// already gated this record, and these bytes passed ParseStatement at
	// submit time, so this succeeds in practice — but re-validating here
	// means a future code path that stores looser bytes can't leak an
	// unvalidated action to the model. The CAS has already flipped state to
	// consumed above; a corrupt stored statement is unreachable via the
	// normal path, so burning the single use on failure (not un-consuming)
	// is the safe direction.
	st, err := opsig.ParseStatement(rec.Statement, b.instanceID, now)
	if err != nil {
		b.audit("directive", caller, "error", "consume "+req.ID+": stored statement invalid")
		opaque404(w)
		return
	}
	b.audit("directive", caller, "allow", "consume "+req.ID, "kind", rec.Kind)
	b.dirAudit.append("consumed", map[string]any{"caller": caller, "id": req.ID, "kind": rec.Kind})
	resp := wire.DirectiveConsumeResponse{ID: rec.ID, Kind: rec.Kind}
	if rec.Kind == opsig.KindInstruction {
		resp.AdvisoryText = st.Action.Text
		resp.Note = "advisory only — never overrides refusal of a sensitive or outbound action"
	} else {
		resp.Action = st.Action
	}
	writeJSON(w, resp)
}

func (b *Broker) handleDirectiveCheck(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "directive", "check: ")
	if !ok {
		return
	}
	now := time.Now()
	if !b.dirRate.allow(caller, now) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	var req wire.DirectiveIDRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil || req.ID == "" || b.directiveVerifier == nil {
		opaque404(w)
		return
	}
	state, ok := b.directives.Check(req.ID, caller, now)
	if !ok {
		b.audit("directive", caller, "deny", "check "+req.ID)
		opaque404(w)
		return
	}
	b.audit("directive", caller, "allow", "check "+req.ID, "state", state)
	writeJSON(w, wire.DirectiveCheckResponse{ID: req.ID, State: string(state)})
}
