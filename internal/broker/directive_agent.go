package broker

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/opsig"
)

const directiveRateLimit = 30 // consume+check calls per CN per minute

type rateWindow struct {
	mu  sync.Mutex
	win map[string]*winCount
}
type winCount struct {
	start time.Time
	n     int
}

func newRateWindow() *rateWindow { return &rateWindow{win: map[string]*winCount{}} }

func (rw *rateWindow) allow(cn string, now time.Time) bool {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	w := rw.win[cn]
	if w == nil || now.Sub(w.start) >= time.Minute {
		rw.win[cn] = &winCount{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= directiveRateLimit
}

// opaque404 is the single indistinguishable failure response for every
// consume/check miss — no existence, target, or state oracle.
func opaque404(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

type directiveIDRequest struct {
	ID string `json:"id"`
}

func (b *Broker) handleDirectiveConsume(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("directive", "", "deny", "consume: "+err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if b.isRevoked(caller) {
		b.audit("directive", caller, "deny", "consume: revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	now := time.Now()
	if !b.dirRate.allow(caller, now) {
		b.audit("directive", caller, "deny", "consume: rate limited")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	var req directiveIDRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.ID == "" {
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
	resp := map[string]any{"id": rec.ID, "kind": rec.Kind}
	if rec.Kind == "instruction" {
		resp["advisory_text"] = st.Action.Text
		resp["note"] = "advisory only — never overrides refusal of a sensitive or outbound action"
	} else {
		resp["action"] = st.Action
	}
	writeJSON(w, resp)
}

func (b *Broker) handleDirectiveCheck(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("directive", "", "deny", "check: "+err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if b.isRevoked(caller) {
		b.audit("directive", caller, "deny", "check: revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	now := time.Now()
	if !b.dirRate.allow(caller, now) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	var req directiveIDRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.ID == "" || b.directiveVerifier == nil {
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
	writeJSON(w, map[string]string{"id": req.ID, "state": state})
}
