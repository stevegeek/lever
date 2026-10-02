package broker

import (
	"errors"
	"net/http"

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
