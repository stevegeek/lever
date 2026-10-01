package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/wire"
)

// errUnknownRecipient is the deny for a `to` that names no identity the broker
// knows. It is wrapped as "unknown recipient %q".
var errUnknownRecipient = errors.New("unknown recipient")

// msgTarget is a resolved, policy-approved message destination: the scion
// recipient string and the project (-g) it must be sent under. Every
// resolved target is agent-addressed and project-scoped — scion has no
// broker-routable user/operator inbox (see resolveMsgTarget).
type msgTarget struct {
	scionTo string
	project string
	// recipientCN is the recipient's cert CN: whom the sent ledger records
	// the message for, and who alone can verify it.
	recipientCN string
	// relayFrom is the sending worker's slug when the caller is a worker, and
	// "" when it is the manager. A non-empty value makes handleMsgSend mark the
	// body (see relayedWorkerBody).
	relayFrom string
}

// Lever's own message markers. The broker sends every message through the
// host-side scion CLI with the controller PAT, so the recipient's envelope
// names the controller's hub user as the sender (`from: user:<controller>`)
// and carries a conversation, whoever wrote the text.
//
// The markers are readable hints, never evidence: anyone whose text reaches
// an agent can type one. Who wrote a message is decided by the sent ledger
// (sent.go): the first line also carries the send's ref, and the recipient's
// message_verify call gets the recorded kind and text back from the host
// record. The broker still writes the marker first, and still rewrites
// marker-like text in a worker's or the manager's body, so a session stays
// readable.
const (
	// relayMarkerFormat starts the first line of every body a worker sends.
	relayMarkerFormat = "[lever: relayed from worker %s]"
	// directiveNoticeMarker starts the first line of a directive notice.
	directiveNoticeMarker = "[lever: operator directive notice]"
	// managerMarker starts the first line of every body the manager sends.
	managerMarker = "[lever: from the manager]"
	// operatorNoteMarker starts the first line of every `lever msg send`
	// note (handleOperatorNote).
	operatorNoteMarker = "[lever: operator note]"
)

// markerLike matches anything a reader could take for a lever marker or for
// a scion envelope delimiter: "[lever:" with any case and spacing (fullwidth
// and white square brackets too), and the BEGIN/END SCION MESSAGE lines. It
// keeps a session readable; it is not a boundary (nothing is trusted for
// looking like a marker).
var markerLike = regexp.MustCompile(`(?i)[\[［⟦〚][\s\p{Cf}]*lever[\s\p{Cf}]*[:：]|-{3}[\s\p{Cf}]*(begin|end)[\s\p{Cf}]+scion[\s\p{Cf}]+message[\s\p{Cf}]*-{3}`)

// neutraliseMarkers rewrites every marker-like sequence in text a worker or
// the manager wrote, so the text does not read as a second lever marker or a
// second envelope. The text stays readable.
func neutraliseMarkers(body string) string {
	return markerLike.ReplaceAllStringFunc(body, func(m string) string {
		if strings.HasPrefix(m, "-") {
			return "(quoted scion delimiter)"
		}
		return "(quoted lever marker:"
	})
}

// markedBody is the body the broker sends for text someone else wrote: the
// marker and the send's ref on the first line (refLine), then the text with
// every marker-like sequence neutralised.
func markedBody(marker, ref, text string) string {
	return refLine(marker, ref) + "\n" + neutraliseMarkers(text)
}

// relayedWorkerBody is the body the broker sends for a worker: the relay
// marker naming the worker (a slug from the operator's config, never from
// the request), the ref, then the worker's own text.
func relayedWorkerBody(slug, ref, body string) string {
	return markedBody(fmt.Sprintf(relayMarkerFormat, slug), ref, body)
}

// resolveMsgTarget applies the messaging policy and resolves `to` for caller.
// Identity-derived, config-authoritative, default-deny. The returned error
// text is the deny reason (audited alongside the recipient by the handler).
func (b *Broker) resolveMsgTarget(caller, to string) (msgTarget, error) {
	// The caller is a cert CN: a slug alias is not an identity.
	callerCN, callerSlug, isManager, ok := b.identity(caller)
	if !ok || callerCN != caller {
		return msgTarget{}, fmt.Errorf("caller %q is not the manager or a declared worker", caller)
	}
	// The manager target ALWAYS routes to agent:<slug> — scion knows the
	// manager by its agent slug (the app name; apply dispatches it as
	// Worker: app.Name), NOT by the cert CN used for authn: agent:<CN> fails
	// live with `Agent "<CN>" not found in project`.
	relayFrom := ""
	if !isManager {
		relayFrom = callerSlug
	}
	managerTarget := msgTarget{scionTo: "agent:" + b.managerSlug, project: b.instanceProject, relayFrom: relayFrom, recipientCN: b.manager}
	// user:* is a legacy-shaped alias, not a real inbox: scion refuses
	// user-addressed sends from outside an agent container ("SCION_AGENT_NAME
	// not set"), and the broker's runtime scion always runs jail-side. In the
	// broker-routed world "the manager" IS an agent, so the only user:* forms
	// worth honoring are the ones that plainly mean "the manager" — the taught
	// alias `user:manager`, the manager's cert CN, and its scion slug. Anything
	// else is denied rather than silently 502ing at the scion CLI.
	// who != "" preserves the bare-"user:" fallthrough to the
	// unknown-recipient deny below.
	if who, ok := strings.CutPrefix(to, "user:"); ok && who != "" {
		if _, _, toManager, known := b.identity(who); who == "manager" || (known && toManager) {
			return managerTarget, nil
		}
		return msgTarget{}, fmt.Errorf("user-addressed recipient %q is not broker-routable (scion supports user messaging only inside agent containers); message the manager agent instead", to)
	}
	name := to
	if rest, ok := strings.CutPrefix(to, "agent:"); ok && rest != "" {
		name = rest
	}
	cn, slug, toManager, known := b.identity(name)
	switch {
	case !known:
		return msgTarget{}, fmt.Errorf("%w %q", errUnknownRecipient, to)
	case toManager:
		return managerTarget, nil
	case !isManager && caller != name && !b.workerToWorker:
		return msgTarget{}, fmt.Errorf("worker→worker messaging is disabled")
	}
	return msgTarget{scionTo: "agent:" + slug, project: b.instanceProject, relayFrom: relayFrom, recipientCN: cn}, nil
}

// resolveListSubject resolves WHOSE inbox caller may read, as an agent slug.
// Manager: its own (empty worker) or any declared worker's. Worker: its own
// only.
//
// It returns a slug rather than a project because the project does not scope
// anything. `scion notifications` is scoped by the hub to the authenticated
// USER, and lever authenticates as the host controller PAT, so the same
// fleet-wide feed comes back whatever -g says. handleMsgList turns this slug
// into the agent id it filters on.
func (b *Broker) resolveListSubject(caller, worker string) (string, error) {
	if caller == b.manager {
		if worker == "" {
			// The manager's agent slug, NOT its cert CN: the hub knows it only
			// by the slug (see IdentityConfig.ManagerSlug).
			return b.managerSlug, nil
		}
		if _, ok := b.workerSpec(worker); !ok {
			return "", fmt.Errorf("unknown worker %q", worker)
		}
		return worker, nil
	}
	if _, ok := b.workerSpec(caller); !ok {
		return "", fmt.Errorf("caller %q is not the manager or a declared worker", caller)
	}
	if worker != "" {
		return "", fmt.Errorf("a worker may only read its own inbox")
	}
	return caller, nil
}

// errText renders err for an audit line, naming the empty-id case that is not
// an error but is still a refusal.
func errText(err error) string {
	if err == nil {
		return "hub returned no id for it"
	}
	return err.Error()
}

// eventsForAgent keeps only the events the hub attributes to agentID. An event
// carrying no agentId is DROPPED: attribution is the whole basis of the cut, so
// something lever cannot attribute cannot be shown to anyone.
func eventsForAgent(events []scion.Event, agentID string) []scion.Event {
	kept := make([]scion.Event, 0, len(events))
	for _, e := range events {
		if id, _ := e["agentId"].(string); id != "" && id == agentID {
			kept = append(kept, e)
		}
	}
	return kept
}

// notRunningRefusal reads the recipient agent's phase and returns the refusal
// for one that cannot take a message now, or "" to send. The hub refuses a
// message to an agent in any phase but running (409), scion's interim
// "resumed" included; without this check that reached the caller as a bare
// "runtime error", after the sent ledger had recorded the send. The broker
// does not queue the message and does not wake the agent: the caller resumes
// it, then sends again.
//
// It is an early, named answer, not a boundary: when the phase cannot be read
// the send goes on and the hub decides, as it did before this check.
func (b *Broker) notRunningRefusal(ctx context.Context, actor, slug string, toManager bool) string {
	phase, err := b.phaseOf(ctx, WorkerSpec{Name: slug})
	if err != nil {
		b.audit("msg", actor, "error", "send->agent:"+slug+": phase: "+err.Error()+" (sending without the check)")
		return ""
	}
	switch {
	case phase == scion.PhaseRunning:
		return ""
	case toManager && phase == "":
		return "the manager has no record on the hub; the operator starts it with `lever up`"
	case toManager:
		return "the manager is not running (phase " + phase + "); the operator resumes it with `lever up`"
	case phase == "":
		return "worker " + slug + " has no record on the hub (never started, or purged); start it first: `lever-manager agent start " + slug + " --task \"…\"`"
	}
	return "worker " + slug + " is not running (phase " + phase + "); resume it first: `lever-manager agent resume " + slug + "`"
}

func (b *Broker) handleMsgSend(w http.ResponseWriter, r *http.Request) {
	// A revoked agent loses its messaging channel too — otherwise a
	// compromised-then-revoked agent could keep steering other agents via
	// messages. Fail closed at use time (identity-keyed, like the gateway).
	caller, ok := b.requireLiveAgent(w, r, "msg", "")
	if !ok {
		return
	}
	var req wire.MsgSendRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("msg", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tgt, rerr := b.resolveMsgTarget(caller, req.To)
	if rerr != nil {
		b.audit("msg", caller, "deny", "send->"+req.To+": "+rerr.Error())
		http.Error(w, rerr.Error(), http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	// Before sendRecorded: a refused send is not made, so it leaves no record.
	slug := strings.TrimPrefix(tgt.scionTo, "agent:")
	if refusal := b.notRunningRefusal(r.Context(), caller, slug, tgt.recipientCN == b.manager); refusal != "" {
		b.audit("msg", caller, "deny", "send->"+tgt.scionTo+": "+refusal)
		http.Error(w, refusal, http.StatusConflict)
		return
	}
	// The kind comes from the caller's identity, never from the request: a
	// worker's message is recorded as that worker's, and anything the
	// manager sends (a note to itself included) as the manager's.
	kind, compose := sentledger.KindManager, func(ref string) string { return markedBody(managerMarker, ref, req.Body) }
	if tgt.relayFrom != "" {
		kind = sentledger.WorkerKind(tgt.relayFrom)
		compose = func(ref string) string { return relayedWorkerBody(tgt.relayFrom, ref, req.Body) }
	}
	ref, err := b.sendRecorded(r.Context(), tgt.recipientCN, slug, kind, compose, scion.MsgOpts{
		To: tgt.scionTo, Interrupt: req.Interrupt, Project: tgt.project,
	})
	if errors.Is(err, errNotRecorded) {
		http.Error(w, "message not sent: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err != nil {
		b.audit("msg", caller, "error", "send->"+req.To+" "+ref+": "+err.Error())
		// Generic wire body (package convention, see worker.go): the scion CLI
		// error text can echo argv (recipient/message body) — detail stays in
		// the audit log only.
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "send->"+tgt.scionTo, "kind", kind, "ref", ref)
	writeJSON(w, wire.MsgSendResponse{OK: true})
}

func (b *Broker) handleMsgList(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "msg", "")
	if !ok {
		return
	}
	var req wire.MsgListRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("msg", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	subject, rerr := b.resolveListSubject(caller, req.Worker)
	if rerr != nil {
		b.audit("msg", caller, "deny", "list "+req.Worker+": "+rerr.Error())
		http.Error(w, rerr.Error(), http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	// Resolve BEFORE reading: without an id to attribute events to there is no
	// safe answer, and returning the raw feed is the leak (see
	// DispatchConfig.ResolveAgentID).
	if b.resolveAgentID == nil {
		b.audit("msg", caller, "error", "list: agent id resolver not wired")
		http.Error(w, "inbox unavailable", http.StatusBadGateway)
		return
	}
	subjectID, err := b.resolveAgentID(r.Context(), subject)
	if err != nil || subjectID == "" {
		b.audit("msg", caller, "error", "list: resolving agent id for "+subject+": "+errText(err))
		http.Error(w, "inbox unavailable", http.StatusBadGateway)
		return
	}
	events, err := b.runtime.Inbox(r.Context(), !req.All, b.instanceProject)
	if err != nil {
		b.audit("msg", caller, "error", "list: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "list "+subject)
	writeJSON(w, wire.MsgListResponse[scion.Event]{Events: eventsForAgent(events, subjectID)})
}
