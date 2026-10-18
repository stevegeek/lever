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

// handleOperatorWake resumes a worker for the remote chat page, with the same
// checks as the manager's resume verb but narrower: never the manager, and a
// stopped worker only for the operator's own login. It is on the 0600
// operator socket, never the admin listener the hub's network can reach.
//
// For host programs it is deprecated in favour of `lever worker resume`
// (PathOperatorWorkerResume); the remote chat page keeps using it. The
// Deprecation header (RFC 9745) says so to a direct caller.
func (b *Broker) handleOperatorWake(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `<`+PathOperatorWorkerResume+`>; rel="successor-version"`)
	var req wire.OperatorWakeRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("worker", "remote", "deny", "wake: bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
		return
	}
	actor := "remote:" + boundedLogin(req.Login)
	spec, ok := b.workerSpec(req.Worker)
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
	if phase == scion.PhaseStopped && req.Tier != "operator" {
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

// handleOperatorWorkerResume is `lever worker resume NAME`: the manager's
// resume verb (resumeWorker: the worker's lock, the role check, every resume
// check, the wait until live) with the host operator as the actor. It is on
// the 0600 operator socket only, so it grants no manager or worker anything:
// the socket's file permissions are the gate, as for notes.
//
// Unlike the remote page's wake it resumes any record that is not running,
// as the manager's verb does: suspended, stopped, completed, or error (with
// a forced resume). A worker with no record is refused (start it instead), as
// is the manager: `lever up` owns it.
func (b *Broker) handleOperatorWorkerResume(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("worker", operatorActor, "deny", "resume: bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
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
