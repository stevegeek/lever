package remoteproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// sseHub is a hub whose /events the test drives.
type sseHub struct {
	mu       sync.Mutex
	streams  int
	subs     []string
	cookies  []string
	reject   int // answer the next n /events with 401
	events   chan string
	closeAll chan struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{events: make(chan string, 16), closeAll: make(chan struct{})}
}

func (s *sseHub) route(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/events" {
		return false
	}
	s.mu.Lock()
	s.streams++
	s.subs = append(s.subs, r.URL.Query().Get("sub"))
	s.cookies = append(s.cookies, r.Header.Get("Cookie"))
	if s.reject > 0 {
		s.reject--
		s.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	closeAll := s.closeAll
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	w.(http.Flusher).Flush()
	for {
		select {
		case e := <-s.events:
			fmt.Fprint(w, e)
			w.(http.Flusher).Flush()
		case <-closeAll:
			return true
		case <-r.Context().Done():
			return true
		}
	}
}

func (s *sseHub) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.streams }

// dropAll ends every open stream (a hub restart).
func (s *sseHub) dropAll() {
	s.mu.Lock()
	close(s.closeAll)
	s.closeAll = make(chan struct{})
	s.mu.Unlock()
}

func pushDMEvent(uid, key string) string {
	return fmt.Sprintf("id: 1\nevent: update\ndata: {\"subject\":\"user.%s.chat.dm\",\"data\":{\"threadId\":%q,\"msg\":\"SECRET\"}}\n\n", uid, key)
}

type watchEnv struct {
	*triggerEnv
	sse    *sseHub
	cancel context.CancelFunc
	done   chan struct{}
}

func startWatch(t *testing.T, before func(e *watchEnv)) *watchEnv {
	t.Helper()
	page := newPageHub(t)
	e := &watchEnv{triggerEnv: &triggerEnv{fs: &fakeSender{}, hub: &pushDMHub{}, lines: &lockedLines{}, now: time.Now()}, sse: newSSEHub()}
	page.route = func(w http.ResponseWriter, r *http.Request) bool { return e.sse.route(w, r) || e.hub.route(w, r) }
	cfg, p := pushConfig(t, page, e.fs, e.lines)
	e.g = NewHandler(cfg).(*gate)
	e.p = p
	p.backoffMin, p.backoffMax, p.idle, p.healthy, p.debounce = 10*time.Millisecond, 50*time.Millisecond, 500*time.Millisecond, time.Second, 10*time.Millisecond
	if before != nil {
		before(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() { p.Run(ctx); close(e.done) }()
	t.Cleanup(func() { cancel(); <-e.done })
	return e
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRunRestoresWatchersFromTheStore: a proxy restart with stored
// subscriptions reconnects every subscribed login, with its own session.
func TestRunRestoresWatchersFromTheStore(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	if e.sse.subs[0] != "user."+chatUID+".chat.>" || e.sse.cookies[0] != sessionCookieName+"="+testCookie {
		t.Fatalf("sub %q cookie %q", e.sse.subs[0], e.sse.cookies[0])
	}
}

func TestWatchPushesOnADMEvent(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	time.Sleep(50 * time.Millisecond) // the catch-up pass sets the baseline
	e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	e.sse.events <- pushDMEvent(chatUID, key)
	eventually(t, "a push", func() bool { return len(e.fs.all()) == 1 })
	if !strings.HasSuffix(e.fs.all()[0], `{"v":1,"agent":"w1"}`) {
		t.Fatalf("payload %v", e.fs.all())
	}
}

func TestWatchIgnoresOtherSubjects(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	e.hub.mu.Lock()
	before := e.hub.reads
	e.hub.mu.Unlock()
	for _, ev := range []string{
		"event: update\ndata: {\"subject\":\"user." + chatUID + ".chat.read-state\",\"data\":{}}\n\n",
		"event: update\ndata: {\"subject\":\"user.someone-else.chat.dm\",\"data\":{}}\n\n",
		":heartbeat 1\n\n",
	} {
		e.sse.events <- ev
	}
	time.Sleep(100 * time.Millisecond)
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	if e.hub.reads != before {
		t.Fatalf("history read %d times for events that are not this login's DMs", e.hub.reads-before)
	}
}

// TestWatchCatchesUpAfterReconnect is Review Focus 1.
func TestWatchCatchesUpAfterReconnect(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	e.sse.dropAll()                                                                            // hub restart
	e.hub.put("dm:agent:"+agentW1+":user:"+chatUID, agentMsg("m1", agentW1, "w1", time.Now())) // stored during the gap
	eventually(t, "a reconnect", func() bool { return e.sse.count() >= 2 })
	eventually(t, "the catch-up push", func() bool { return len(e.fs.all()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if len(e.fs.all()) != 1 {
		t.Fatal("pushed twice")
	}
}

// TestWatchRenewsARejectedSession is Review Focus 1.
func TestWatchRenewsARejectedSession(t *testing.T) {
	var sess *stubSession
	e := startWatch(t, func(e *watchEnv) {
		e.sse.reject = 1
		sess = e.g.cfg.Session.(*stubSession)
		e.p.store.Add(chatOp, sub("op"))
	})
	eventually(t, "a second stream", func() bool { return e.sse.count() >= 2 })
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.invalidated) == 0 {
		t.Fatal("the rejected session was not invalidated")
	}
}

func TestWatchReconnectsWhenIdle(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "an idle reconnect", func() bool { return e.sse.count() >= 2 }) // idle = 500ms, nothing sent
}

func TestWatchStopsWhenTheLastSubscriptionGoes(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")) })
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	e.p.unsubscribe(chatOp, sub("op").Endpoint)
	time.Sleep(300 * time.Millisecond)
	n := e.sse.count()
	time.Sleep(700 * time.Millisecond) // past idle and backoff: no new stream
	if e.sse.count() != n {
		t.Fatal("a watcher kept reconnecting without a subscription")
	}
}

func TestRunEndsOnShutdown(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) { e.p.store.Add(chatOp, sub("op")); e.p.store.Add("c@x", sub("c")) })
	eventually(t, "two streams", func() bool { return e.sse.count() == 2 })
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestWatchContactGetsNoPushForUnrecordedRows(t *testing.T) {
	var asked atomic.Int32
	e := startWatch(t, func(e *watchEnv) {
		e.g.cfg.MatchAgentMessages = func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
			return map[string]bool{}, nil
		}
		e.g.cfg.PeekAgentMessages = func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error) {
			asked.Add(1)
			return map[string]bool{}, map[string]bool{}, nil
		}
		e.p.store.Add("c@x", sub("c"))
	})
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	key := "dm:agent:" + agentW1 + ":user:" + chatUID // the page hub names every login chatUID
	e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	e.sse.events <- pushDMEvent(chatUID, key)
	eventually(t, "the matcher question", func() bool { return asked.Load() > 0 })
	time.Sleep(100 * time.Millisecond)
	if len(e.fs.all()) != 0 {
		t.Fatal("a contact was pushed about an unrecorded agent row")
	}
}

func TestDMEventKey(t *testing.T) {
	subj := "user.u1.chat.dm"
	for name, tc := range map[string]struct {
		lines []string
		key   string
		ok    bool
	}{
		"message": {[]string{"id: 3", "event: update", `data: {"subject":"user.u1.chat.dm","data":{"threadId":"dm:agent:a:user:u1"}}`}, "dm:agent:a:user:u1", true},
		"edit":    {[]string{"event: update", `data: {"subject":"user.u1.chat.dm","data":{"conversationKey":"dm:agent:a:user:u1"}}`}, "dm:agent:a:user:u1", true},
		"no key":  {[]string{"event: update", `data: {"subject":"user.u1.chat.dm","data":{}}`}, "", true},
		"other":   {[]string{"event: update", `data: {"subject":"user.u1.chat.dm.promoted","data":{}}`}, "", false},
		"bad":     {[]string{"event: update", `data: {`}, "", false},
		"beat":    {[]string{":heartbeat 5"}, "", false},
	} {
		key, ok := dmEventKey(tc.lines, subj)
		if key != tc.key || ok != tc.ok {
			t.Errorf("%s: %q %v", name, key, ok)
		}
	}
}

func (e *watchEnv) reads() int {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	return e.hub.reads
}

// TestWatchRetriesAFailedCatchUp: a message stored during a gap is found
// only by the catch-up after the connect; a failed read there is retried.
func TestWatchRetriesAFailedCatchUp(t *testing.T) {
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.p.store.Add(chatOp, sub("op"))
		e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: time.Now().Add(-time.Minute)})
		e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
		e.hub.histErr, e.hub.histErrTimes = http.StatusBadGateway, 2 // boss and w1 fail once
	})
	eventually(t, "the push after the retry", func() bool { return len(e.fs.all()) == 1 })
}

// TestWatchRetryKeepsTheCatchUpFlag: a retried catch-up is still a catch-up:
// a DM seen for the first time sets the mark and pushes nothing.
func TestWatchRetryKeepsTheCatchUpFlag(t *testing.T) {
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.p.store.Add(chatOp, sub("op"))
		e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
		e.hub.histErr, e.hub.histErrTimes = http.StatusBadGateway, 2
	})
	eventually(t, "the baseline mark", func() bool { m, ok := e.p.store.Mark(chatOp, "w1"); return ok && m.ID == "m1" })
	time.Sleep(100 * time.Millisecond)
	if len(e.fs.all()) != 0 {
		t.Fatal("a retried catch-up pushed a row that was there before the first connect")
	}
}

func TestWatchRetriesAreBounded(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.p.store.Add(chatOp, sub("op"))
		e.hub.histErr = http.StatusBadGateway
	})
	// Four targets (boss, w1, w2, w3), each read once and retried five times.
	eventually(t, "every retry", func() bool { return e.reads() == 4*(1+maxCheckRetries) })
	time.Sleep(300 * time.Millisecond)
	if n := e.reads(); n != 4*(1+maxCheckRetries) {
		t.Fatalf("%d history reads: the retries do not stop", n)
	}
	b, _ := json.Marshal(e.lines.all())
	if !strings.Contains(string(b), `"retries"`) {
		t.Fatalf("no audit line for the given-up check: %s", b)
	}
}

// TestWatchRejectedSessionReconnectsAndCatchesUp: a history read the hub
// refuses (401) ends the stream at once; the next connect logs in again and
// its catch-up pushes the message.
func TestWatchRejectedSessionReconnectsAndCatchesUp(t *testing.T) {
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	var sess *stubSession
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute // only the rejected session may end the stream
		sess = e.g.cfg.Session.(*stubSession)
		e.p.store.Add(chatOp, sub("op"))
		e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: time.Now().Add(-time.Minute)})
	})
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	e.hub.mu.Lock()
	e.hub.histErr, e.hub.histErrTimes = http.StatusUnauthorized, 1
	e.hub.mu.Unlock()
	e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	e.sse.events <- pushDMEvent(chatUID, key)
	eventually(t, "a reconnect", func() bool { return e.sse.count() >= 2 })
	eventually(t, "the catch-up push", func() bool { return len(e.fs.all()) == 1 })
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.invalidated) == 0 {
		t.Fatal("the rejected session was not invalidated")
	}
}

// TestWatchHeartbeatsKeepTheStream: any event or heartbeat resets the idle
// timer.
func TestWatchHeartbeatsKeepTheStream(t *testing.T) {
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = 300 * time.Millisecond
		e.p.store.Add(chatOp, sub("op"))
	})
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	for range 9 {
		e.sse.events <- ":heartbeat 1\n\n"
		time.Sleep(100 * time.Millisecond)
	}
	if n := e.sse.count(); n != 1 {
		t.Fatalf("%d streams: a beating stream was dropped as idle", n)
	}
}

// TestRunWaitsForInFlightSends: Run returns only after every watcher, and
// each watcher only after its checks, so a shutdown never cuts a send.
func TestRunWaitsForInFlightSends(t *testing.T) {
	entered := make(chan struct{}, 1)
	var finished atomic.Bool
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.p.store.Add(chatOp, sub("op"))
		e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: time.Now().Add(-time.Minute)})
		e.fs.hook = func(context.Context) {
			entered <- struct{}{}
			time.Sleep(200 * time.Millisecond)
			finished.Store(true)
		}
	})
	eventually(t, "a stream", func() bool { return e.sse.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	e.sse.events <- pushDMEvent(chatUID, key)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no send")
	}
	e.cancel()
	<-e.done
	if !finished.Load() {
		t.Fatal("Run returned while a send was under way")
	}
}

func TestNextBackoff(t *testing.T) {
	p := &Push{backoffMin: time.Second, backoffMax: 8 * time.Second, healthy: time.Minute}
	for _, tc := range []struct{ cur, lived, wait, next time.Duration }{
		{time.Second, 0, time.Second, 2 * time.Second},
		{4 * time.Second, 0, 4 * time.Second, 8 * time.Second},
		{8 * time.Second, 0, 8 * time.Second, 8 * time.Second},
		{8 * time.Second, 2 * time.Minute, time.Second, 2 * time.Second},
	} {
		if w, n := p.nextBackoff(tc.cur, tc.lived); w != tc.wait || n != tc.next {
			t.Errorf("nextBackoff(%v, %v) = %v, %v; want %v, %v", tc.cur, tc.lived, w, n, tc.wait, tc.next)
		}
	}
}

// TestSchedulerKeepsACatchUpFlag: an event for a key that a catch-up pass
// already waits for does not turn that pass into a plain check.
func TestSchedulerKeepsACatchUpFlag(t *testing.T) {
	q := newPushScheduler(context.Background(), &Push{debounce: time.Millisecond}, chatOp, &pushSession{})
	q.add("", 0, true)
	q.add("", 0, false)
	q.add("k", 0, false)
	if !q.catch[""] || q.catch["k"] {
		t.Fatalf("catch %v", q.catch)
	}
}

// failingRecords makes the agent records fail n times, then answer as rec.
func failingRecords(n int, rec func(context.Context) (map[string]AgentRecord, error)) func(context.Context) (map[string]AgentRecord, error) {
	var left atomic.Int32
	left.Store(int32(n))
	return func(ctx context.Context) (map[string]AgentRecord, error) {
		if left.Add(-1) >= 0 {
			return nil, fmt.Errorf("records down")
		}
		return rec(ctx)
	}
}

// TestWatchRetriesWhenTheTargetsFail: the catch-up pass cannot list the
// login's agents (the hub's records fail) and is tried again.
func TestWatchRetriesWhenTheTargetsFail(t *testing.T) {
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.g.cfg.AgentRecords = failingRecords(2, e.g.cfg.AgentRecords)
		e.p.store.Add(chatOp, sub("op"))
		e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: time.Now().Add(-time.Minute)})
		e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	})
	eventually(t, "the push after the targets came back", func() bool { return len(e.fs.all()) == 1 })
}

// TestWatchTargetsRetryKeepsTheCatchUpFlag: retried after a targets
// failure, the pass is still a catch-up: a first sight sets the mark only.
func TestWatchTargetsRetryKeepsTheCatchUpFlag(t *testing.T) {
	key := "dm:agent:" + agentW1 + ":user:" + chatUID
	e := startWatch(t, func(e *watchEnv) {
		e.p.idle = time.Minute
		e.g.cfg.AgentRecords = failingRecords(2, e.g.cfg.AgentRecords)
		e.p.store.Add(chatOp, sub("op"))
		e.hub.put(key, agentMsg("m1", agentW1, "w1", time.Now()))
	})
	eventually(t, "the baseline mark", func() bool { m, ok := e.p.store.Mark(chatOp, "w1"); return ok && m.ID == "m1" })
	time.Sleep(100 * time.Millisecond)
	if len(e.fs.all()) != 0 {
		t.Fatal("a retried catch-up pushed a row that was there before the connect")
	}
}

// TestSchedulerRetryBackoffDoubles: each retry of a key waits twice the
// last, from backoffMin up to backoffMax.
func TestSchedulerRetryBackoffDoubles(t *testing.T) {
	p := &Push{debounce: time.Millisecond, backoffMin: 100 * time.Millisecond, backoffMax: 350 * time.Millisecond}
	q := newPushScheduler(context.Background(), p, chatOp, &pushSession{})
	for i, want := range []time.Duration{100, 200, 350, 350} {
		start := time.Now()
		q.retry("k", false)
		q.mu.Lock()
		got := q.due["k"].Sub(start)
		delete(q.due, "k")
		q.mu.Unlock()
		if d := got - want*time.Millisecond; d < 0 || d > 30*time.Millisecond {
			t.Errorf("retry %d waits %v, want %v", i+1, got, want*time.Millisecond)
		}
	}
}

// TestSchedulerSuccessResetsTheRetryCount: a check that succeeds clears its
// count, so a later run of failures gets the full five retries again.
func TestSchedulerSuccessResetsTheRetryCount(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	e.p.debounce, e.p.backoffMin, e.p.backoffMax = time.Millisecond, time.Millisecond, time.Millisecond
	tg := e.target(t, chatOp, chatUID, "w1")
	q := newPushScheduler(context.Background(), e.p, chatOp, e.session(chatUID, chatledger.TierOperator))
	run := func(fail bool) bool {
		e.hub.mu.Lock()
		if fail {
			e.hub.histErr, e.hub.histErrTimes = http.StatusBadGateway, 1
		}
		e.hub.mu.Unlock()
		q.mu.Lock()
		q.due[tg.key] = time.Now().Add(-time.Millisecond)
		q.mu.Unlock()
		q.runDue()
		q.mu.Lock()
		defer q.mu.Unlock()
		_, again := q.due[tg.key]
		return again
	}
	if !run(true) {
		t.Fatal("a failed check was not retried")
	}
	if run(false) {
		t.Fatal("a successful check was scheduled again")
	}
	for i := range maxCheckRetries {
		if !run(true) {
			t.Fatalf("failure %d after a success was not retried: the count was not reset", i+1)
		}
	}
	if run(true) {
		t.Fatal("the retries did not stop")
	}
}
