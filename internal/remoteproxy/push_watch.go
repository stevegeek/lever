package remoteproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// The push watchers.
//
// Run keeps one goroutine per login that has a push subscription. Each
// holds one hub events stream (/events?sub=user.<uid>.chat.>) with the
// login's own hub session, the same subjects the page subscribes to. The
// stream is only a hint: an event for one of the login's DMs makes the
// scheduler run check (push_trigger.go), which reads the history itself.
//
//   - Start: Run reads the store's logins (after a proxy restart too) and
//     starts a watcher for each; subscribe, unsubscribe and a gone
//     subscription kick it to start or stop watchers.
//   - Connect: a session from the login driver (it renews after 12 h); a
//     401 or login redirect invalidates it and the next attempt logs in.
//   - After every connect, a catch-up check of every target: the hub does
//     not replay events, and the stream is already open, so a message
//     stored after this point raises an event and one stored before it is
//     found by the catch-up.
//   - Reconnect with backoff (doubling, jittered, reset after a healthy
//     stream); an idle stream (no event or heartbeat for idle; the hub
//     beats every 30 s) is dropped and reopened.
//   - Shutdown: Run's context ends every stream and waits for the
//     watchers.

func (p *Push) Run(ctx context.Context) {
	if p.g == nil {
		return // no chat page: NewHandler did not attach
	}
	type watcher struct{ cancel context.CancelFunc }
	watchers := map[string]watcher{}
	var wg sync.WaitGroup
	reconcile := func() {
		want := p.store.Logins()
		for l, w := range watchers {
			if !slices.Contains(want, l) {
				w.cancel()
				delete(watchers, l)
			}
		}
		for _, l := range want {
			if _, ok := watchers[l]; ok {
				continue
			}
			wctx, cancel := context.WithCancel(ctx)
			watchers[l] = watcher{cancel}
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.watch(wctx, l)
			}()
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			for _, w := range watchers {
				w.cancel()
			}
			wg.Wait()
			return
		case <-p.kicks:
			reconcile()
		}
	}
}

func (p *Push) watch(ctx context.Context, login string) {
	backoff := p.backoffMin
	for {
		start := time.Now()
		err := p.streamOnce(ctx, login)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.record(login, "", "", DecisionPushStream, 0, streamFault(err))
		}
		var wait time.Duration
		wait, backoff = p.nextBackoff(backoff, time.Since(start))
		jitter := time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
	}
}

// nextBackoff is the wait before the next connect and the backoff after it:
// a stream that lived healthy starts again from backoffMin; otherwise the
// wait doubles up to backoffMax.
func (p *Push) nextBackoff(backoff, lived time.Duration) (wait, next time.Duration) {
	if lived >= p.healthy {
		backoff = p.backoffMin
	}
	return backoff, min(backoff*2, p.backoffMax)
}

var (
	errPushSession = errors.New("the hub refused the login's session")
	errPushIdle    = errors.New("the events stream went idle")
)

func streamFault(err error) string {
	switch {
	case errors.Is(err, errPushSession):
		return "session"
	case errors.Is(err, errPushIdle):
		return "idle"
	}
	return "connect"
}

func (p *Push) streamClient() *http.Client {
	if p.g.cfg.DialContext != nil {
		return &http.Client{Transport: jailTransport(p.g.cfg.DialContext)}
	}
	return &http.Client{}
}

// streamOnce runs one connection until it ends.
func (p *Push) streamOnce(ctx context.Context, login string) error {
	g := p.g
	cookie, err := g.cfg.Session.Cookie(ctx, login)
	if err != nil {
		return errPushSession
	}
	uid, cookie, err := g.hubUserID(ctx, login, cookie)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	u := g.cfg.Target.JoinPath("/events")
	u.RawQuery = url.Values{"sub": {"user." + uid + ".chat.>"}}.Encode()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.streamClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sessionRejected(resp) {
		g.cfg.Session.Invalidate(login, cookie)
		return errPushSession
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		return fmt.Errorf("hub events: HTTP %d", resp.StatusCode)
	}
	s := &pushSession{uid: uid, cookie: cookie, tier: g.viewerFor(login).tier, end: func() { cancel(errPushSession) }}
	sched := newPushScheduler(sctx, p, login, s)
	s.schedule = func(t pushTarget, after time.Duration) { sched.add(t.key, after, false) }
	schedDone := make(chan struct{})
	go func() { sched.run(); close(schedDone) }()
	// The stream ends only after its checks: a shutdown leaves no check
	// running once Run returns.
	defer func() { cancel(nil); <-schedDone }()
	sched.add("", 0, true) // catch-up: the stream is open
	idle := time.AfterFunc(p.idle, func() { cancel(errPushIdle) })
	defer idle.Stop()
	rd := &eventReducer{src: bufio.NewReaderSize(resp.Body, 64<<10)}
	subject := "user." + uid + ".chat.dm"
	for {
		lines, err := rd.nextEvent()
		if err != nil {
			if s.stale.Load() {
				g.cfg.Session.Invalidate(login, cookie)
				return errPushSession
			}
			if c := context.Cause(sctx); c != nil && !errors.Is(c, context.Canceled) {
				return c
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		idle.Reset(p.idle)
		if s.stale.Load() {
			g.cfg.Session.Invalidate(login, cookie)
			return errPushSession
		}
		if key, ok := dmEventKey(lines, subject); ok {
			sched.add(key, 0, false)
		}
	}
}

// dmEventKey reads one event: a DM update for this login, and the DM key
// it names ("" = check every DM). Nothing else in the event is read. The
// hub publishes only message events on user.<id>.chat.dm (a
// UserMessageEvent; edits and deletes go to chat.message.edited and
// .deleted), and a message names its DM in threadId.
func dmEventKey(lines []string, subject string) (string, bool) {
	var event, data string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "event: "):
			event = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			data = strings.TrimPrefix(l, "data: ")
		}
	}
	if event != "update" {
		return "", false
	}
	var in struct {
		Subject string `json:"subject"`
		Data    struct {
			ThreadID string `json:"threadId"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(data), &in) != nil || in.Subject != subject {
		return "", false
	}
	return in.Data.ThreadID, true
}

// pushScheduler runs one login's checks, one at a time. key "" means every
// target; catchUp marks a pass after a connect. A check that decided
// nothing (the hub or the store failed) runs again, with backoff, at most
// maxCheckRetries times, and keeps its catch-up flag: a message stored in
// a reconnect gap is found only by that pass.
type pushScheduler struct {
	ctx   context.Context
	p     *Push
	login string
	s     *pushSession
	mu    sync.Mutex
	due   map[string]time.Time
	catch map[string]bool
	tries map[string]int
	wake  chan struct{}
}

const maxCheckRetries = 5

func newPushScheduler(ctx context.Context, p *Push, login string, s *pushSession) *pushScheduler {
	return &pushScheduler{ctx: ctx, p: p, login: login, s: s, due: map[string]time.Time{}, catch: map[string]bool{},
		tries: map[string]int{}, wake: make(chan struct{}, 1)}
}

// retry schedules key again after a failed check, or gives up after
// maxCheckRetries (audited; the next event or connect checks it again).
func (q *pushScheduler) retry(key string, catchUp bool) {
	q.mu.Lock()
	q.tries[key]++
	n := q.tries[key]
	if n > maxCheckRetries {
		delete(q.tries, key)
	}
	q.mu.Unlock()
	if n > maxCheckRetries {
		q.p.record(q.login, "", "", DecisionPushFailed, 0, "retries")
		return
	}
	q.add(key, min(q.p.backoffMin<<(n-1), q.p.backoffMax), catchUp)
}

func (q *pushScheduler) done(key string) {
	q.mu.Lock()
	delete(q.tries, key)
	q.mu.Unlock()
}

// add asks for a check of key after the debounce (or after, if longer).
func (q *pushScheduler) add(key string, after time.Duration, catchUp bool) {
	at := time.Now().Add(max(after, q.p.debounce))
	q.mu.Lock()
	if d, ok := q.due[key]; !ok || at.Before(d) {
		q.due[key] = at
	}
	q.catch[key] = q.catch[key] || catchUp
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *pushScheduler) run() {
	for {
		q.mu.Lock()
		var next time.Time
		for _, d := range q.due {
			if next.IsZero() || d.Before(next) {
				next = d
			}
		}
		q.mu.Unlock()
		var timer <-chan time.Time
		if !next.IsZero() {
			timer = time.After(time.Until(next))
		}
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
			continue
		case <-timer:
		}
		q.runDue()
	}
}

func (q *pushScheduler) runDue() {
	now := time.Now()
	q.mu.Lock()
	keys, catch := map[string]bool{}, map[string]bool{}
	for k, d := range q.due {
		if !d.After(now) {
			keys[k], catch[k] = true, q.catch[k]
			delete(q.due, k)
			delete(q.catch, k)
		}
	}
	q.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	targets, err := q.p.g.pushTargets(q.ctx, q.login, q.s.uid)
	if err != nil {
		if q.ctx.Err() == nil {
			for k := range keys {
				q.retry(k, catch[k])
			}
		}
		return
	}
	q.done("")
	// A key that names none of the targets (a spelling the hub changed)
	// checks every DM: dropped, its message would wait for a reconnect.
	for k := range keys {
		if k != "" && !slices.ContainsFunc(targets, func(t pushTarget) bool { return t.key == k }) {
			q.p.record(q.login, "", "", DecisionPushStream, 0, "unknown-dm")
			keys[""], catch[""] = true, catch[""] || catch[k]
		}
	}
	for _, t := range targets {
		if q.ctx.Err() != nil {
			return
		}
		if !keys[""] && !keys[t.key] {
			continue
		}
		c := catch[""] || catch[t.key]
		switch err := q.p.check(q.ctx, q.login, t, q.s, c); {
		case err == nil:
			q.done(t.key)
		case errors.Is(err, errCheck) && q.ctx.Err() == nil:
			q.retry(t.key, c)
		}
	}
}
