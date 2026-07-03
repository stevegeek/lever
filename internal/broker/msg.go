package broker

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/scion"
)

// msgTarget is a resolved, policy-approved message destination: the scion
// recipient string and the project (-g) it must be sent under ("" = the
// operator/user inbox, not project-scoped).
type msgTarget struct {
	scionTo string
	project string
}

// resolveMsgTarget applies the messaging policy and resolves `to` for caller.
// Identity-derived, config-authoritative, default-deny. The returned error
// text is the deny reason (audited alongside the recipient by the handler).
func (b *Broker) resolveMsgTarget(caller, to string) (msgTarget, error) {
	isManager := caller == b.manager
	_, isGrove := b.groves[caller]
	if !isManager && !isGrove {
		return msgTarget{}, fmt.Errorf("caller %q is not the manager or a declared grove", caller)
	}
	// user:* recipients are not project-scoped; any authenticated agent may
	// message the user/operator inbox (that is how groves report to the manager).
	if len(to) > 5 && to[:5] == "user:" {
		return msgTarget{scionTo: to, project: ""}, nil
	}
	name := to
	if len(to) > 6 && to[:6] == "agent:" {
		name = to[6:]
	}
	if name == b.manager {
		return msgTarget{scionTo: "agent:" + name, project: b.managerProject}, nil
	}
	spec, ok := b.groves[name]
	if !ok {
		return msgTarget{}, fmt.Errorf("unknown recipient %q", to)
	}
	if !isManager && caller != name && !b.groveToGrove {
		return msgTarget{}, fmt.Errorf("grove→grove messaging is disabled")
	}
	return msgTarget{scionTo: "agent:" + spec.Name, project: spec.JailProject}, nil
}

// resolveListProject resolves which project inbox caller may read. Manager:
// its own/operator inbox ("" ) or any declared grove's. Grove: its own only.
func (b *Broker) resolveListProject(caller, grove string) (string, error) {
	if caller == b.manager {
		if grove == "" {
			return "", nil
		}
		spec, ok := b.groves[grove]
		if !ok {
			return "", fmt.Errorf("unknown grove %q", grove)
		}
		return spec.JailProject, nil
	}
	spec, ok := b.groves[caller]
	if !ok {
		return "", fmt.Errorf("caller %q is not the manager or a declared grove", caller)
	}
	if grove != "" {
		return "", fmt.Errorf("a grove may only read its own inbox")
	}
	return spec.JailProject, nil
}

type msgSendRequest struct {
	To        string `json:"to"`
	Body      string `json:"body"`
	Interrupt bool   `json:"interrupt"`
}

type msgListRequest struct {
	All   bool   `json:"all"`
	Grove string `json:"grove"`
}

type msgListResponse struct {
	Events []scion.Event `json:"events"`
}

func (b *Broker) handleMsgSend(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("msg", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req msgSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("msg", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tgt, rerr := b.resolveMsgTarget(caller, req.To)
	if rerr != nil {
		b.audit("msg", caller, "deny", "send->"+req.To+": "+rerr.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	if err := b.runtime.Message(r.Context(), scion.MsgOpts{
		To: tgt.scionTo, Body: req.Body, Interrupt: req.Interrupt, Project: tgt.project,
	}); err != nil {
		b.audit("msg", caller, "error", "send->"+req.To+": "+err.Error())
		http.Error(w, "runtime error: "+err.Error(), http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "send->"+tgt.scionTo)
	writeJSON(w, map[string]bool{"ok": true})
}

func (b *Broker) handleMsgList(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("msg", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req msgListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("msg", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	project, rerr := b.resolveListProject(caller, req.Grove)
	if rerr != nil {
		b.audit("msg", caller, "deny", "list "+req.Grove+": "+rerr.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	events, err := b.runtime.Inbox(r.Context(), !req.All, project)
	if err != nil {
		b.audit("msg", caller, "error", "list: "+err.Error())
		http.Error(w, "runtime error: "+err.Error(), http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "list "+req.Grove)
	writeJSON(w, msgListResponse{Events: events})
}
