package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/webpush"
)

// pushDMHub answers history and the DM list for the trigger tests.
type pushDMHub struct {
	mu      sync.Mutex
	rows    map[string][]historyRow // conversation key → newest first
	unread  map[string]bool
	histErr int // status to answer history with (0 = 200)
	// histErrTimes, when > 0, is how many history reads get histErr; then
	// the reads answer 200 again.
	histErrTimes int
	reads        int
	itemsKey     bool // answer history rows under "items", not "messages"
}

func (d *pushDMHub) route(w http.ResponseWriter, r *http.Request) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.URL.Path == "/api/v1/chat/dms" {
		var dms []map[string]any
		for k, u := range d.unread {
			dms = append(dms, map[string]any{"conversationKey": k, "hasUnread": u})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dms": dms})
		return true
	}
	if key, ok := strings.CutPrefix(r.URL.Path, "/api/v1/chat/conversations/"); ok && strings.HasSuffix(key, "/messages") {
		d.reads++
		if d.histErr != 0 && d.histErrTimes > 0 {
			d.histErrTimes--
			if d.histErrTimes == 0 {
				st := d.histErr
				d.histErr = 0
				w.WriteHeader(st)
				return true
			}
		}
		if d.histErr != 0 {
			w.WriteHeader(d.histErr)
			return true
		}
		key = strings.TrimSuffix(key, "/messages")
		field := "messages"
		if d.itemsKey {
			field = "items"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{field: d.rows[key]})
		return true
	}
	return false
}

func (d *pushDMHub) put(key string, rows ...historyRow) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rows == nil {
		d.rows, d.unread = map[string][]historyRow{}, map[string]bool{}
	}
	d.rows[key] = append(append([]historyRow{}, rows...), d.rows[key]...)
	d.unread[key] = true
}

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func agentMsg(id, agentID, slug string, at time.Time) historyRow {
	return historyRow{ID: id, Sender: "agent:" + slug, SenderID: agentID, Type: "instruction", Msg: "SECRET-TEXT " + id, CreatedAt: at.Format(time.RFC3339Nano)}
}

type triggerEnv struct {
	p     *Push
	g     *gate
	fs    *fakeSender
	hub   *pushDMHub
	lines *lockedLines
	now   time.Time
	sched []string
	binds atomic.Int32 // calls of the binding matcher
}

// newTriggerEnv: operator op@x (uid u-op) and contact c@x (agents [w1], see [w2]).
func newTriggerEnv(t *testing.T, match func(context.Context, string, string, []AgentMessage) (map[string]bool, error)) *triggerEnv {
	t.Helper()
	page := newPageHub(t)
	e := &triggerEnv{fs: &fakeSender{}, hub: &pushDMHub{}, lines: &lockedLines{}, now: t0}
	page.route = e.hub.route
	cfg, p := pushConfig(t, page, e.fs, e.lines)
	if match != nil {
		// match answers both questions; the binding one also counts, so a
		// test sees whether a push check bound anything.
		cfg.MatchAgentMessages = func(ctx context.Context, c, a string, msgs []AgentMessage) (map[string]bool, error) {
			e.binds.Add(1)
			return match(ctx, c, a, msgs)
		}
		cfg.PeekAgentMessages = func(ctx context.Context, c, a string, msgs []AgentMessage) (map[string]bool, map[string]bool, error) {
			keep, err := match(ctx, c, a, msgs)
			return keep, map[string]bool{}, err
		}
	}
	h := NewHandler(cfg).(*gate)
	e.p, e.g = p, h
	p.now = func() time.Time { return e.now }
	return e
}

func (e *triggerEnv) session(uid, tier string) *pushSession {
	return &pushSession{uid: uid, cookie: testCookie, tier: tier, schedule: func(t pushTarget, after time.Duration) {
		e.sched = append(e.sched, fmt.Sprintf("%s %s", t.name, after))
	}}
}

func (e *triggerEnv) target(t *testing.T, login, uid, name string) pushTarget {
	t.Helper()
	ts, err := e.g.pushTargets(context.Background(), login, uid)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.name == name {
			return x
		}
	}
	t.Fatalf("no target %s for %s in %v", name, login, ts)
	return pushTarget{}
}

func TestPushTargetsAreTheLoginsMessageAgents(t *testing.T) {
	e := newTriggerEnv(t, nil)
	names := func(login, uid string) string {
		ts, _ := e.g.pushTargets(context.Background(), login, uid)
		var n []string
		for _, x := range ts {
			n = append(n, x.name+"="+x.key)
		}
		return strings.Join(n, ",")
	}
	if got := names("c@x", "u-c"); got != "w1=dm:agent:"+agentW1+":user:u-c" {
		t.Fatalf("contact targets %s (see-only w2 and hidden agents must be absent)", got)
	}
	if got := names(chatOp, chatUID); !strings.Contains(got, "boss=") || !strings.Contains(got, "w3=") {
		t.Fatalf("operator targets %s", got)
	}
}

func TestCheckPushesANewAgentRowWithoutText(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
	sent := e.fs.all()
	if len(sent) != 1 || !strings.HasSuffix(sent[0], ` {"v":1,"agent":"w1"}`) || strings.Contains(sent[0], "SECRET") {
		t.Fatalf("sent %v", sent)
	}
	if m, _ := e.p.store.Mark(chatOp, "w1"); m.ID != "m1" {
		t.Fatalf("mark %+v", m)
	}
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
	if len(e.fs.all()) != 1 {
		t.Fatal("the same row was pushed twice")
	}
}

func TestCheckIgnoresOwnAndHubRows(t *testing.T) {
	for name, row := range map[string]historyRow{
		"own":          {ID: "m1", Sender: "user:op@x", SenderID: chatUID, Type: "instruction", Msg: "hi", CreatedAt: t0.Format(time.RFC3339Nano)},
		"system":       {ID: "m1", Sender: "system", SenderID: "hub", Type: "system", Msg: "x", CreatedAt: t0.Format(time.RFC3339Nano)},
		"state change": {ID: "m1", Sender: "agent:w1", SenderID: agentW1, Type: "state-change", Msg: "x", CreatedAt: t0.Format(time.RFC3339Nano)},
		"deleted":      {ID: "m1", Sender: "agent:w1", SenderID: agentW1, Type: "instruction", Msg: "", CreatedAt: t0.Format(time.RFC3339Nano)},
		"other agent":  {ID: "m1", Sender: "agent:w2", SenderID: "id-w2", Type: "instruction", Msg: "x", CreatedAt: t0.Format(time.RFC3339Nano)},
		"bad time":     {ID: "m1", Sender: "agent:w1", SenderID: agentW1, Type: "instruction", Msg: "x", CreatedAt: "yesterday"},
		// The agent's id with a person's sender: only an agent sender pushes.
		"user sender": {ID: "m1", Sender: "user:x@y", SenderID: agentW1, Type: "instruction", Msg: "x", CreatedAt: t0.Format(time.RFC3339Nano)},
	} {
		e := newTriggerEnv(t, nil)
		e.p.store.Add(chatOp, sub("op"))
		tg := e.target(t, chatOp, chatUID, "w1")
		e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Hour)})
		e.hub.put(tg.key, row)
		e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
		if len(e.fs.all()) != 0 {
			t.Errorf("%s: pushed", name)
		}
	}
}

// The hub answers history under "messages": rows under any other key push
// nothing, as the contact's history filter shows nothing.
func TestCheckIgnoresRowsUnderItems(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.hub.itemsKey = true
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
	if sent := e.fs.all(); len(sent) != 0 {
		t.Fatalf("sent %v", sent)
	}
}

func TestCheckContactOnlyForRowsTheMatcherShows(t *testing.T) {
	keep := map[string]bool{}
	var down bool
	e := newTriggerEnv(t, func(_ context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error) {
		if down {
			return nil, errors.New("broker down")
		}
		out := map[string]bool{}
		for _, m := range msgs {
			if keep[m.ID] {
				out[m.ID] = true
			}
		}
		return out, nil
	})
	e.p.store.Add("c@x", sub("c"))
	tg := e.target(t, "c@x", "u-c", "w1")
	s := e.session("u-c", chatledger.TierContact)
	e.p.store.SetMark("c@x", "w1", PushMark{ID: "m0", At: t0.Add(-time.Hour)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), "c@x", tg, s, false)
	if len(e.fs.all()) != 0 {
		t.Fatal("a contact got a push for an unrecorded agent row")
	}
	keep["m1"] = true
	e.p.check(context.Background(), "c@x", tg, s, false)
	if len(e.fs.all()) != 1 {
		t.Fatalf("recorded row: %v", e.fs.all())
	}
}

// TestCheckMatcherDownThenUp is Review Focus 4.
func TestCheckMatcherDownThenUp(t *testing.T) {
	down := true
	e := newTriggerEnv(t, func(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, error) {
		if down {
			return nil, errors.New("broker down")
		}
		return map[string]bool{msgs[0].ID: true}, nil
	})
	e.p.store.Add("c@x", sub("c"))
	tg := e.target(t, "c@x", "u-c", "w1")
	s := e.session("u-c", chatledger.TierContact)
	e.p.store.SetMark("c@x", "w1", PushMark{ID: "m0", At: t0.Add(-time.Hour)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), "c@x", tg, s, false)
	if len(e.fs.all()) != 0 {
		t.Fatal("pushed while the matcher could not answer")
	}
	if m, _ := e.p.store.Mark("c@x", "w1"); m.ID != "m0" {
		t.Fatal("the mark moved on a failed check: the row would never be pushed")
	}
	down = false
	e.p.check(context.Background(), "c@x", tg, s, false)
	if len(e.fs.all()) != 1 {
		t.Fatal("no push once the matcher answered")
	}
}

func TestCheckCoalescesPerLoginAndAgent(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	s := e.session(chatUID, chatledger.TierOperator)
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Hour)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), chatOp, tg, s, false)
	e.now = t0.Add(20 * time.Second)
	e.hub.put(tg.key, agentMsg("m2", agentW1, "w1", e.now))
	e.p.check(context.Background(), chatOp, tg, s, false)
	if len(e.fs.all()) != 1 || len(e.sched) != 1 || e.sched[0] != "w1 40s" {
		t.Fatalf("sent %v, scheduled %v", e.fs.all(), e.sched)
	}
	e.now = t0.Add(61 * time.Second)
	e.p.check(context.Background(), chatOp, tg, s, false)
	if len(e.fs.all()) != 2 {
		t.Fatal("the trailing push after the window did not happen")
	}
}

func TestCheckCatchUp(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	s := e.session(chatUID, chatledger.TierOperator)
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), chatOp, tg, s, true) // first sight: baseline only
	if len(e.fs.all()) != 0 {
		t.Fatal("first catch-up pushed an existing row")
	}
	if m, ok := e.p.store.Mark(chatOp, "w1"); !ok || m.ID != "m1" {
		t.Fatal("no baseline mark")
	}
	e.now = t0.Add(10 * time.Minute)
	e.hub.put(tg.key, agentMsg("m2", agentW1, "w1", t0.Add(5*time.Minute)))
	e.p.check(context.Background(), chatOp, tg, s, true) // a row from the gap, recent: push
	if len(e.fs.all()) != 1 {
		t.Fatal("a recent row from the reconnect gap was not pushed")
	}
}

func TestCheckCatchUpEmptyDMThenRow(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	s := e.session(chatUID, chatledger.TierOperator)
	e.p.check(context.Background(), chatOp, tg, s, true) // connect: the DM is empty
	if _, ok := e.p.store.Mark(chatOp, "w1"); !ok {
		t.Fatal("an empty DM got no mark on connect")
	}
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0)) // stored during a reconnect gap
	e.p.check(context.Background(), chatOp, tg, s, true)
	if len(e.fs.all()) != 1 {
		t.Fatal("the first message of a DM that was empty at connect was not pushed")
	}
}

// TestCheckCatchUpSkipsOldRows is Review Focus 5.
func TestCheckCatchUpSkipsOldRows(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-3 * time.Hour)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0.Add(-2*time.Hour)))
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), true)
	if len(e.fs.all()) != 0 {
		t.Fatal("a two-hour-old row was pushed after a restart")
	}
	if m, _ := e.p.store.Mark(chatOp, "w1"); m.ID != "m1" {
		t.Fatal("the mark did not move past the old row")
	}
}

func TestCheckMarksBeforeSending(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.fs.err = func(webpush.Subscription) error {
		if m, _ := e.p.store.Mark(chatOp, "w1"); m.ID != "m1" {
			t.Error("sent before the mark was stored")
		}
		return nil
	}
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
}

func TestCheckNoPushWhenRead(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.hub.unread[tg.key] = false // the page is open and marked it read
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
	if len(e.fs.all()) != 0 {
		t.Fatal("pushed a row the login has read")
	}
}

func TestNotifyDeletesGoneAndKeepsFailed(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("gone"))
	e.p.store.Add(chatOp, sub("busy"))
	e.fs.err = func(s webpush.Subscription) error {
		if strings.HasSuffix(s.Endpoint, "/gone") {
			return &webpush.HTTPError{Status: 410}
		}
		return &webpush.HTTPError{Status: 500}
	}
	e.p.notify(context.Background(), chatOp, "w1")
	subs := e.p.store.Subs(chatOp)
	if len(subs) != 1 || !strings.HasSuffix(subs[0].Endpoint, "/busy") {
		t.Fatalf("subs %v", subs)
	}
	b, _ := json.Marshal(e.lines.all())
	if !strings.Contains(string(b), string(DecisionPushGone)) || !strings.Contains(string(b), string(DecisionPushFailed)) ||
		strings.Contains(string(b), "/fcm/send/") {
		t.Fatalf("audit %s", b)
	}
	sum := ReadPushSummary(e.p.dir)
	if sum.Last == nil || sum.Last.Host != "fcm.googleapis.com" {
		t.Fatalf("status %+v", sum.Last)
	}
}

func TestCheckHistory401MarksTheSessionStale(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.hub.histErr = http.StatusUnauthorized
	s := e.session(chatUID, chatledger.TierOperator)
	e.p.check(context.Background(), chatOp, tg, s, false)
	if !s.stale.Load() {
		t.Fatal("a rejected session was not reported to the watcher")
	}
}

func TestCheckSkipsADuplicatedID(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0), agentMsg("m1", agentW1, "w1", t0.Add(time.Second)))
	e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), false)
	if len(e.fs.all()) != 0 {
		t.Fatal("a row whose id appears twice on the page was pushed")
	}
}

// TestCheckSendOutlivesTheStream: the mark is stored before the send, so a
// send cancelled with the stream (idle drop, hub restart) would be a lost
// push. The send gets a context the stream's end does not cancel.
func TestCheckSendOutlivesTheStream(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.p.store.SetMark(chatOp, "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fs.hook = func(c context.Context) {
		cancel() // the stream ends while the send is under way
		if c.Err() != nil {
			t.Error("the send was cancelled with the stream")
		}
		if _, ok := c.Deadline(); !ok {
			t.Error("the send has no time limit")
		}
	}
	if err := e.p.check(ctx, chatOp, tg, e.session(chatUID, chatledger.TierOperator), false); err != nil {
		t.Fatal(err)
	}
	if len(e.fs.all()) != 1 {
		t.Fatal("no send")
	}
}

func TestCheckReportsWhatToRetry(t *testing.T) {
	e := newTriggerEnv(t, nil)
	e.p.store.Add(chatOp, sub("op"))
	tg := e.target(t, chatOp, chatUID, "w1")
	e.hub.histErr = http.StatusBadGateway
	if err := e.p.check(context.Background(), chatOp, tg, e.session(chatUID, chatledger.TierOperator), true); !errors.Is(err, errCheck) {
		t.Fatalf("a failed history read: %v, want errCheck", err)
	}
	e.hub.histErr = http.StatusUnauthorized
	ended := false
	s := e.session(chatUID, chatledger.TierOperator)
	s.end = func() { ended = true }
	if err := e.p.check(context.Background(), chatOp, tg, s, true); !errors.Is(err, errSessionUnknown) || !ended {
		t.Fatalf("a rejected session: %v, ended %v", err, ended)
	}
}

// pushLedger binds a record to a message id on Match, never on Peek.
type pushLedger struct {
	mu    sync.Mutex
	bound map[string]bool
}

func (l *pushLedger) match(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for _, m := range msgs {
		l.bound[m.ID] = true
		out[m.ID] = true
	}
	return out, nil
}

func (l *pushLedger) peek(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, map[string]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	keep, pending := map[string]bool{}, map[string]bool{}
	for _, m := range msgs {
		keep[m.ID] = true
		if !l.bound[m.ID] {
			pending[m.ID] = true
		}
	}
	return keep, pending, nil
}

// TestCheckContactBindsNoRecord: a push check for a contact uses the peek,
// so it binds no ledger record; the contact's own read binds later exactly
// as it would have without the push.
func TestCheckContactBindsNoRecord(t *testing.T) {
	led := &pushLedger{bound: map[string]bool{}}
	page := newPageHub(t)
	fs, hub := &fakeSender{}, &pushDMHub{}
	page.route = hub.route
	cfg, p := pushConfig(t, page, fs, nil)
	cfg.MatchAgentMessages, cfg.PeekAgentMessages = led.match, led.peek
	g := NewHandler(cfg).(*gate)
	p.now = func() time.Time { return t0 }
	p.store.Add("c@x", sub("c"))
	ts, err := g.pushTargets(context.Background(), "c@x", "u-c")
	if err != nil || len(ts) != 1 {
		t.Fatalf("%v %v", ts, err)
	}
	p.store.SetMark("c@x", "w1", PushMark{ID: "m0", At: t0.Add(-time.Minute)})
	hub.put(ts[0].key, agentMsg("m1", agentW1, "w1", t0))
	s := &pushSession{uid: "u-c", cookie: testCookie, tier: chatledger.TierContact}
	if err := p.check(context.Background(), "c@x", ts[0], s, false); err != nil {
		t.Fatal(err)
	}
	if len(fs.all()) != 1 {
		t.Fatalf("pushes %v: the peek keeps the row", fs.all())
	}
	if len(led.bound) != 0 {
		t.Fatalf("the push check bound %v", led.bound)
	}
	// The contact's own read binds it, as it would have without the check.
	if _, err := g.keepAgentRows(context.Background(), "c@x", "w1", agentW1, "u-c", []historyRow{agentMsg("m1", agentW1, "w1", t0)}); err != nil || !led.bound["m1"] {
		t.Fatalf("read: %v %v", err, led.bound)
	}
}

func TestCheckContactWithoutPeekFailsClosed(t *testing.T) {
	e := newTriggerEnv(t, func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
		return map[string]bool{"m1": true}, nil
	})
	e.g.cfg.PeekAgentMessages = nil
	e.p.store.Add("c@x", sub("c"))
	tg := e.target(t, "c@x", "u-c", "w1")
	e.p.store.SetMark("c@x", "w1", PushMark{ID: "m0", At: t0.Add(-time.Hour)})
	e.hub.put(tg.key, agentMsg("m1", agentW1, "w1", t0))
	e.p.check(context.Background(), "c@x", tg, e.session("u-c", chatledger.TierContact), false)
	if len(e.fs.all()) != 0 || e.binds.Load() != 0 {
		t.Fatalf("pushes %v, binds %d", e.fs.all(), e.binds.Load())
	}
}
