package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

func TestAgentState(t *testing.T) {
	for _, tc := range []struct {
		rec         AgentRecord
		found, down bool
		state, act  string
	}{
		{AgentRecord{}, false, true, "unknown", ""},
		{AgentRecord{}, false, false, "no-record", ""},
		{AgentRecord{Phase: "running", Activity: "thinking"}, true, false, "running", "working"},
		{AgentRecord{Phase: "running", Activity: "working"}, true, false, "running", "working"},
		{AgentRecord{Phase: "running", Activity: "waiting_for_input"}, true, false, "running", "waiting"},
		{AgentRecord{Phase: "running", Activity: "blocked"}, true, false, "running", "waiting"},
		{AgentRecord{Phase: "running", Activity: "stalled"}, true, false, "running", "idle"},
		{AgentRecord{Phase: "running", Activity: "completed"}, true, false, "running", "idle"},
		{AgentRecord{Phase: "running", Activity: "crashed"}, true, false, "running", ""},
		{AgentRecord{Phase: "running", Activity: "unrecognised"}, true, false, "running", ""},
		{AgentRecord{Phase: "running", ContainerDown: true}, true, false, "error", ""},
		{AgentRecord{Phase: "resumed"}, true, false, "starting", ""},
		{AgentRecord{Phase: "provisioning"}, true, false, "starting", ""},
		{AgentRecord{Phase: "suspended"}, true, false, "suspended", ""},
		{AgentRecord{Phase: "stopping"}, true, false, "stopped", ""},
		{AgentRecord{Phase: "stopped"}, true, false, "stopped", ""},
		{AgentRecord{Phase: "error"}, true, false, "error", ""},
		{AgentRecord{Phase: "unrecognised"}, true, false, "unknown", ""},
		{AgentRecord{Phase: ""}, true, false, "unknown", ""},
		{AgentRecord{Phase: "suspended"}, true, true, "unknown", ""},
	} {
		s, a := agentState(tc.rec, tc.found, tc.down)
		if s != tc.state || a != tc.act {
			t.Errorf("%+v found=%v down=%v: %s/%s, want %s/%s", tc.rec, tc.found, tc.down, s, a, tc.state, tc.act)
		}
	}
}

func TestBuildAgentsOperator(t *testing.T) {
	v := viewer{login: "op@x", tier: chatledger.TierOperator, message: []string{"boss", "w1", "w2"}}
	recs := map[string]AgentRecord{"boss": {ID: "id-mgr", Phase: "running", Activity: "working"}, "w1": {ID: "id-w1", Phase: "suspended"}}
	got := buildAgents(v, "boss", "u-op", recs, false, map[string]string{"w1": "Via Roma 12", "zz": "x"}, nil)
	want := []agentEntry{
		{Name: "boss", Role: "manager", Access: "message", State: "running", Activity: "working", ID: "id-mgr",
			Conversation: "dm:agent:id-mgr:user:u-op", Terminal: "/agents/id-mgr/terminal"},
		{Name: "w1", Role: "worker", Label: "Via Roma 12", Access: "message", State: "suspended", ID: "id-w1",
			Conversation: "dm:agent:id-w1:user:u-op", Terminal: "/agents/id-w1/terminal"},
		{Name: "w2", Role: "worker", Access: "message", State: "no-record"},
	}
	if !reflect.DeepEqual(got.Agents, want) || got.Console != "/agents" || got.Tier != "operator" || got.UserID != "u-op" || got.Login != "op@x" {
		t.Fatalf("got %+v", got)
	}
}

func TestBuildAgentsContact(t *testing.T) {
	v := viewer{login: "c@x", tier: chatledger.TierContact, message: []string{"w1", "w3"}, see: []string{"w2"}}
	recs := map[string]AgentRecord{"boss": {ID: "id-mgr", Phase: "running"}, "w1": {ID: "id-w1", Phase: "running"},
		"w2": {ID: "id-w2", Phase: "running", Activity: "working"}, "w3": {ID: "id-w3", Phase: "suspended"}, "w9": {ID: "id-w9", Phase: "running"}}
	fresh := func(n string) error {
		if n == "w3" {
			return errors.New("not fresh")
		}
		return nil
	}
	got := buildAgents(v, "boss", "u-c", recs, false, nil, fresh)
	want := []agentEntry{
		{Name: "w1", Role: "worker", Access: "message", State: "running", ID: "id-w1", Conversation: "dm:agent:id-w1:user:u-c"},
		{Name: "w3", Role: "worker", Access: "message", State: "not-fresh", ID: "id-w3", Conversation: "dm:agent:id-w3:user:u-c"},
		{Name: "w2", Role: "worker", Access: "see", State: "running"}, // no id, no activity, no conversation
	}
	if !reflect.DeepEqual(got.Agents, want) || got.Console != "" {
		t.Fatalf("got %+v", got)
	}
	// No session check wired: every contact post would be refused, so the
	// list says not fresh too.
	if got := buildAgents(v, "boss", "u-c", recs, false, nil, nil); got.Agents[0].State != "not-fresh" {
		t.Fatalf("nil fresh: %+v", got.Agents[0])
	}
}

// The manager comes first even when a contact may only see it.
func TestBuildAgentsManagerFirst(t *testing.T) {
	v := viewer{login: "c@x", tier: chatledger.TierContact, message: []string{"w1"}, see: []string{"boss", "w2"}}
	got := buildAgents(v, "boss", "u-c", nil, false, nil, func(string) error { return nil })
	var names []string
	for _, a := range got.Agents {
		names = append(names, a.Name+"/"+a.Access)
	}
	if strings.Join(names, ",") != "boss/see,w1/message,w2/see" {
		t.Fatalf("order %v", names)
	}
}

func TestBuildAgentsRefusesABadHubID(t *testing.T) {
	v := viewer{login: "op@x", tier: chatledger.TierOperator, message: []string{"boss"}}
	got := buildAgents(v, "boss", "u-op", map[string]AgentRecord{"boss": {ID: "../x", Phase: "running"}}, false, nil, nil)
	if a := got.Agents[0]; a.ID != "" || a.Conversation != "" || a.Terminal != "" || a.State != "unknown" {
		t.Fatalf("a hub id that cannot be a path segment must not be used: %+v", a)
	}
}

func TestCountUnread(t *testing.T) {
	m := func(id, sender, at string) hubMessage { return hubMessage{ID: id, SenderID: sender, CreatedAt: at} }
	// The hub's order is not trusted: sorted here by time, then id.
	items := []hubMessage{m("m3", "a", "2026-10-06T10:03:00Z"), m("m1", "a", "2026-10-06T10:01:00Z"),
		m("m5", "a", "2026-10-06T10:05:00Z"), m("m2", "a", "2026-10-06T10:02:00Z"), m("m4", "u", "2026-10-06T10:04:00Z")}
	if n := countUnread(items, "a", "m2", false); n != 2 { // m5, m3; m4 is the user's own
		t.Errorf("after m2: %d", n)
	}
	if n := countUnread(items, "a", "", false); n != 4 {
		t.Errorf("never read: %d", n)
	}
	if n := countUnread(items, "a", "gone", true); n != 99 {
		t.Errorf("marker not in a full page: %d, want 99", n)
	}
	if n := countUnread(items, "a", "gone", false); n != 4 {
		t.Errorf("marker not in a short page: %d, want all of the agent's", n)
	}
}

// agentsAs runs the agent list handler as login would reach it.
func agentsAs(t *testing.T, h http.Handler, login string) (*httptest.ResponseRecorder, agentsAnswer) {
	t.Helper()
	rw := chatDo(h, login, "GET", chatAgentsPath)
	var ans agentsAnswer
	if rw.Code == http.StatusOK {
		if err := json.Unmarshal(rw.Body.Bytes(), &ans); err != nil {
			t.Fatalf("answer %s: %v", rw.Body, err)
		}
	}
	return rw, ans
}

func TestAgentsListOperatorOrderAndLinks(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.Labels = func() map[string]string { return map[string]string{"w2": "Via Roma 12", "nobody": "x"} }
	rw, ans := agentsAs(t, NewHandler(cfg), chatOp)
	if rw.Code != 200 {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store",
		"Content-Security-Policy": "sandbox", "X-Content-Type-Options": "nosniff"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	var got []string
	for _, a := range ans.Agents {
		got = append(got, a.Name+"/"+a.Role+"/"+a.Access+"/"+a.State+"/"+a.Label+"/"+a.Terminal)
	}
	want := []string{"boss/manager/message/running//" + "/agents/" + chatMgrID + "/terminal",
		"w1/worker/message/running//" + "/agents/" + agentW1 + "/terminal",
		"w2/worker/message/running/Via Roma 12//agents/id-w2/terminal",
		"w3/worker/message/suspended//" + "/agents/id-w3/terminal"}
	if !reflect.DeepEqual(got, want) || ans.Console != chatConsolePath || ans.UserID != chatUID || ans.Login != chatOp || ans.Tier != "operator" {
		t.Fatalf("got %v\nwant %v\n%+v", got, want, ans)
	}
	if head := chatDo(NewHandler(cfg), chatOp, "HEAD", chatAgentsPath); head.Code != 200 || head.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d bytes", head.Code, head.Body.Len())
	}
}

// TestAgentsListHidesWhatALoginMayNotSee (Review Focus 1): a contact's
// answer names only its own agents.
func TestAgentsListHidesWhatALoginMayNotSee(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub) // contact c@x: agents [w1]; see [w2]; hidden w3; manager boss
	g := NewHandler(cfg).(*gate)
	rw := httptest.NewRecorder()
	line := AuditLine{}
	g.serveAgents(rw, proxyRequest("GET", chatAgentsPath, nil), &line, g.viewerFor("c@x"), testCookie)
	if rw.Code != 200 || rw.Header().Get("Cache-Control") != "no-store" || rw.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("%d %v", rw.Code, rw.Header())
	}
	for _, leak := range []string{"w3", "id-w3", "boss", chatMgrID, "id-w2", "terminal", "console", "/agents"} {
		if strings.Contains(rw.Body.String(), leak) {
			t.Errorf("contact answer leaks %q: %s", leak, rw.Body.String())
		}
	}
	var ans agentsAnswer
	_ = json.Unmarshal(rw.Body.Bytes(), &ans)
	if len(ans.Agents) != 2 || ans.Agents[0].Name != "w1" || ans.Agents[1].Name != "w2" || ans.Agents[1].Access != "see" || ans.Tier != "contact" {
		t.Fatalf("%s", rw.Body)
	}
}

func TestAgentsListHubDown(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) { return nil, errors.New("down") }
	rw, ans := agentsAs(t, NewHandler(cfg), chatOp)
	if rw.Code != 200 || len(ans.Agents) == 0 {
		t.Fatalf("%d %s", rw.Code, rw.Body.String())
	}
	for _, a := range ans.Agents {
		if a.State != "unknown" || a.ID != "" || a.Conversation != "" {
			t.Errorf("hub down: %+v", a)
		}
	}
	cfg.AgentRecords = nil
	if _, ans := agentsAs(t, NewHandler(cfg), chatOp); len(ans.Agents) == 0 || ans.Agents[0].State != "unknown" {
		t.Errorf("no records source: %+v", ans)
	}
	hub.me = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	var lines []AuditLine
	cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
	rw = chatDo(NewHandler(cfg), chatOp, "GET", chatAgentsPath)
	if rw.Code != http.StatusBadGateway || strings.Contains(rw.Body.String(), "{") {
		t.Fatalf("identity down: %d %s, want a bare 502", rw.Code, rw.Body)
	}
	if len(lines) != 1 || lines[0].Decision != DecisionChatUnavailable {
		t.Fatalf("audit %+v", lines)
	}
}

// TestAgentsListHealsALapsedSession: a 401 from the identity lookup
// replaces the session once.
func TestAgentsListHealsALapsedSession(t *testing.T) {
	hub := newPageHub(t)
	hub.me = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != sessionCookieName+"=new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+chatUID+`"}`)
	}
	cfg := chatConfig(t, hub)
	cfg.Session = &rotatingSession{}
	rw, ans := agentsAs(t, NewHandler(cfg), chatOp)
	if rw.Code != http.StatusOK || ans.UserID != chatUID {
		t.Fatalf("%d %s, want the list on the new session", rw.Code, rw.Body)
	}
}

func TestAgentsListFailsClosedOnABadUserID(t *testing.T) {
	for _, body := range []string{`{"id":"u/../x"}`, `{"id":"u:agent:x"}`, `{"id":""}`, `<html>`} {
		hub := newPageHub(t)
		hub.me = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
		if rw := chatDo(NewHandler(chatConfig(t, hub)), chatOp, "GET", chatAgentsPath); rw.Code != http.StatusBadGateway {
			t.Errorf("%s: %d, want 502", body, rw.Code)
		}
	}
}

// dmHub answers the DM list and history reads the unread count needs.
func dmHub(t *testing.T, dms string, history map[string]string) *pageHub {
	hub := newPageHub(t)
	hub.route = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Cookie") != sessionCookieName+"="+testCookie {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		if r.URL.Path == "/api/v1/chat/dms" {
			if dms == "500" {
				w.WriteHeader(500)
				return true
			}
			_, _ = io.WriteString(w, dms)
			return true
		}
		if key, ok := strings.CutPrefix(r.URL.Path, "/api/v1/chat/conversations/"); ok {
			key = strings.TrimSuffix(key, "/messages")
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("history read without limit=100: %s", r.URL)
			}
			if b, ok := history[key]; ok {
				_, _ = io.WriteString(w, b)
				return true
			}
		}
		return false
	}
	return hub
}

func TestAgentsListUnread(t *testing.T) {
	w1Key := "dm:agent:" + agentW1 + ":user:" + chatUID
	mgrKey := "dm:agent:" + chatMgrID + ":user:" + chatUID
	dms := `{"dms":[` +
		`{"conversationKey":"` + w1Key + `","hasUnread":true,"lastReadMessageId":"m2"},` +
		`{"conversationKey":"` + mgrKey + `","hasUnread":false,"lastReadMessageId":"m9"},` +
		`{"conversationKey":"dm:agent:id-w2:user:` + chatUID + `","hasUnread":true}]}`
	history := map[string]string{
		w1Key: `{"messages":[` +
			`{"id":"m1","senderId":"` + agentW1 + `","createdAt":"2026-10-06T10:01:00Z"},` +
			`{"id":"m2","senderId":"` + agentW1 + `","createdAt":"2026-10-06T10:02:00Z"},` +
			`{"id":"m3","senderId":"` + chatUID + `","createdAt":"2026-10-06T10:03:00Z"},` +
			`{"id":"m4","senderId":"` + agentW1 + `","createdAt":"2026-10-06T10:04:00Z"},` +
			`{"id":"m5","senderId":"` + agentW1 + `","createdAt":"2026-10-06T10:05:00Z"}]}`,
		"dm:agent:id-w2:user:" + chatUID: `{"messages":[{"id":"x1","senderId":"id-w2","createdAt":"2026-10-06T10:01:00Z"}]}`,
	}
	hub := dmHub(t, dms, history)
	_, ans := agentsAs(t, NewHandler(chatConfig(t, hub)), chatOp)
	unread := map[string]string{}
	for _, a := range ans.Agents {
		if a.Unread == nil {
			unread[a.Name] = "none"
		} else {
			unread[a.Name] = fmt.Sprint(*a.Unread)
		}
	}
	// boss: read; w1: two after the marker; w2: one, never read; w3: no DM yet.
	if want := map[string]string{"boss": "0", "w1": "2", "w2": "1", "w3": "0"}; !reflect.DeepEqual(unread, want) {
		t.Fatalf("unread %v, want %v", unread, want)
	}

	// A see-only agent gets no unread key, even with an unread DM.
	g := NewHandler(chatConfig(t, hub)).(*gate)
	rw := httptest.NewRecorder()
	g.serveAgents(rw, proxyRequest("GET", chatAgentsPath, nil), &AuditLine{}, g.viewerFor("c@x"), testCookie)
	var c agentsAnswer
	_ = json.Unmarshal(rw.Body.Bytes(), &c)
	if len(c.Agents) != 2 || c.Agents[1].Access != "see" || strings.Count(rw.Body.String(), `"unread"`) != 1 {
		t.Fatalf("contact answer %s", rw.Body)
	}

	// The DM list down: no unread key at all.
	hub = dmHub(t, "500", nil)
	if rw, _ := agentsAs(t, NewHandler(chatConfig(t, hub)), chatOp); strings.Contains(rw.Body.String(), "unread") {
		t.Fatalf("DM list down: %s", rw.Body)
	}
	// A DM list without its key is not "nothing unread".
	hub = dmHub(t, `{}`, nil)
	if rw, _ := agentsAs(t, NewHandler(chatConfig(t, hub)), chatOp); strings.Contains(rw.Body.String(), "unread") {
		t.Fatalf("DM list without dms: %s", rw.Body)
	}
}

// At most maxUnreadReads history reads per list: the ninth unread DM gets no
// count rather than a ninth read.
func TestAgentsListUnreadReadBudget(t *testing.T) {
	var workers []string
	recs := map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "running"}}
	var dms []string
	history := map[string]string{}
	for i := range 10 {
		n, id := fmt.Sprintf("w%d", i), fmt.Sprintf("id-w%d", i)
		workers = append(workers, n)
		recs[n] = AgentRecord{ID: id, Phase: "running"}
		key := "dm:agent:" + id + ":user:" + chatUID
		dms = append(dms, `{"conversationKey":"`+key+`","hasUnread":true}`)
		history[key] = `{"messages":[{"id":"x","senderId":"` + id + `","createdAt":"2026-10-06T10:01:00Z"}]}`
	}
	hub := dmHub(t, `{"dms":[`+strings.Join(dms, ",")+`]}`, history)
	cfg := chatConfig(t, hub)
	cfg.Workers = workers
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) { return recs, nil }
	_, ans := agentsAs(t, NewHandler(cfg), chatOp)
	counted, none := 0, 0
	for _, a := range ans.Agents[1:] {
		if a.Unread == nil {
			none++
		} else if *a.Unread == 1 {
			counted++
		}
	}
	if counted != maxUnreadReads || none != 10-maxUnreadReads {
		t.Fatalf("counted %d, none %d", counted, none)
	}
	reads := 0
	for _, r := range hub.reached() {
		if strings.Contains(r, "/messages") {
			reads++
		}
	}
	if reads != maxUnreadReads {
		t.Fatalf("history reads %d", reads)
	}
}

// Each list costs several hub calls with the login's session; a page (or a
// script) asking in a burst gets the answer of a moment ago instead, per
// login and never another login's.
func TestAgentsListIsCachedPerLogin(t *testing.T) {
	hub := newPageHub(t)
	hub.me = func(w http.ResponseWriter, r *http.Request) {
		id := strings.NewReplacer("@", "-", ".", "-").Replace(strings.TrimPrefix(r.Header.Get("Cookie"), sessionCookieName+"="))
		_, _ = io.WriteString(w, `{"id":"u-`+id+`"}`)
	}
	cfg := chatConfig(t, hub)
	var recCalls atomic.Int32
	inner := cfg.AgentRecords
	cfg.AgentRecords = func(ctx context.Context) (map[string]AgentRecord, error) { recCalls.Add(1); return inner(ctx) }
	cfg.Session = perLoginSession{}
	g := NewHandler(cfg).(*gate)
	now := time.Unix(1000, 0)
	g.chat.nowFn = func() time.Time { return now }
	_, a1 := agentsAs(t, g, chatOp)
	hubCalls := len(hub.reached())
	_, a2 := agentsAs(t, g, chatOp)
	if recCalls.Load() != 1 || len(hub.reached()) != hubCalls || !reflect.DeepEqual(a1, a2) {
		t.Fatalf("a quick second list asked again: records %d, hub %d → %d", recCalls.Load(), hubCalls, len(hub.reached()))
	}
	// Another login gets its own answer, built for it.
	_, c := agentsAs(t, g, "c@x")
	if c.Login != "c@x" || c.Tier != "contact" || c.UserID != "u-sess-c-x" || recCalls.Load() != 2 {
		t.Fatalf("contact got %+v (records %d)", c, recCalls.Load())
	}
	if _, o := agentsAs(t, g, chatOp); o.Login != chatOp || o.UserID != "u-sess-op-x" {
		t.Fatalf("operator after the contact: %+v", o)
	}
	now = now.Add(3 * time.Second)
	agentsAs(t, g, chatOp)
	if recCalls.Load() != 3 {
		t.Fatalf("after the cache window: records %d", recCalls.Load())
	}
}

// perLoginSession hands each login a cookie of its own.
type perLoginSession struct{}

func (perLoginSession) Cookie(_ context.Context, login string) (string, error) {
	return "sess-" + login, nil
}
func (perLoginSession) Invalidate(string, string) {}
