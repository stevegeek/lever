package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/webpush"
)

// A push check reads the newest page of a DM with the login's own session and
// takes the newest row the agent wrote; for a contact with agent messages on,
// only a row the matcher would show (agentmsgs.go). The mark is stored before
// the push goes out, so a restart never sends one row twice.

const (
	pushHistoryPage = 20
	pushEvery       = 60 * time.Second
	pushCatchUp     = time.Hour
)

type pushTarget struct{ name, id, key string }

// pushSession is one connected stream's view of a login; schedule asks for a
// later check (the trailing push).
type pushSession struct {
	uid, cookie, tier string
	stale             atomic.Bool
	schedule          func(t pushTarget, after time.Duration)
	// end, when set, ends the stream at once (a rejected session): the
	// next connect logs in again and its catch-up runs the check again.
	end func()
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

func (g *gate) newestForPush(ctx context.Context, login, cookie, uid, tier string, t pushTarget) (historyRow, time.Time, bool, error) {
	var page struct {
		Messages json.RawMessage `json:"messages"`
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
	var rows []historyRow
	if err := json.Unmarshal(body, &page); err != nil {
		return historyRow{}, time.Time{}, false, err
	}
	if err := historyMessages(page.Messages, &rows); err != nil {
		return historyRow{}, time.Time{}, false, err
	}
	var keep map[string]bool
	if tier == chatledger.TierContact && g.cfg.MatchAgentMessages != nil {
		// The peek, never the binding match: a push check must not decide which
		// row a ledger record shows. No peek means no answer: fail closed.
		var peek func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error)
		if g.cfg.PeekAgentMessages != nil {
			peek = func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error) {
				keep, _, err := g.cfg.PeekAgentMessages(ctx, contact, agent, msgs)
				return keep, err
			}
		}
		if keep, err = askAgentRows(ctx, peek, login, t.name, t.id, uid, rows); err != nil {
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

// errCheck: the check decided nothing (history or store failed); it is retried.
var errCheck = errors.New("push check: no decision")

// check decides one (login, agent). On catchUp (after a connect) a DM never
// seen only sets the mark, and a row older than pushCatchUp only moves it.
func (p *Push) check(ctx context.Context, login string, t pushTarget, s *pushSession, catchUp bool) error {
	row, at, ok, err := p.g.newestForPush(ctx, login, s.cookie, s.uid, s.tier, t)
	if errors.Is(err, errSessionUnknown) {
		s.stale.Store(true)
		if s.end != nil {
			s.end()
		}
		return errSessionUnknown
	}
	if err != nil {
		p.record(login, t.name, "", DecisionPushFailed, 0, "history")
		return errCheck
	}
	mark, had := p.store.Mark(login, t.name)
	if !ok {
		// An empty DM on a connect is seen too (an empty mark): its first
		// message, even one stored during a reconnect gap, is then new.
		if catchUp && !had {
			_ = p.store.SetMark(login, t.name, PushMark{})
		}
		return nil
	}
	if had && !newerRow(at, row.ID, mark.At, mark.ID) {
		return nil
	}
	next := PushMark{ID: row.ID, At: at}
	if catchUp && (!had || p.now().Sub(at) > pushCatchUp) {
		_ = p.store.SetMark(login, t.name, next)
		return nil
	}
	if !p.g.dmUnread(ctx, s.cookie, t.key) {
		_ = p.store.SetMark(login, t.name, next)
		return nil
	}
	wait, release := p.takeSlot(login, t.name)
	if wait > 0 {
		if s.schedule != nil {
			s.schedule(t, wait)
		}
		return nil
	}
	if err := p.store.SetMark(login, t.name, next); err != nil {
		// Nothing went out: the retry may push at once. The mark is not
		// stored, so the retry decides the same row again, not a second.
		release()
		p.record(login, t.name, "", DecisionPushFailed, 0, "store")
		return errCheck
	}
	// The mark is stored: the sends must outlive the stream (idle drop, hub
	// restart) or the push is lost. notify bounds them itself.
	p.notify(context.WithoutCancel(ctx), login, t.name)
	return nil
}

// release gives back a spent slot when no push went out.
func (p *Push) takeSlot(login, agent string) (wait time.Duration, release func()) {
	k := login + "\x00" + agent
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	prev, had := p.last[k]
	if had && now.Sub(prev) < pushEvery {
		return pushEvery - now.Sub(prev), func() {}
	}
	p.last[k] = now
	return 0, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.last[k].Equal(now) {
			return // spent again since
		}
		if had {
			p.last[k] = prev
		} else {
			delete(p.last, k)
		}
	}
}

// notify sends the content-free push to every subscription at once, each
// bounded by SendTimeout, so a slow push service holds the login's checks for
// one send time. After Run stops the sends get shutdownGrace more.
func (p *Push) notify(ctx context.Context, login, agent string) {
	if p.g != nil && !p.g.loginCurrent(login) {
		return
	}
	payload, _ := json.Marshal(struct {
		V     int    `json:"v"`
		Agent string `json:"agent"`
	}{1, agent})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-p.stopping:
		}
		select {
		case <-ctx.Done():
		case <-time.After(p.shutdownGrace):
			cancel()
		}
	}()
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxSubsPerLogin)
	for _, sub := range p.store.Subs(login) {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, webpush.SendTimeout)
			err := p.send.Send(sctx, sub.Subscription, payload)
			cancel()
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
		})
	}
	wg.Wait()
}

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
	p.recordCount(login, agent, host, d, status, reason, nil)
}

func (p *Push) recordCount(login, agent, host string, d Decision, status int, reason string, count *int) {
	if p.audit != nil {
		p.audit(AuditLine{Time: p.now().UTC(), TSLogin: truncateAudit(login), Method: "PUSH", Path: "/lever/push/" + agent,
			Decision: d, Status: status, Reason: reason, Error: host, Count: count})
	}
	if host != "" {
		result := map[Decision]string{DecisionPushSent: "sent", DecisionPushGone: "gone"}[d]
		if result == "" {
			result = "failed"
		}
		_ = WritePushStatus(p.dir, PushStatus{At: p.now().UTC(), Result: result, Status: status, Host: host, TestHosts: len(p.test) > 0})
	}
}
