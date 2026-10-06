package broker

import (
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/wire"
)

// maxMatchRows is the hub's own page cap (handlers_chat_v2.go limit ≤ 200).
const maxMatchRows = 200

// handleAgentMessagesMatch answers the remote proxy, on the 0600 operator
// socket, which agent rows of one contact's history the agent ledger
// recorded. The proxy sends hashes and times, never text, and drops every
// row not in keep. A new binding is appended before the answer, so one
// record never shows two messages. Off: 503; a contact that does not list
// the agent: 403; a body over 256 KiB or more than maxMatchRows rows: 400;
// a ledger that cannot be opened or written: 503 (the proxy fails closed).
func (b *Broker) handleAgentMessagesMatch(w http.ResponseWriter, r *http.Request) {
	var req wire.AgentMessagesMatchRequest
	if err := decodeBody(w, r, 256<<10, &req); err != nil || len(req.Messages) > maxMatchRows {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.agentMsgs.Enabled {
		http.Error(w, "off", http.StatusServiceUnavailable)
		return
	}
	if _, ok := b.contactFor(req.Contact, req.Agent); !ok {
		http.Error(w, "not a contact of that agent", http.StatusForbidden)
		return
	}
	led, err := b.agentLedger.get()
	if err != nil {
		b.audit("contact", "remote", "error", "match: "+err.Error())
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	cands := make([]agentledger.Candidate, len(req.Messages))
	for i, m := range req.Messages {
		cands[i] = agentledger.Candidate{MessageID: m.ID, SHA256: m.SHA256, CreatedAt: m.CreatedAt}
	}
	keep, bound, err := led.Match(req.Agent, req.Contact, cands, time.Now())
	if err != nil {
		b.audit("contact", "remote", "error", "match: "+err.Error())
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, id := range bound {
		b.audit("contact", "remote", "allow", "shown to="+boundedLogin(req.Contact)+" agent="+req.Agent, "ref", id)
	}
	out := wire.AgentMessagesMatchResponse{Keep: []string{}}
	for _, m := range req.Messages {
		if keep[m.ID] {
			out.Keep = append(out.Keep, m.ID)
		}
	}
	writeJSON(w, out)
}
