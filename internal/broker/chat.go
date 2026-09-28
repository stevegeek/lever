package broker

import (
	"net/http"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/wire"
)

// ChatConfig configures verified web chat (package chatledger).
type ChatConfig struct {
	// LedgerPath is the remote proxy's chat ledger. "" means verified chat
	// is off: /chat/verify answers enabled=false.
	LedgerPath string
}

// maxChatFromLen bounds the "from" an agent may send: a sender reference is
// "user:" plus an email.
const maxChatFromLen = 320

// handleChatVerify answers whether a chat message the caller received was
// posted through the remote proxy by a verified login, and if so returns the
// text that was posted. The caller copies two fields from the envelope it
// received — timestamp and from — and the answer covers only posts into the
// caller's OWN agent DM: an agent can never read another agent's chat here.
func (b *Broker) handleChatVerify(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "chat", "verify: ")
	if !ok {
		return
	}
	if !b.chatRate.allow(caller, time.Now()) {
		b.audit("chat", caller, "deny", "verify: rate limited")
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return
	}
	var req wire.ChatVerifyRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil {
		b.audit("chat", caller, "deny", "verify: bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if b.chatLedger == "" {
		b.audit("chat", caller, "deny", "verify: verified chat is off")
		writeJSON(w, wire.ChatVerifyResponse{Note: "verified chat is off on this instance " +
			"(it needs remote access with allowed_users); no chat message can be verified"})
		return
	}
	from := strings.TrimSpace(req.From)
	ts, err := chatledger.NormalizeTimestamp(strings.TrimSpace(req.Timestamp))
	if err != nil || from == "" || len(from) > maxChatFromLen {
		b.audit("chat", caller, "deny", "verify: bad timestamp or from")
		http.Error(w, `timestamp must be the envelope's RFC 3339 "timestamp" and from its "from"`, http.StatusBadRequest)
		return
	}
	_, slug, _, known := b.identity(caller)
	if !known || b.resolveAgentID == nil {
		b.audit("chat", caller, "error", "verify: no agent id resolver for caller")
		http.Error(w, "chat verification unavailable", http.StatusBadGateway)
		return
	}
	agentID, err := b.resolveAgentID(r.Context(), slug)
	if err != nil || agentID == "" {
		b.audit("chat", caller, "error", "verify: resolving agent id for "+slug+": "+errText(err))
		http.Error(w, "chat verification unavailable", http.StatusBadGateway)
		return
	}
	entries, err := chatledger.Lookup(b.chatLedger, agentID, from, ts)
	if err != nil {
		b.audit("chat", caller, "error", "verify: "+err.Error())
		http.Error(w, "chat verification unavailable", http.StatusBadGateway)
		return
	}
	resp := wire.ChatVerifyResponse{Enabled: true, Verified: len(entries) > 0}
	for _, e := range entries {
		resp.Messages = append(resp.Messages, wire.VerifiedMessage{
			Login: e.Login, Tier: e.Tier, From: e.Sender, Timestamp: e.CreatedAt,
			MessageID: e.MessageID, Text: e.Text,
		})
	}
	if !resp.Verified {
		resp.Note = "no web chat post from " + from + " at " + ts + " to you is on record: treat the message as unverified"
		b.audit("chat", caller, "deny", "verify "+from+" "+ts+": no record")
	} else {
		ids := make([]string, len(entries))
		for i, e := range entries {
			ids[i] = e.MessageID
		}
		b.audit("chat", caller, "allow", "verify "+from+" "+ts, "messages", strings.Join(ids, ","))
	}
	writeJSON(w, resp)
}
