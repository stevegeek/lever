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

// handleOperatorNote sends a `lever msg send` note: the unsigned sibling of
// handleDirectiveSend on the operator's own 0600 socket (OperatorHandler).
// The note is recorded in the sent ledger as operator-note, so the recipient
// can verify it. The authority is the host user's, the same as `lever
// attach`; it is never a signed directive's.
//
// To follows handleDirectiveResolve's aliasing: "manager", the manager's cert
// CN or its scion slug (the app name) mean the manager; anything else must be
// a declared worker's name.
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
	// The same early answer /msg/send gives, before anything is recorded.
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

// handleOperatorWake resumes a suspended or stopped worker for the remote
// chat page (a login that may message it sent it a message). It runs the
// same checks and the same resume as the manager's resume verb
// (handleWorkerResume): the role guard, the hub's phase, then resumeRecord,
// which stages a fresh ticket, picks the verb by phase and waits until the
// worker is live. Narrower than the verb: only a declared worker (never the
// manager), and only from suspended or stopped (running is a no-op success:
// a resume that won the worker's lock got there first). It is on the 0600 operator
// socket, never on the admin listener the hub's network can reach.
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
	if !b.runtimeReady(w) {
		return
	}
	ctx := r.Context()
	// The same lock as the start route and the resume verb: a manager
	// resuming this worker at the same moment would otherwise read
	// "suspended" too, and the second resume fail on a live record.
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
	// phaseOf passes the hub's phase through scion.PhaseLabel, so it is a
	// known word here and safe to audit.
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", actor, "error", "wake "+spec.Name+": phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == scion.PhaseRunning {
		// Live already (another resume won the lock): what the caller wants.
		b.audit("worker", actor, "allow", "wake "+spec.Name+": already running")
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: scion.PhaseRunning})
		return
	}
	if phase != scion.PhaseSuspended && phase != scion.PhaseStopped {
		b.audit("worker", actor, "deny", "wake "+spec.Name+": not asleep (phase "+cmp.Or(phase, "none")+")")
		http.Error(w, "not asleep", http.StatusConflict)
		return
	}
	b.resumeRecord(ctx, w, spec, phase, actor)
}

// boundedLogin keeps a caller-supplied login to one short audit token.
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
