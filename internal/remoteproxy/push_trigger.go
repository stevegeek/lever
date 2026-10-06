package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/webpush"
)

// Which new message raises a push.
//
// A check reads the newest page of one DM as the login (its own session,
// what the page itself would read), picks the newest row the DM agent
// wrote, and, for a contact with agent messages on, only a row the spec-2
// matcher shows (agentmsgs.go): a contact is never told about a message
// it would not be shown. The mark per (login, agent) is the newest row a
// decision covered; it is stored before the push goes out, so a crash or
// restart never sends one row twice.

const (
	pushHistoryPage = 20
	pushEvery       = 60 * time.Second
	pushCatchUp     = time.Hour
)

type pushTarget struct{ name, id, key string }

// pushSession is one connected stream's view: the login's hub user, its
// session, its tier; stale is set when the hub rejects the session, and
// schedule asks for a later check (the trailing push).
type pushSession struct {
	uid, cookie, tier string
	stale             atomic.Bool
	schedule          func(t pushTarget, after time.Duration)
}

// pushTargets are the agents login may message that have a usable hub
// record, each with its DM key. See-only and hidden agents are never one.
func (g *gate) pushTargets(ctx context.Context, login, uid string) ([]pushTarget, error) {
	v := g.viewerFor(login)
	recs, err := g.records(ctx)
	if err != nil {
		return nil, err
	}
	var out []pushTarget
	for _, n := range v.message {
		rec, ok := recs[n]
		if !ok || !validHubID(rec.ID) {
			continue
		}
		out = append(out, pushTarget{name: n, id: rec.ID, key: "dm:agent:" + rec.ID + ":user:" + uid})
	}
	return out, nil
}

// pushable: a message the DM agent wrote (its id, an agent sender), not a
// hub line, not the login's own, not deleted.
func pushable(m historyRow, uid, agentID string) bool {
	return m.ID != "" && m.Msg != "" && m.SenderID == agentID && strings.HasPrefix(m.Sender, "agent:") &&
		m.Type != "system" && m.Type != "state-change" && agentRow(m, uid, agentID)
}

func newerRow(at time.Time, id string, than time.Time, thanID string) bool {
	c := at.Compare(than)
	return c > 0 || c == 0 && id > thanID
}

// newestForPush is the newest pushable row of t's DM for login, or ok=false.
func (g *gate) newestForPush(ctx context.Context, login, cookie, uid, tier string, t pushTarget) (historyRow, time.Time, bool, error) {
	var page struct {
		Messages []historyRow `json:"messages"`
		Items    []historyRow `json:"items"`
	}
	// hubBody, not hubGet: a page of long agent rows can pass hubGet's 1 MiB
	// bound, and a decode failure would silence every push for the DM.
	p := "/api/v1/chat/conversations/" + url.PathEscape(t.key) + fmt.Sprintf("/messages?limit=%d", pushHistoryPage)
	st, body, err := g.chat.hubBody(ctx, cookie, p)
	switch {
	case err != nil:
		return historyRow{}, time.Time{}, false, err
	case st == http.StatusUnauthorized:
		return historyRow{}, time.Time{}, false, errSessionUnknown
	case st != http.StatusOK:
		return historyRow{}, time.Time{}, false, fmt.Errorf("hub history: HTTP %d", st)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return historyRow{}, time.Time{}, false, err
	}
	rows := page.Messages
	if rows == nil {
		rows = page.Items
	}
	var keep map[string]bool
	if tier == chatledger.TierContact && g.cfg.MatchAgentMessages != nil {
		if keep, err = g.keepAgentRows(ctx, login, t.name, t.id, uid, rows); err != nil {
			return historyRow{}, time.Time{}, false, err // fail closed: no push
		}
	}
	seen := idCounts(rows)
	var best historyRow
	var bestAt time.Time
	found := false
	for _, m := range rows {
		if !pushable(m, uid, t.id) || seen[m.ID] != 1 || keep != nil && !keep[m.ID] {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
		if err != nil {
			continue
		}
		if !found || newerRow(at, m.ID, bestAt, best.ID) {
			best, bestAt, found = m, at, true
		}
	}
	return best, bestAt, found, nil
}

// dmUnread asks the hub whether login has an unread message in key. Any
// fault reads as unread: a push too many beats a message missed.
func (g *gate) dmUnread(ctx context.Context, cookie, key string) bool {
	var list struct {
		DMs []hubDM `json:"dms"`
	}
	if st, err := g.chat.hubGet(ctx, cookie, "/api/v1/chat/dms", &list); err != nil || st != http.StatusOK {
		return true
	}
	for _, d := range list.DMs {
		if d.ConversationKey == key {
			return d.HasUnread
		}
	}
	return true
}

// check decides one (login, agent). catchUp is the pass after a (re)connect:
// a DM never seen sets the mark only, and a row older than pushCatchUp only
// moves it.
func (p *Push) check(ctx context.Context, login string, t pushTarget, s *pushSession, catchUp bool) {
	row, at, ok, err := p.g.newestForPush(ctx, login, s.cookie, s.uid, s.tier, t)
	if errors.Is(err, errSessionUnknown) {
		s.stale.Store(true)
		return
	}
	if err != nil {
		p.record(login, t.name, "", DecisionPushFailed, 0, "history")
		return
	}
	mark, had := p.store.Mark(login, t.name)
	if !ok {
		// An empty DM on a connect is seen too (an empty mark): its first
		// message, even one stored during a reconnect gap, is then new.
		if catchUp && !had {
			_ = p.store.SetMark(login, t.name, PushMark{})
		}
		return
	}
	if had && !newerRow(at, row.ID, mark.At, mark.ID) {
		return
	}
	next := PushMark{ID: row.ID, At: at}
	if catchUp && (!had || p.now().Sub(at) > pushCatchUp) {
		_ = p.store.SetMark(login, t.name, next)
		return
	}
	if !p.g.dmUnread(ctx, s.cookie, t.key) {
		_ = p.store.SetMark(login, t.name, next)
		return
	}
	if wait := p.takeSlot(login, t.name); wait > 0 {
		if s.schedule != nil {
			s.schedule(t, wait)
		}
		return
	}
	if err := p.store.SetMark(login, t.name, next); err != nil {
		p.record(login, t.name, "", DecisionPushFailed, 0, "store")
		return
	}
	p.notify(ctx, login, t.name)
}

// takeSlot spends the (login, agent) push slot, or reports how long until
// it is free.
func (p *Push) takeSlot(login, agent string) time.Duration {
	k := login + "\x00" + agent
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if t, ok := p.last[k]; ok && now.Sub(t) < pushEvery {
		return pushEvery - now.Sub(t)
	}
	p.last[k] = now
	return 0
}

// notify sends the content-free push to each of login's subscriptions.
func (p *Push) notify(ctx context.Context, login, agent string) {
	payload, _ := json.Marshal(struct {
		V     int    `json:"v"`
		Agent string `json:"agent"`
	}{1, agent})
	for _, sub := range p.store.Subs(login) {
		err := p.send.Send(ctx, sub.Subscription, payload)
		host := endpointHost(sub.Endpoint)
		switch {
		case err == nil:
			p.record(login, agent, host, DecisionPushSent, http.StatusCreated, "sent")
		case webpush.Gone(err):
			_, _ = p.store.Remove(login, sub.Endpoint)
			p.kick()
			p.record(login, agent, host, DecisionPushGone, webpush.StatusOf(err), "gone")
		default:
			p.record(login, agent, host, DecisionPushFailed, webpush.StatusOf(err), sendFault(err))
		}
	}
}

// sendFault names a failed send in fixed words.
func sendFault(err error) string {
	switch {
	case webpush.StatusOf(err) != 0:
		return "http"
	case errors.Is(err, webpush.ErrEndpoint):
		return "endpoint"
	case errors.Is(err, webpush.ErrAddress):
		return "address"
	}
	return "unreachable"
}

// record audits one push decision and keeps the last outcome for doctor.
// Path names the agent; the endpoint appears as its host only.
func (p *Push) record(login, agent, host string, d Decision, status int, reason string) {
	if p.audit != nil {
		p.audit(AuditLine{Time: p.now().UTC(), TSLogin: truncateAudit(login), Method: "PUSH", Path: "/lever/push/" + agent,
			Decision: d, Status: status, Reason: reason, Error: host})
	}
	if host != "" {
		result := map[Decision]string{DecisionPushSent: "sent", DecisionPushGone: "gone"}[d]
		if result == "" {
			result = "failed"
		}
		_ = WritePushStatus(p.dir, PushStatus{At: p.now().UTC(), Result: result, Status: status, Host: host, TestHosts: len(p.test) > 0})
	}
}
