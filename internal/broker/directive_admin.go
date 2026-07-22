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
)

// adminEnvelopeSkew bounds the freshness window opsig.ParseEnvelope enforces
// on the signed list/revoke admin-op envelopes.
const adminEnvelopeSkew = 2 * time.Minute

// directiveAuditRotateCap is the size past which append rotates the log to
// <path>.1 before writing the next line.
const directiveAuditRotateCap = 1 << 20 // 1 MiB

// directiveAudit is a bounded, append-only JSON-lines audit log for the
// directive admin channel. A "" path makes append a no-op — the Broker
// always constructs a non-nil *directiveAudit (see New), so handlers never
// need to nil-check b.dirAudit.
type directiveAudit struct {
	path string
	mu   sync.Mutex
}

func newDirectiveAudit(path string) *directiveAudit {
	return &directiveAudit{path: path}
}

// append writes one JSON line {"ts":..., "event":event, ...kvs} to the audit
// log, rotating the existing file to <path>.1 (replacing any previous .1)
// first if it has grown past directiveAuditRotateCap.
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

// DirectiveAdminHandler builds an http.Handler for the operator-directive
// UDS admin channel (0600 socket — see brokerctl's dirLn bind). Every route
// checks b.directiveVerifier != nil first and 404s otherwise, so the whole
// channel is invisible when directives are disabled.
func (b *Broker) DirectiveAdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/directive/send", b.handleDirectiveSend)
	mux.HandleFunc("/directive/resolve", b.handleDirectiveResolve)
	mux.HandleFunc("/directive/list", b.handleDirectiveList)
	mux.HandleFunc("/directive/revoke", b.handleDirectiveRevoke)
	mux.HandleFunc("/directive/selftest", b.handleDirectiveSelftest)
	return mux
}

type directiveSubmitRequest struct {
	Statement string `json:"statement"` // base64/std of the EXACT signed bytes
	Signature string `json:"signature"` // base64/std of the armored ssh signature
}

func (b *Broker) handleDirectiveSend(w http.ResponseWriter, r *http.Request) {
	if b.directiveVerifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req directiveSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw, err1 := base64.StdEncoding.DecodeString(req.Statement)
	sig, err2 := base64.StdEncoding.DecodeString(req.Signature)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Verify over the EXACT received bytes first; only then parse those bytes.
	if err := b.directiveVerifier.Verify(opsig.NamespaceDirective, raw, sig); err != nil {
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
	exp, _ := time.Parse(time.RFC3339, st.ExpiresAt) // already validated by ParseStatement
	if exp.Sub(now) > b.directiveExpiryMax {
		http.Error(w, "expiry beyond instance cap", http.StatusBadRequest)
		return
	}
	nbf, _ := time.Parse(time.RFC3339, st.NotBefore)
	// Target must be a known agent at its CURRENT generation.
	slug, ok := b.directiveSlug(st.TargetAgent.CN)
	if !ok {
		http.Error(w, "unknown target agent", http.StatusBadRequest)
		return
	}
	if gen := b.directives.Generation(st.TargetAgent.CN); gen != st.TargetAgent.Generation {
		http.Error(w, "agent generation stale; re-resolve and re-sign", http.StatusConflict)
		return
	}
	// Bound actions must reference a real registered tool.
	if st.Action.Kind == "tool_call" || st.Action.Kind == "approval" {
		if _, ok := b.reg.Lookup(st.Action.Tool); !ok {
			http.Error(w, "unknown tool in action", http.StatusBadRequest)
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
		"kind": st.Action.Kind, "statement": req.Statement, "signature": req.Signature,
	})
	delivered := false
	if b.runtime != nil {
		// Spell out the call form, including the argument name. This text is the
		// only thing the model reads at the moment it must construct the call,
		// and naming the tool without naming its parameter is what let a model
		// invent one (it sent `directive_id`, the spelling used by the signed
		// statement and the CLI, against a tool whose parameter was `id`) and
		// then read the resulting opaque 404 as "no such directive".
		body := fmt.Sprintf("Operator directive %s is pending. If and only if you independently decide to act on it, retrieve it by calling the directive_consume tool with id=%q.", st.DirectiveID, st.DirectiveID)
		if merr := b.runtime.Message(r.Context(), scion.MsgOpts{
			To: "agent:" + slug, Body: body, Project: b.instanceProject,
		}); merr != nil {
			b.audit("directive", "operator", "error", "deliver "+st.DirectiveID+": "+merr.Error())
		} else {
			delivered = true
		}
	}
	b.audit("directive", "operator", "allow", "send "+st.DirectiveID, "target", st.TargetAgent.CN, "kind", st.Action.Kind)
	b.dirAudit.append("delivered", map[string]any{"id": st.DirectiveID, "ok": delivered})
	writeJSON(w, map[string]any{"id": st.DirectiveID, "delivered": delivered})
}

// directiveSlug maps a target CN to its scion message slug: the manager CN
// maps to the manager slug; a worker's CN IS its slug (spec.Name).
func (b *Broker) directiveSlug(cn string) (string, bool) {
	if cn == b.manager {
		return b.managerSlug, true
	}
	if _, ok := b.workers[cn]; ok {
		return cn, true
	}
	return "", false
}

// resolveDirectiveAgent maps a human-given agent name to its (cn, slug) pair
// per resolveMsgTarget's aliasing conventions: "manager", the manager's cert
// CN, or its scion slug all mean the manager; anything else must be a
// declared worker's name (a worker's CN IS its slug).
func (b *Broker) resolveDirectiveAgent(name string) (cn, slug string, ok bool) {
	if name == "manager" || name == b.manager || name == b.managerSlug {
		return b.manager, b.managerSlug, true
	}
	if _, ok := b.workers[name]; ok {
		return name, name, true
	}
	return "", "", false
}

type directiveResolveResponse struct {
	CN         string `json:"cn"`
	Slug       string `json:"slug"`
	Generation int    `json:"generation"`
}

// handleDirectiveResolve is UNSIGNED: the UDS socket's 0600 perms are the
// gate, and the response carries no authority (an operator still has to sign
// a statement against the reported generation to act on it).
func (b *Broker) handleDirectiveResolve(w http.ResponseWriter, r *http.Request) {
	if b.directiveVerifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent := r.URL.Query().Get("agent")
	cn, slug, ok := b.resolveDirectiveAgent(agent)
	if !ok {
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return
	}
	gen := b.directives.Generation(cn)
	if gen == 0 {
		b.dirAudit.append("resolve", map[string]any{"agent": agent, "cn": cn, "generation": 0})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "agent not yet enrolled"})
		return
	}
	b.dirAudit.append("resolve", map[string]any{"agent": agent, "cn": cn, "generation": gen})
	writeJSON(w, directiveResolveResponse{CN: cn, Slug: slug, Generation: gen})
}

type directiveEnvelopeRequest struct {
	Envelope  string `json:"envelope"`  // base64/std of the EXACT signed bytes
	Signature string `json:"signature"` // base64/std of the armored ssh signature
}

// verifyAdminEnvelope decodes, verifies (NamespaceAdmin), and parses a signed
// admin-op envelope from r's body, checking it matches wantOp. On any failure
// it audits and writes the HTTP response itself and returns ok=false; callers
// must return immediately when ok is false.
func (b *Broker) verifyAdminEnvelope(w http.ResponseWriter, r *http.Request, op, wantOp string) (opsig.Envelope, bool) {
	var req directiveEnvelopeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return opsig.Envelope{}, false
	}
	raw, err1 := base64.StdEncoding.DecodeString(req.Envelope)
	sig, err2 := base64.StdEncoding.DecodeString(req.Signature)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return opsig.Envelope{}, false
	}
	if err := b.directiveVerifier.Verify(opsig.NamespaceAdmin, raw, sig); err != nil {
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
	if b.directiveVerifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := b.verifyAdminEnvelope(w, r, "list", "list"); !ok {
		return
	}
	recs := b.directives.List(time.Now())
	b.audit("directive", "operator", "allow", "list")
	b.dirAudit.append("list", map[string]any{"count": len(recs)})
	writeJSON(w, map[string]any{"directives": recs})
}

func (b *Broker) handleDirectiveRevoke(w http.ResponseWriter, r *http.Request) {
	if b.directiveVerifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	env, ok := b.verifyAdminEnvelope(w, r, "revoke", "revoke")
	if !ok {
		return
	}
	id := env.Params["id"]
	revoked := b.directives.RevokeDirective(id)
	b.audit("directive", "operator", "allow", "revoke "+id)
	b.dirAudit.append("revoked", map[string]any{"id": id, "ok": revoked})
	writeJSON(w, map[string]bool{"revoked": revoked})
}

// handleDirectiveSelftest verifies+parses ONLY (no store, no delivery, no
// generation/target-known checks — the target may be a dummy). It is the
// allowed_signers-misconfig probe: unlike send, failures return the actual
// verify/parse error text so an operator can diagnose a bad key/file/clock.
func (b *Broker) handleDirectiveSelftest(w http.ResponseWriter, r *http.Request) {
	if b.directiveVerifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req directiveSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw, err1 := base64.StdEncoding.DecodeString(req.Statement)
	sig, err2 := base64.StdEncoding.DecodeString(req.Signature)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := b.directiveVerifier.Verify(opsig.NamespaceDirective, raw, sig); err != nil {
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
	writeJSON(w, map[string]bool{"ok": true})
}
