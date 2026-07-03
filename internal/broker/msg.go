package broker

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/scion"
)

// msgTarget is a resolved, policy-approved message destination: the scion
// recipient string and the project (-g) it must be sent under. Every
// resolved target is agent-addressed and project-scoped — scion has no
// broker-routable user/operator inbox (see resolveMsgTarget).
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
	// The manager target ALWAYS routes to agent:<slug> — scion knows the
	// manager by its agent slug (the app name; apply dispatches it as
	// Grove: app.Name), NOT by the cert CN used for authn: agent:<CN> fails
	// live with `Agent "<CN>" not found in project`.
	managerTarget := msgTarget{scionTo: "agent:" + b.managerSlug, project: b.managerProject}
	// user:* is a legacy-shaped alias, not a real inbox: scion refuses
	// user-addressed sends from outside an agent container ("SCION_AGENT_NAME
	// not set"), and the broker's runtime scion always runs jail-side. In the
	// broker-routed world "the manager" IS an agent, so the only user:* forms
	// worth honoring are the ones that plainly mean "the manager" — the taught
	// alias `user:manager`, the manager's cert CN, and its scion slug. Anything
	// else is denied rather than silently 502ing at the scion CLI.
	if len(to) > 5 && to[:5] == "user:" {
		who := to[5:]
		if who == "manager" || who == b.manager || who == b.managerSlug {
			return managerTarget, nil
		}
		return msgTarget{}, fmt.Errorf("user-addressed recipient %q is not broker-routable (scion supports user messaging only inside agent containers); message the manager agent instead", to)
	}
	name := to
	if len(to) > 6 && to[:6] == "agent:" {
		name = to[6:]
	}
	if name == b.manager || name == b.managerSlug {
		return managerTarget, nil
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
// its own agent inbox (empty grove — jail-side `scion notifications` requires
// -g; the bare/operator form is container-only) or any declared grove's.
// Grove: its own only.
func (b *Broker) resolveListProject(caller, grove string) (string, error) {
	if caller == b.manager {
		if grove == "" {
			return b.managerProject, nil
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
		// Generic wire body (package convention, see grove.go): the scion CLI
		// error text can echo argv (recipient/message body) — detail stays in
		// the audit log only.
		http.Error(w, "runtime error", http.StatusBadGateway)
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
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "list "+req.Grove)
	writeJSON(w, msgListResponse{Events: events})
}
