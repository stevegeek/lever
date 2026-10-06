package remoteproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
			asked.Add(1)
			return map[string]bool{}, nil
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
