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
	if refusal := b.notRunningRefusal(r.Context(), operatorActor, slug, toManager); refusal != "" {
		b.audit("msg", "operator", "deny", "note->"+cn+": "+refusal)
		http.Error(w, refusal, http.StatusConflict)
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
func (b *Broker) handleOperatorWake(w http.ResponseWriter, r *http.Request) {
	var req wire.OperatorWakeRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("worker", "remote", "deny", "wake: bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	actor := "remote:" + boundedLogin(req.Login)
	spec, ok := b.workerSpec(req.Worker)
	if !ok {
		b.audit("worker", actor, "deny", "wake: not a declared worker")
		http.Error(w, "not a worker", http.StatusForbidden)
		return
	}
	if b.isRevoked(spec.Name) {
		// A wake would stage a revoked identity a fresh ticket.
		b.audit("worker", actor, "deny", "wake "+spec.Name+": revoked")
		http.Error(w, "revoked", http.StatusForbidden)
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
		http.Error(w, "refused", http.StatusConflict)
		return
	}
	// A known label from phaseOf, so safe to audit.
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", actor, "error", "wake "+spec.Name+": phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
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
		http.Error(w, "stopped by the operator", http.StatusConflict)
		return
	}
	if phase != scion.PhaseSuspended && phase != scion.PhaseStopped {
		b.audit("worker", actor, "deny", "wake "+spec.Name+": not asleep (phase "+cmp.Or(phase, "none")+")")
		http.Error(w, "not asleep", http.StatusConflict)
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
