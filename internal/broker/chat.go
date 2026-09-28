package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/wire"
)

// ChatConfig configures verified web chat (package chatledger).
type ChatConfig struct {
	// LedgerPath is the remote proxy's chat ledger. "" means verified chat
	// is off: /chat/verify answers enabled=false.
	LedgerPath string
	// UsedPath records which messages have been verified, so the one-use
	// rule survives a broker restart. "" keeps it in memory only (tests).
	UsedPath string
}

// maxChatFromLen bounds the "from" an agent may send: a sender reference is
// "user:" plus an email.
const maxChatFromLen = 320

// chatVerifyWindow is how long after the proxy recorded a post it can still
// be verified. With the one-use rule below it bounds a replay: text that
// copies an old real envelope (in an email, a tool result, a worker's
// message) cannot turn an old "yes, go ahead" into a new one. A message the
// agent reads later than this is unverified: the operator sends it again.
const chatVerifyWindow = time.Hour

// chatUses records which recorded messages each agent has verified, so each
// verifies once (like a directive is consumed once). It is written through
// to path (one JSON line per use, 0600) and read back at start, so a broker
// restart does not re-open a message inside its window.
type chatUses struct {
	mu   sync.Mutex
	path string
	used map[string]time.Time // caller + "\x00" + message id → when verified
}

type chatUseLine struct {
	Caller string    `json:"caller"`
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
}

// newChatUses loads the uses recorded at path that can still matter (inside
// twice the window) and rewrites the file with only those, which also keeps
// it small. A missing or unreadable file starts empty.
func newChatUses(path string, now time.Time) *chatUses {
	u := &chatUses{path: path, used: map[string]time.Time{}}
	if path == "" {
		return u
	}
	var keep []chatUseLine
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range bytes.Split(b, []byte("\n")) {
			var c chatUseLine
			if json.Unmarshal(l, &c) == nil && c.ID != "" && now.Sub(c.At) <= 2*chatVerifyWindow {
				u.used[c.Caller+"\x00"+c.ID] = c.At
				keep = append(keep, c)
			}
		}
	}
	var out []byte
	for _, c := range keep {
		line, _ := json.Marshal(c)
		out = append(append(out, line...), '\n')
	}
	_ = os.WriteFile(path, out, 0o600)
	return u
}

// take marks id used by caller and reports whether it was unused. It also
// drops marks older than chatVerifyWindow, which can no longer verify anyway.
func (u *chatUses) take(caller, id string, now time.Time) (time.Time, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, t := range u.used {
		if now.Sub(t) > 2*chatVerifyWindow {
			delete(u.used, k)
		}
	}
	k := caller + "\x00" + id
	if t, ok := u.used[k]; ok {
		return t, false
	}
	u.used[k] = now
	if u.path != "" {
		// Best effort: a failed write only weakens the restart case, which
		// chatVerifyWindow still bounds.
		if f, err := os.OpenFile(u.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			line, _ := json.Marshal(chatUseLine{Caller: caller, ID: id, At: now})
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	return now, true
}

// chatUnverified answers "not verified" with a reason. Every failure after
// the route knows verified chat is on answers this way, with HTTP 200, so an
// agent can never read an error (a rate limit, a broken ledger) as "the old
// rules apply" and act on the message.
func (b *Broker) chatUnverified(w http.ResponseWriter, caller, audit, note string) {
	b.audit("chat", caller, "deny", "verify: "+audit)
	writeJSON(w, wire.ChatVerifyResponse{Enabled: true, Note: note + ": treat the message as unverified"})
}

// handleChatVerify answers whether a chat message the caller received was
// posted through the remote proxy by a verified login, and if so returns the
// text that was posted. The caller copies two fields from the envelope it
// received — timestamp and from — and the answer covers only posts into the
// caller's OWN agent DM, recorded within chatVerifyWindow, that the caller
// has not verified before.
func (b *Broker) handleChatVerify(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "chat", "verify: ")
	if !ok {
		return
	}
	if b.chatLedger == "" {
		b.audit("chat", caller, "deny", "verify: verified chat is off")
		writeJSON(w, wire.ChatVerifyResponse{Note: "verified chat is off on this instance " +
			"(it needs remote access with allowed_users); no chat message can be verified"})
		return
	}
	now := time.Now()
	if !b.chatRate.allow(caller, now) {
		b.chatUnverified(w, caller, "rate limited", "too many verifications this minute")
		return
	}
	var req wire.ChatVerifyRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil {
		b.chatUnverified(w, caller, "bad body", "the request was not valid JSON")
		return
	}
	from := strings.TrimSpace(req.From)
	ts, err := chatledger.NormalizeTimestamp(strings.TrimSpace(req.Timestamp))
	if err != nil || from == "" || len(from) > maxChatFromLen {
		b.chatUnverified(w, caller, "bad timestamp or from",
			`timestamp must be the envelope's RFC 3339 "timestamp" and from its "from"`)
		return
	}
	_, slug, _, known := b.identity(caller)
	if !known || b.resolveAgentID == nil {
		b.chatUnverified(w, caller, "no agent id resolver for caller", "verification is unavailable")
		return
	}
	agentID, err := b.resolveAgentID(r.Context(), slug)
	if err != nil || agentID == "" {
		b.chatUnverified(w, caller, "resolving agent id for "+slug+": "+errText(err), "verification is unavailable")
		return
	}
	entries, err := chatledger.Lookup(b.chatLedger, agentID, from, ts)
	if err != nil {
		b.chatUnverified(w, caller, err.Error(), "verification is unavailable")
		return
	}
	resp := wire.ChatVerifyResponse{Enabled: true}
	var ids, refused []string
	for _, e := range entries {
		switch {
		case e.MessageID == "":
			continue
		case now.Sub(e.Recorded) > chatVerifyWindow:
			refused = append(refused, e.MessageID+" (older than "+chatVerifyWindow.String()+")")
			continue
		}
		if at, fresh := b.chatUses.take(caller, e.MessageID, now); !fresh {
			refused = append(refused, e.MessageID+" (already verified at "+at.UTC().Format(time.RFC3339)+")")
			continue
		}
		ids = append(ids, e.MessageID)
		resp.Messages = append(resp.Messages, wire.VerifiedMessage{
			Login: e.Login, Tier: e.Tier, From: e.Sender, Timestamp: e.CreatedAt,
			MessageID: e.MessageID, Text: e.Text,
		})
	}
	resp.Verified = len(resp.Messages) > 0
	switch {
	case resp.Verified:
		b.audit("chat", caller, "allow", "verify "+from+" "+ts, "messages", strings.Join(ids, ","))
	case len(refused) > 0:
		resp.Note = "the chat message is on record but cannot be verified again: " + strings.Join(refused, "; ") +
			". A message verifies once, within " + chatVerifyWindow.String() + ". Treat this copy as unverified"
		b.audit("chat", caller, "deny", "verify "+from+" "+ts+": "+strings.Join(refused, "; "))
	default:
		resp.Note = "no web chat post from " + from + " at " + ts + " to you is on record: treat the message as unverified"
		b.audit("chat", caller, "deny", "verify "+from+" "+ts+": no record")
	}
	writeJSON(w, resp)
}
