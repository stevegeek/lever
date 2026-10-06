package remoteproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Agent messages a contact sees (remote.agent_messages).
//
// The hub shows a user every message an agent sends it. With agent messages
// on, a contact is shown an agent's row only when the broker's agent ledger
// recorded its exact text for that agent and contact
// (Config.MatchAgentMessages, over the operator socket). Every other agent
// row is removed from the history answer, and nothing else in the answer
// may carry its text: reply previews go, attachment and extension entries
// of removed rows go, and so do the attachment entries of every agent row
// (their names are agent text no record covers). Paging is the hub's:
// nextCursor and totalCount stay as they are, so a page may hold fewer rows
// than asked for, and totalCount can show how many rows a page hid (never
// their text; accepted). Any answer the proxy cannot read becomes an empty
// page: failing closed.

// AgentMessage is one agent row the broker is asked about: no text.
type AgentMessage struct {
	ID, SHA256 string
	CreatedAt  time.Time
}

type historyRow struct {
	ID        string `json:"id"`
	Sender    string `json:"sender"`
	SenderID  string `json:"senderId"`
	Type      string `json:"type"`
	Msg       string `json:"msg"`
	CreatedAt string `json:"createdAt"`
}

// maxHistoryAnswer bounds the history answer the proxy reads: 200 rows of
// 16000 characters, JSON-escaped.
const maxHistoryAnswer = 32 << 20

// maxMatchRows is the most rows one broker question carries (the hub's own
// page cap; the broker refuses more).
const maxMatchRows = 200

// agentRow reports whether a row needs a ledger record to be shown: every
// row but the contact's own and a hub line. A hub line is a system or state
// type that neither names an agent sender nor carries the DM agent's id; the
// hub stamps both from the agent's token, so an agent cannot write one.
func agentRow(m historyRow, uid, agentID string) bool {
	if m.SenderID == uid && uid != "" && strings.HasPrefix(m.Sender, "user:") {
		return false
	}
	hubLine := (m.Type == "system" || m.Type == "state-change") && !strings.HasPrefix(m.Sender, "agent:") &&
		m.SenderID != agentID && m.SenderID != ""
	return !hubLine
}

// keepAgentRows asks the broker which agent rows to keep. Rows that are not
// agent rows are not asked about (and not in the answer); an agent row from
// another sender id than the DM's agent, with no id, no text (a deleted one), no readable time, or an id that appears
// more than once in rows is never kept. Only an id the proxy asked about
// can be in the answer: the broker's keep list is checked against the
// question, never trusted on its own.
func (g *gate) keepAgentRows(ctx context.Context, contact, agent, agentID, uid string, rows []historyRow) (map[string]bool, error) {
	seen := idCounts(rows)
	var ask []AgentMessage
	for _, m := range rows {
		// Only the DM agent's own rows are asked about: another sender's row
		// with the same text must not take the agent's record.
		if !agentRow(m, uid, agentID) || m.SenderID != agentID || m.ID == "" || m.Msg == "" || seen[m.ID] != 1 {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
		if err != nil {
			continue
		}
		h := sha256.Sum256([]byte(m.Msg))
		ask = append(ask, AgentMessage{ID: m.ID, SHA256: hex.EncodeToString(h[:]), CreatedAt: t})
	}
	if len(ask) == 0 {
		return map[string]bool{}, nil
	}
	if len(ask) > maxMatchRows {
		ask = ask[:maxMatchRows] // the hub's own page cap; the rest stays hidden
	}
	got, err := g.cfg.MatchAgentMessages(ctx, contact, agent, ask)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, m := range ask {
		if got[m.ID] {
			keep[m.ID] = true
		}
	}
	return keep, nil
}

// idCounts counts each row id on a page.
func idCounts(rows []historyRow) map[string]int {
	n := map[string]int{}
	for _, m := range rows {
		n[m.ID]++
	}
	return n
}

// emptyHistory is the fail-closed answer.
var emptyHistory = []byte(`{"messages":[],"totalCount":0}`)

func setBody(resp *http.Response, b []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	resp.Header.Del("Content-Encoding")
}

// filterHistory rewrites a 200 history answer for contact.
func (g *gate) filterHistory(resp *http.Response, contact, agent, agentID, uid string) {
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHistoryAnswer+1))
	_ = resp.Body.Close()
	var doc map[string]json.RawMessage
	var raws []json.RawMessage
	if err != nil || len(body) > maxHistoryAnswer || resp.Header.Get("Content-Encoding") != "" ||
		json.Unmarshal(body, &doc) != nil || json.Unmarshal(doc["messages"], &raws) != nil {
		setBody(resp, emptyHistory)
		return
	}
	rows := make([]historyRow, len(raws))
	listed := map[string]bool{}
	for i, raw := range raws {
		if json.Unmarshal(raw, &rows[i]) != nil {
			rows[i] = historyRow{Sender: "agent:?"} // unreadable: treated as an agent row, never kept
		}
		listed[rows[i].ID] = true
	}
	keep, err := g.keepAgentRows(resp.Request.Context(), contact, agent, agentID, uid, rows)
	if err != nil {
		keep = map[string]bool{}
	}
	kept := []json.RawMessage{}
	removed := map[string]bool{}
	agentIDs := map[string]bool{} // ids of agent rows, kept or not
	seen := idCounts(rows)
	shown := map[string]bool{} // ids of the rows the contact gets
	for i, m := range rows {
		isAgent := agentRow(m, uid, agentID)
		if isAgent {
			agentIDs[m.ID] = true
		}
		// A repeated id is hidden whoever sent it: an entry keyed by it
		// (attachments, extensions) could belong to either row.
		if seen[m.ID] != 1 || isAgent && !keep[m.ID] {
			removed[m.ID] = true
			continue
		}
		kept = append(kept, raws[i])
		shown[m.ID] = true
	}
	doc["messages"], _ = json.Marshal(kept)
	delete(doc, "items")
	// A reply preview quotes ANOTHER row's text, which may be one this
	// contact is not shown: none passes.
	delete(doc, "replyPreviews")
	for _, k := range []string{"messageAttachments", "messageExtensions"} {
		var m map[string]json.RawMessage
		if _, ok := doc[k]; !ok {
			continue
		}
		if json.Unmarshal(doc[k], &m) != nil {
			delete(doc, k)
			continue
		}
		for id := range m {
			// An attachment's name and type are the agent's own text, which
			// the ledger never authorized: no agent row keeps one (files
			// for contacts are a later spec).
			if removed[id] || !listed[id] || k == "messageAttachments" && agentIDs[id] {
				delete(m, id)
				continue
			}
			if k == "messageExtensions" {
				m[id] = dropHiddenReplyTo(m[id], shown)
			}
		}
		doc[k], _ = json.Marshal(m)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		out = emptyHistory
	}
	setBody(resp, out)
}

// dmEntryHidden are the DM list keys a contact does not get with agent
// messages on: each describes the latest row (its text, sender, id, time)
// or whether one is unread, and that row may be one the contact is not
// shown. The chat page takes its unread counts from /lever/api/agents,
// which counts only shown rows.
var dmEntryHidden = []string{"lastMessagePreview", "lastMessageSender", "lastMessageId", "lastActivityAt", "hasUnread"}

// dropHiddenReplyTo removes an extension entry's replyToId unless it names
// a row the contact is shown on this page: the id of a hidden row is not
// the contact's to see. An entry that is not an object is dropped.
func dropHiddenReplyTo(raw json.RawMessage, shown map[string]bool) json.RawMessage {
	var e map[string]json.RawMessage
	if json.Unmarshal(raw, &e) != nil || e == nil {
		return json.RawMessage(`{}`)
	}
	var to string
	if v, ok := e["replyToId"]; ok && (json.Unmarshal(v, &to) != nil || !shown[to]) {
		delete(e, "replyToId")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// stripDMPreviews removes dmEntryHidden from every DM entry. An answer it
// cannot read becomes an empty list.
func stripDMPreviews(resp *http.Response) {
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	out := []byte(`{"dms":[]}`)
	var doc map[string]json.RawMessage
	var dms []map[string]json.RawMessage
	if err == nil && resp.Header.Get("Content-Encoding") == "" && json.Unmarshal(body, &doc) == nil && json.Unmarshal(doc["dms"], &dms) == nil {
		for _, d := range dms {
			for _, k := range dmEntryHidden {
				delete(d, k)
			}
		}
		if dms == nil {
			dms = []map[string]json.RawMessage{}
		}
		doc["dms"], _ = json.Marshal(dms)
		if b, err := json.Marshal(doc); err == nil {
			out = b
		}
	}
	setBody(resp, out)
}

type rewriteKey struct{}

// withRewrite attaches a response rewrite to the request (the fence sets it,
// completeAudit runs it, on the first attempt and on the session retry).
func withRewrite(r *http.Request, f func(*http.Response)) *http.Request {
	r = r.Clone(context.WithValue(r.Context(), rewriteKey{}, f))
	// The proxy reads these answers: ask for them unencoded. Without the
	// client's header the transport asks for gzip itself and decodes it.
	r.Header.Del("Accept-Encoding")
	return r
}

// contactRewrite is the rewrite the fence attached to r, if any.
func contactRewrite(r *http.Request) func(*http.Response) {
	f, _ := r.Context().Value(rewriteKey{}).(func(*http.Response))
	return f
}
