package broker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/wire"
)

// adminEnvelopeSkew is the freshness window of signed admin-op envelopes.
const adminEnvelopeSkew = 2 * time.Minute

const directiveAuditRotateCap = 1 << 20 // 1 MiB

// directiveAudit is a bounded, append-only JSON-lines audit log for the
// directive admin channel. A "" path makes append a no-op.
type directiveAudit struct {
	path string
	mu   sync.Mutex
}

func newDirectiveAudit(path string) *directiveAudit {
	return &directiveAudit{path: path}
}

func (a *directiveAudit) append(event string, kvs map[string]any) {
	if a.path == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if fi, err := os.Stat(a.path); err == nil && fi.Size() > directiveAuditRotateCap {
		_ = os.Rename(a.path, a.path+".1")
	}
	rec := make(map[string]any, len(kvs)+2)
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["event"] = event
	for k, v := range kvs {
		rec[k] = v
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
}

// DirectiveAdminHandler serves the 0600 directive UDS channel. With
// directives disabled every path and method 404s.
func (b *Broker) DirectiveAdminHandler() http.Handler {
	if b.directiveVerifier == nil {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathDirectiveSend, b.handleDirectiveSend)
	mux.HandleFunc("GET "+wire.PathDirectiveResolve, b.handleDirectiveResolve)
	mux.HandleFunc("POST "+wire.PathDirectiveList, b.handleDirectiveList)
	mux.HandleFunc("POST "+wire.PathDirectiveRevoke, b.handleDirectiveRevoke)
	mux.HandleFunc("POST "+wire.PathDirectiveSelftest, b.handleDirectiveSelftest)
	return mux
}

// decodeSignedPair decodes a signed two-field envelope, writing 400 on
// failure. Verification stays at the call sites: send and selftest report
// verify failures differently (opaque vs. verbatim).
func decodeSignedPair[T any](w http.ResponseWriter, r *http.Request, fields func(T) (msg, sig string)) (raw, sig []byte, ok bool) {
	var req T
	if err := decodeBody(w, r, signedBodyLimit, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, false
	}
	m, s := fields(req)
	raw, err1 := base64.StdEncoding.DecodeString(m)
	sig, err2 := base64.StdEncoding.DecodeString(s)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, false
	}
	return raw, sig, true
}

func decodeSignedStatement(w http.ResponseWriter, r *http.Request) (raw, sig []byte, ok bool) {
	return decodeSignedPair(w, r, func(q wire.DirectiveSubmitRequest) (string, string) { return q.Statement, q.Signature })
}

func (b *Broker) handleDirectiveSend(w http.ResponseWriter, r *http.Request) {
	raw, sig, ok := decodeSignedStatement(w, r)
	if !ok {
		return
	}
	// Verify over the EXACT received bytes first; only then parse those bytes.
	if err := b.directiveVerifier.Verify(r.Context(), opsig.NamespaceDirective, raw, sig); err != nil {
		b.audit("directive", "operator", "deny", "send: bad signature")
		b.dirAudit.append("send_denied", map[string]any{"reason": "signature"})
		http.Error(w, "signature verification failed", http.StatusBadRequest)
		return
	}
	now := time.Now()
	st, err := opsig.ParseStatement(raw, b.instanceID, now)
	if err != nil {
		b.audit("directive", "operator", "deny", "send: "+err.Error())
		http.Error(w, "invalid statement", http.StatusBadRequest)
		return
	}
	exp, _ := time.Parse(time.RFC3339, st.ExpiresAt) // validated by ParseStatement
	if exp.Sub(now) > b.directiveExpiryMax {
		http.Error(w, "expiry beyond instance cap", http.StatusBadRequest)
		return
	}
	nbf, _ := time.Parse(time.RFC3339, st.NotBefore)
	// Target must be a known cert CN (not a slug alias) at its CURRENT
	// generation.
	cn, slug, _, ok := b.identity(st.TargetAgent.CN)
	if !ok || cn != st.TargetAgent.CN {
		http.Error(w, "unknown target agent", http.StatusBadRequest)
		return
	}
	if gen := b.directives.Generation(st.TargetAgent.CN); gen != st.TargetAgent.Generation {
		http.Error(w, "agent generation stale; re-resolve and re-sign", http.StatusConflict)
		return
	}
	// Bound actions must reference a real registered tool.
	if st.Action.Kind == opsig.KindToolCall || st.Action.Kind == opsig.KindApproval {
		if _, ok := b.reg.Lookup(st.Action.Tool); !ok {
			http.Error(w, "unknown tool in action", http.StatusBadRequest)
			return
		}
	}
	// A directive whose notice cannot reach the agent is not stored, or it
	// would only sit "in flight" until expiry. The statement is not spent, so
	// the operator can send it again.
	if b.runtime != nil {
		if refusal := b.notRunningRefusal(r.Context(), operatorActor, slug, cn == b.manager); refusal != "" {
			b.audit("directive", "operator", "deny", "send "+st.DirectiveID+": "+refusal)
			b.dirAudit.append("send_denied", map[string]any{"id": st.DirectiveID, "target": st.TargetAgent.CN, "reason": "target_not_running"})
			http.Error(w, refusal+". The directive was not stored; send it again when the agent is running.", http.StatusConflict)
			return
		}
	}
	rec := DirectiveRecord{
		ID: st.DirectiveID, Statement: raw, Signature: sig,
		TargetCN: st.TargetAgent.CN, TargetGen: st.TargetAgent.Generation,
		Kind: st.Action.Kind, NotBefore: nbf, ExpiresAt: exp,
	}
	if err := b.directives.Submit(rec, now); err != nil {
		b.audit("directive", "operator", "deny", "send: "+err.Error())
		http.Error(w, "directive id already seen", http.StatusConflict)
		return
	}
	b.dirAudit.append("issued", map[string]any{
		"id": st.DirectiveID, "target": st.TargetAgent.CN, "gen": st.TargetAgent.Generation,
		"kind": st.Action.Kind,
		// The exact bytes Verify ran over and the store holds.
		"statement": base64.StdEncoding.EncodeToString(raw),
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	delivered := false
	if b.runtime != nil {
		// Name the argument too: a model told only the tool name invented a
		// parameter and read the opaque 404 as "no such directive". The first
		// line's ref lets message_verify find this notice in the sent ledger,
		// so a copy of the text elsewhere proves nothing.
		text := fmt.Sprintf("Operator directive %s is pending. If and only if you independently decide to act on it, retrieve it by calling the directive_consume tool with id=%q.", st.DirectiveID, st.DirectiveID)
		ref, merr := b.sendRecorded(r.Context(), cn, slug, sentledger.KindDirectiveNotice,
			func(ref string) string { return refLine(directiveNoticeMarker, ref) + "\n" + text },
			scion.MsgOpts{To: "agent:" + slug, Project: b.instanceProject})
		if merr != nil {
			// Revoke rather than leave an active directive the agent was never
			// told about; if the notice landed after all, its id now consumes
			// as the usual opaque not-found.
			revoked, perr := b.directives.RevokeDirective(st.DirectiveID)
			b.audit("directive", "operator", "error", "deliver "+st.DirectiveID+" "+ref+": "+merr.Error(), "revoked", revoked, "persisted", perr == nil)
			b.dirAudit.append("delivered", map[string]any{"id": st.DirectiveID, "ok": false})
			b.dirAudit.append("revoked", map[string]any{"id": st.DirectiveID, "ok": revoked, "reason": "undelivered"})
			if !revoked {
				// No longer active: the notice landed despite the error and
				// the agent consumed it (or re-enrolled). "Send again" would
				// have the operator sign a second authority for one intent.
				state := b.directiveState(st.DirectiveID)
				http.Error(w, "directive "+st.DirectiveID+": the notice was reported as not delivered, but the directive is no longer active (state: "+state+"). "+
					"If the state is consumed, the notice reached the agent and the agent took the directive: do NOT send it again. "+
					"If it is invalidated, the agent re-enrolled meanwhile: send a new directive. "+
					"Check `lever directive list` and the agent's session.", http.StatusBadGateway)
				return
			}
			if perr != nil {
				http.Error(w, "directive "+st.DirectiveID+": the notice could not be delivered to the agent, and the directive was revoked in memory only: "+
					"the broker could not write its state to disk. A broker restart before the directive expires would make it active again. "+
					"Fix the disk (see the broker log), run `lever directive revoke "+st.DirectiveID+"` again, and send a new directive only after that.", http.StatusBadGateway)
				return
			}
			http.Error(w, "directive "+st.DirectiveID+": the notice could not be delivered to the agent, so the directive was revoked and nothing is pending. "+
				"Check the agent with `lever doctor`, then send again.", http.StatusBadGateway)
			return
		}
		delivered = true
	}
	b.audit("directive", "operator", "allow", "send "+st.DirectiveID, "target", st.TargetAgent.CN, "kind", st.Action.Kind)
	b.dirAudit.append("delivered", map[string]any{"id": st.DirectiveID, "ok": delivered})
	writeJSON(w, wire.DirectiveSendResponse{ID: st.DirectiveID, Delivered: delivered})
}

func (b *Broker) directiveState(id string) string {
	for _, rec := range b.directives.List(time.Now()) {
		if rec.ID == id {
			return string(rec.State)
		}
	}
	return "unknown"
}

// handleDirectiveResolve is UNSIGNED: the 0600 socket is the gate, and the
// answer carries no authority (acting on it still needs a signed statement).
func (b *Broker) handleDirectiveResolve(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	name := agent
	if name == "manager" {
		name = b.manager
	}
	cn, slug, _, ok := b.identity(name)
	if !ok {
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return
	}
	gen := b.directives.Generation(cn)
	if gen == 0 {
		b.dirAudit.append("resolve", map[string]any{"agent": agent, "cn": cn, "generation": 0})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Error: "agent not yet enrolled"})
		return
	}
	b.dirAudit.append("resolve", map[string]any{"agent": agent, "cn": cn, "generation": gen})
	writeJSON(w, wire.DirectiveResolveResponse{CN: cn, Slug: slug, Generation: gen})
}

// verifyAdminEnvelope verifies a signed admin-op envelope for wantOp. On
// failure it has written the response; callers must return.
func (b *Broker) verifyAdminEnvelope(w http.ResponseWriter, r *http.Request, op, wantOp string) (opsig.Envelope, bool) {
	raw, sig, ok := decodeSignedPair(w, r, func(q wire.DirectiveEnvelopeRequest) (string, string) { return q.Envelope, q.Signature })
	if !ok {
		return opsig.Envelope{}, false
	}
	if err := b.directiveVerifier.Verify(r.Context(), opsig.NamespaceAdmin, raw, sig); err != nil {
		b.audit("directive", "operator", "deny", op+": bad signature")
		b.dirAudit.append(op+"_denied", map[string]any{"reason": "signature"})
		http.Error(w, "signature verification failed", http.StatusBadRequest)
		return opsig.Envelope{}, false
	}
	env, perr := opsig.ParseEnvelope(raw, b.instanceID, time.Now(), adminEnvelopeSkew)
	if perr != nil {
		b.audit("directive", "operator", "deny", op+": "+perr.Error())
		b.dirAudit.append(op+"_denied", map[string]any{"reason": perr.Error()})
		http.Error(w, "invalid envelope", http.StatusBadRequest)
		return opsig.Envelope{}, false
	}
	if env.Op != wantOp {
		b.audit("directive", "operator", "deny", op+": op mismatch")
		b.dirAudit.append(op+"_denied", map[string]any{"reason": "op mismatch", "op": env.Op})
		http.Error(w, "invalid envelope", http.StatusBadRequest)
		return opsig.Envelope{}, false
	}
	return env, true
}

func (b *Broker) handleDirectiveList(w http.ResponseWriter, r *http.Request) {
	if _, ok := b.verifyAdminEnvelope(w, r, "list", "list"); !ok {
		return
	}
	recs := b.directives.List(time.Now())
	b.audit("directive", "operator", "allow", "list")
	b.dirAudit.append("list", map[string]any{"count": len(recs)})
	writeJSON(w, wire.DirectiveListResponse[DirectiveRecord]{Directives: recs})
}

func (b *Broker) handleDirectiveRevoke(w http.ResponseWriter, r *http.Request) {
	env, ok := b.verifyAdminEnvelope(w, r, "revoke", "revoke")
	if !ok {
		return
	}
	id := env.Params["id"]
	revoked, perr := b.directives.RevokeDirective(id)
	b.audit("directive", "operator", "allow", "revoke "+id, "persisted", perr == nil)
	b.dirAudit.append("revoked", map[string]any{"id": id, "ok": revoked})
	writeJSON(w, wire.DirectiveRevokeResponse{Revoked: revoked, NotPersisted: perr != nil})
}

// handleDirectiveSelftest only verifies and parses (the target may be a
// dummy). Unlike send, it returns the real error text so an operator can
// diagnose a bad key, signers file or clock.
func (b *Broker) handleDirectiveSelftest(w http.ResponseWriter, r *http.Request) {
	raw, sig, ok := decodeSignedStatement(w, r)
	if !ok {
		return
	}
	if err := b.directiveVerifier.Verify(r.Context(), opsig.NamespaceDirective, raw, sig); err != nil {
		b.dirAudit.append("selftest", map[string]any{"ok": false, "reason": "signature: " + err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := opsig.ParseStatement(raw, b.instanceID, time.Now()); err != nil {
		b.dirAudit.append("selftest", map[string]any{"ok": false, "reason": err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.dirAudit.append("selftest", map[string]any{"ok": true})
	writeJSON(w, wire.DirectiveSelftestResponse{OK: true})
}
