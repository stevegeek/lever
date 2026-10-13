package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/wire"
)

var errUnknownRecipient = errors.New("unknown recipient")

// msgTarget is a resolved, policy-approved message destination. Every target
// is an agent: scion has no broker-routable user inbox.
type msgTarget struct {
	scionTo string
	project string
	// recipientCN is whom the sent ledger records the message for, and who
	// alone can verify it.
	recipientCN string
	// relayFrom is the sending worker's slug, "" for the manager.
	relayFrom string
}

// Lever's message markers. Every message goes through the host scion CLI
// with the controller PAT, so the envelope sender is the controller's hub
// user whoever wrote the text.
//
// The markers are readable hints, never evidence: anyone whose text reaches
// an agent can type one. The sent ledger decides who wrote a message (the
// first line carries the send's ref for message_verify).
const (
	relayMarkerFormat     = "[lever: relayed from worker %s]"
	directiveNoticeMarker = "[lever: operator directive notice]"
	managerMarker         = "[lever: from the manager]"
	operatorNoteMarker    = "[lever: operator note]"
)

// markerLike matches what a reader could take for a lever marker or a scion
// envelope delimiter, across case, spacing and look-alike brackets. It keeps a
// session readable; it is not a boundary.
var markerLike = regexp.MustCompile(`(?i)[\[［⟦〚][\s\p{Cf}]*lever[\s\p{Cf}]*[:：]|-{3}[\s\p{Cf}]*(begin|end)[\s\p{Cf}]+scion[\s\p{Cf}]+message[\s\p{Cf}]*-{3}`)

func neutraliseMarkers(body string) string {
	return markerLike.ReplaceAllStringFunc(body, func(m string) string {
		if strings.HasPrefix(m, "-") {
			return "(quoted scion delimiter)"
		}
		return "(quoted lever marker:"
	})
}

func markedBody(marker, ref, text string) string {
	return refLine(marker, ref) + "\n" + neutraliseMarkers(text)
}

// relayedWorkerBody names the worker by its config slug, never request text.
func relayedWorkerBody(slug, ref, body string) string {
	return markedBody(fmt.Sprintf(relayMarkerFormat, slug), ref, body)
}

// resolveMsgTarget applies the messaging policy: identity-derived,
// config-authoritative, default-deny. The error text is the deny reason.
func (b *Broker) resolveMsgTarget(caller, to string) (msgTarget, error) {
	// The caller is a cert CN: a slug alias is not an identity.
	callerCN, callerSlug, isManager, ok := b.identity(caller)
	if !ok || callerCN != caller {
		return msgTarget{}, fmt.Errorf("caller %q is not the manager or a declared worker", caller)
	}
	// scion knows the manager by its slug, not its cert CN.
	relayFrom := ""
	if !isManager {
		relayFrom = callerSlug
	}
	managerTarget := msgTarget{scionTo: "agent:" + b.managerSlug, project: b.instanceProject, relayFrom: relayFrom, recipientCN: b.manager}
	// scion refuses user-addressed sends from outside an agent container, so
	// user:* is honoured only when it plainly means the manager; anything
	// else is denied by name. A bare "user:" falls through to the
	// unknown-recipient deny.
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
	case toManager:
		return managerTarget, nil
	case !isManager && caller != name && !b.workerToWorker:
		// Before the unknown check, with one text for both, so a worker cannot
		// probe for the names of peers it may not message.
		return msgTarget{}, fmt.Errorf("recipient %q is not the manager or this worker (worker→worker messaging is disabled)", to)
	case !known:
		return msgTarget{}, fmt.Errorf("%w %q", errUnknownRecipient, to)
	}
	return msgTarget{scionTo: "agent:" + slug, project: b.instanceProject, relayFrom: relayFrom, recipientCN: cn}, nil
}

// managerAlias is taught to workers instead of the manager's slug or cert CN.
const managerAlias = "user:manager"

const msgRecipientCap = 16

// msgRecipients is resolveMsgTarget's policy read the other way: it lists
// only names the caller can already reach, all from config (never request
// text). nil for a caller that is not an identity.
func (b *Broker) msgRecipients(caller string) []string {
	callerCN, _, isManager, ok := b.identity(caller)
	if !ok || callerCN != caller {
		return nil
	}
	var workers []string
	switch {
	case isManager || b.workerToWorker:
		for name := range b.workers {
			workers = append(workers, "agent:"+name)
		}
		slices.Sort(workers)
	default:
		workers = []string{"agent:" + caller}
	}
	if isManager {
		return append([]string{"agent:" + b.managerSlug}, workers...)
	}
	return append([]string{managerAlias}, workers...)
}

func recipientsHint(addrs []string) string {
	if len(addrs) == 0 {
		return ""
	}
	more := ""
	if n := len(addrs) - msgRecipientCap; n > 0 {
		addrs = addrs[:msgRecipientCap]
		more = fmt.Sprintf(" and %d more (`lever-manager msg recipients` lists them all)", n)
	}
	return "; you may send to: " + strings.Join(addrs, ", ") + more
}

// resolveListSubject resolves whose inbox caller may read, as an agent slug:
// the manager its own or any worker's, a worker its own only. A project would
// not scope anything: the hub scopes `scion notifications` to the controller
// PAT's user, so the feed is fleet-wide whatever -g says.
func (b *Broker) resolveListSubject(caller, worker string) (string, error) {
	if caller == b.manager {
		if worker == "" {
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

func errText(err error) string {
	if err == nil {
		return "hub returned no id for it"
	}
	return err.Error()
}

// eventsForAgent keeps only the events the hub attributes to agentID. An
// event with no agentId is dropped: what cannot be attributed is shown to no
// one.
func eventsForAgent(events []scion.Event, agentID string) []scion.Event {
	kept := make([]scion.Event, 0, len(events))
	for _, e := range events {
		if id, _ := e["agentId"].(string); id != "" && id == agentID {
			kept = append(kept, e)
		}
	}
	return kept
}

// notRunningRefusal returns a named refusal for a recipient that is not
// running, or "" to send: the hub refuses a message in any other phase with
// a bare 409. It is not a boundary: when the phase cannot be read the send
// goes on and the hub decides. The read has its own, shorter bound so a slow
// list leaves time for the send.
func (b *Broker) notRunningRefusal(ctx context.Context, actor, slug string, toManager bool) string {
	pctx, cancel := context.WithTimeout(ctx, cmp.Or(b.msgPhaseTimeout, msgPhaseTimeoutDefault))
	phase, err := b.phaseOf(pctx, WorkerSpec{Name: slug})
	cancel()
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
	case phase == "" && actor == operatorActor:
		return "worker " + slug + " has no record on the hub (never started, or purged); ask the manager to start it (`lever-manager agent start " + slug + " --task \"…\"` in the manager's session)"
	case phase == "":
		return "worker " + slug + " has no record on the hub (never started, or purged); start it first: `lever-manager agent start " + slug + " --task \"…\"`"
	case actor == operatorActor:
		return "worker " + slug + " is not running (phase " + phase + "); ask the manager to resume it (`lever-manager agent resume " + slug + "` in the manager's session)"
	}
	return "worker " + slug + " is not running (phase " + phase + "); resume it first: `lever-manager agent resume " + slug + "`"
}

const msgPhaseTimeoutDefault = 5 * time.Second

// operatorActor is the audit actor of the host operator's own sends.
const operatorActor = "operator"

// maxMsgRecipientLen bounds the `to` of a send: agent names are at most 63
// bytes (config.nameRE) plus an "agent:"/"user:" prefix.
const maxMsgRecipientLen = 128

func (b *Broker) handleMsgSend(w http.ResponseWriter, r *http.Request) {
	// A revoked agent loses messaging too, or it could keep steering other
	// agents. Fail closed at use time.
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
	// Agent-chosen: bound it before it is echoed or audited.
	if len(req.To) > maxMsgRecipientLen {
		b.audit("msg", caller, "deny", "send: recipient longer than the limit")
		http.Error(w, "recipient is too long", http.StatusBadRequest)
		return
	}
	tgt, rerr := b.resolveMsgTarget(caller, req.To)
	if rerr != nil {
		b.audit("msg", caller, "deny", "send->"+req.To+": "+rerr.Error())
		http.Error(w, rerr.Error()+recipientsHint(b.msgRecipients(caller)), http.StatusForbidden)
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
	// The kind comes from the caller's identity, never from the request.
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
		// Generic wire body: the scion error text can echo argv. The detail
		// stays in the audit log.
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("msg", caller, "allow", "send->"+tgt.scionTo, "kind", kind, "ref", ref)
	writeJSON(w, wire.MsgSendResponse{OK: true})
}

// handleMsgRecipients is uncapped: the config bounds it.
func (b *Broker) handleMsgRecipients(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "msg", "")
	if !ok {
		return
	}
	addrs := b.msgRecipients(caller)
	if addrs == nil {
		b.audit("msg", caller, "deny", "recipients: not the manager or a declared worker")
		http.Error(w, fmt.Sprintf("caller %q is not the manager or a declared worker", caller), http.StatusForbidden)
		return
	}
	b.audit("msg", caller, "allow", "recipients")
	writeJSON(w, wire.MsgRecipientsResponse{Recipients: addrs})
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
	if len(req.Worker) > maxMsgRecipientLen {
		b.audit("msg", caller, "deny", "list: worker name longer than the limit")
		http.Error(w, "worker name is too long", http.StatusBadRequest)
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
	// Without an id to attribute events there is no safe answer: the raw feed
	// would leak (DispatchConfig.ResolveAgentID).
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
	// Each event embeds the worker's own text: mark, bound and sanitize it so
	// no reader takes it for lever's statement.
	kept := eventsForAgent(events, subjectID)
	for i, e := range kept {
		kept[i] = scion.WorkerReported(e)
	}
	writeJSON(w, wire.MsgListResponse[scion.Event]{Events: kept})
}
