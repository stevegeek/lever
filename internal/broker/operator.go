package broker

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/wire"
)

// handleOperatorNote sends a `lever msg send` note, recorded in the sent
// ledger so the recipient can verify it. Its authority is the host user's
// (the 0600 socket), never a signed directive's.
func (b *Broker) handleOperatorNote(w http.ResponseWriter, r *http.Request) {
	var req wire.OperatorNoteRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("msg", "operator", "deny", "note: bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := req.To
	if name == "manager" {
		name = b.manager
	}
	cn, slug, toManager, ok := b.identity(name)
	if !ok || req.To == "" {
		b.audit("msg", "operator", "deny", "note->"+req.To+": unknown agent")
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return
	}
	if b.runtime == nil {
		b.audit("msg", "operator", "error", "note: runtime not wired")
		http.Error(w, "messaging unavailable", http.StatusBadGateway)
		return
	}
	if refusal, reason, phase := b.notRunningCheck(r.Context(), operatorActor, slug, toManager); refusal != "" {
		b.audit("msg", "operator", "deny", "note->"+cn+": "+refusal)
		refusePhase(w, reason, phase, refusal, http.StatusConflict)
		return
	}
	ref, err := b.sendRecorded(r.Context(), cn, slug, sentledger.KindOperatorNote,
		func(ref string) string { return markedBody(operatorNoteMarker, ref, req.Body) },
		scion.MsgOpts{To: "agent:" + slug, Interrupt: req.Interrupt, Project: b.instanceProject})
	if errors.Is(err, errNotRecorded) {
		http.Error(w, "note not sent: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err != nil {
		b.audit("msg", "operator", "error", "note->"+cn+" "+ref+": "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("msg", "operator", "allow", "note->"+cn, "ref", ref)
	writeJSON(w, wire.OperatorNoteResponse{ID: ref})
}

// ResumeFor is the remote chat page's resume of Worker for a chat login. The
// login is for the audit line only; the socket's file mode is the
// authority. Only Tier "operator" may resume a stopped worker; any other
// value reads as a contact, which resumes only a suspended one.
type ResumeFor struct {
	Worker string `json:"worker"`
	Login  string `json:"login"`
	Tier   string `json:"tier"`
}

// WorkerResumeRequest is the body of POST PathOperatorWorkerResume: Worker
// for the operator's own resume, or For alone for the chat page's narrower
// one. The chat page names its worker inside For, so a broker that does not
// know For finds no worker and refuses, rather than running the operator's
// wider resume for a contact.
type WorkerResumeRequest struct {
	Worker string     `json:"worker,omitempty"`
	For    *ResumeFor `json:"for,omitempty"`
}

// handleOperatorWorkerResume is `lever worker resume NAME`: the manager's
// resume verb (resumeWorker: the worker's lock, the role check, every resume
// check, the wait until live) with the host operator as the actor. It is on
// the 0600 operator socket only, so it grants no manager or worker anything:
// the socket's file permissions are the gate, as for notes.
//
// It resumes any record that is not running, as the manager's verb does:
// suspended, stopped, completed, or error (with a forced resume). A worker
// with no record is refused (start it instead), as is the manager: `lever
// up` owns it. A request For a chat login is wakeFor.
func (b *Broker) handleOperatorWorkerResume(w http.ResponseWriter, r *http.Request) {
	var req WorkerResumeRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("worker", operatorActor, "deny", "resume: bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
		return
	}
	if req.For != nil {
		if req.Worker != "" {
			b.audit("worker", operatorActor, "deny", "resume: both a worker and a resume for a chat login")
			refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
			return
		}
		b.wakeFor(w, r, *req.For)
		return
	}
	spec, ok := b.workerSpec(req.Worker)
	if !ok {
		b.audit("worker", operatorActor, "deny", "resume: not a declared worker")
		refuse(w, ReasonNotAWorker, "not a declared worker (the manager is resumed with `lever up`)", http.StatusForbidden)
		return
	}
	if b.isRevoked(spec.Name) {
		// A resume would stage a revoked identity a fresh ticket.
		b.audit("worker", operatorActor, "deny", "resume "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	b.resumeWorker(r.Context(), w, spec, operatorActor)
}

// wakeFor resumes a worker for the remote chat page, with the same checks as
// the manager's resume verb but narrower: never the manager, only a
// suspended or stopped record, and a stopped one only for the operator's own
// login. Its answers are short words: the proxy shows none of them.
func (b *Broker) wakeFor(w http.ResponseWriter, r *http.Request, who ResumeFor) {
	actor := "remote:" + boundedLogin(who.Login)
	spec, ok := b.workerSpec(who.Worker)
	if !ok {
		b.audit("worker", actor, "deny", "wake: not a declared worker")
		refuse(w, ReasonNotAWorker, "not a worker", http.StatusForbidden)
		return
	}
	if b.isRevoked(spec.Name) {
		// A wake would stage a revoked identity a fresh ticket.
		b.audit("worker", actor, "deny", "wake "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "revoked", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	ctx := r.Context()
	unlock, ok := b.lockWorkerOrRefuse(w, ctx, actor, "wake", spec.Name)
	if !ok {
		return
	}
	defer unlock()
	if b.isRevoked(spec.Name) {
		b.audit("worker", actor, "deny", "wake "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "revoked", http.StatusForbidden)
		return
	}
	if err := b.checkAgentRole(ctx, spec.Name); err != nil {
		b.audit("worker", actor, "deny", "wake "+spec.Name+": "+err.Error())
		refuse(w, ReasonRoleMismatch, "refused", http.StatusConflict)
		return
	}
	// A known label from phaseOf, so safe to audit.
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", actor, "error", "wake "+spec.Name+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == scion.PhaseRunning {
		b.audit("worker", actor, "allow", "wake "+spec.Name+": already running")
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
		return
	}
	if phase == scion.PhaseStopped && who.Tier != "operator" {
		// Stopped is the operator's decision; a contact may not undo it.
		b.audit("worker", actor, "deny", "wake "+spec.Name+": stopped, and the login is not the operator's")
		refusePhase(w, ReasonStoppedByOperator, phase, "stopped by the operator", http.StatusConflict)
		return
	}
	if phase != scion.PhaseSuspended && phase != scion.PhaseStopped {
		b.audit("worker", actor, "deny", "wake "+spec.Name+": not asleep (phase "+cmp.Or(phase, "none")+")")
		reason := ReasonNotAsleep
		if phase == "" {
			reason = ReasonNoRecord
		}
		refusePhase(w, reason, phase, "not asleep", http.StatusConflict)
		return
	}
	b.resumeRecord(ctx, w, spec, phase, actor)
}

// boundedLogin keeps a caller-supplied login to one short, safe audit token.
func boundedLogin(s string) string {
	s = strings.ToValidUTF8(s, "_")
	if len(s) > 120 {
		s = strings.ToValidUTF8(s[:120], "_")
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return '_'
		}
		return r
	}, s)
}
