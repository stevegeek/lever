package remoteproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// historyHub answers the contact's DM history with rows from the agent
// (recorded and not), the contact and a hub line.
func historyHub(t *testing.T, body string, header map[string]string) *contactHub {
	h := &contactHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = io.WriteString(w, `{"id":"`+contactUID+`"}`)
			return
		}
		for k, v := range header {
			w.Header().Set(k, v)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(h.Close)
	return h
}

const historyBody = `{"messages":[
 {"id":"a1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"recorded","createdAt":"2026-10-06T10:00:00Z"},
 {"id":"a2","sender":"agent:w1","senderId":"id-w1","type":"input-needed","msg":"SECRET unrecorded","createdAt":"2026-10-06T10:01:00Z"},
 {"id":"a3","sender":"agent:w1","senderId":"id-w1","type":"state-change","msg":"SECRET as state","createdAt":"2026-10-06T10:02:00Z"},
 {"id":"c1","sender":"user:c@x","senderId":"u-contact","type":"instruction","msg":"mine","createdAt":"2026-10-06T09:59:00Z"},
 {"id":"s1","sender":"system","senderId":"hub","type":"system","msg":"agent started","createdAt":"2026-10-06T09:58:00Z"}],
 "nextCursor":"cur-1","totalCount":5,
 "messageAttachments":{"a2":[{"id":"f1","name":"SECRET.pdf"}],"a1":[{"id":"f2","name":"SECRET-kept-row.pdf"}],"c1":[{"id":"f3","name":"mine.pdf"}]},
 "messageExtensions":{"a2":{"messageId":"a2"},"zz":{"messageId":"SECRET-other"}},
 "replyPreviews":{"a2":{"messageId":"a2","senderName":"w1","content":"SECRET unrecorded"}}}`

func agentMsgHandler(t *testing.T, hub *contactHub, match func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error)) http.Handler {
	t.Helper()
	return NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers: []string{"op@x", "c@x"}, Contacts: map[string][]string{"c@x": {"w1"}},
		ResolveAgents:  func(context.Context) (map[string]string, error) { return map[string]string{"w1": agentW1}, nil },
		ContactSession: func(string) error { return nil }, MatchAgentMessages: match})
}

func recordedOnly(want string) func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
	return func(_ context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error) {
		keep := map[string]bool{}
		for _, m := range msgs {
			if contact == "c@x" && agent == "w1" && m.SHA256 == hashHex(want) {
				keep[m.ID] = true
			}
		}
		return keep, nil
	}
}

func hashHex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestHistoryFilterKeepsOnlyRecordedAgentRows(t *testing.T) {
	hub := historyHub(t, historyBody, nil)
	var asked []AgentMessage
	match := func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error) {
		asked = append(asked, msgs...)
		return recordedOnly("recorded")(ctx, contact, agent, msgs)
	}
	h := agentMsgHandler(t, hub, match)
	rw := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages?limit=50"), "")
	body := rw.Body.String()
	if rw.Code != 200 || strings.Contains(body, "SECRET") {
		t.Fatalf("%d %s", rw.Code, body)
	}
	for _, want := range []string{`"a1"`, `"c1"`, `"s1"`, `"nextCursor":"cur-1"`, `"totalCount":5`, `mine.pdf`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, `"a3"`) || strings.Contains(body, "replyPreviews") {
		t.Fatalf("an agent state line or a reply preview leaked: %s", body)
	}
	// Only agent rows are asked about (never the contact's own or a hub line).
	for _, m := range asked {
		if m.ID == "c1" || m.ID == "s1" {
			t.Errorf("asked the broker about %s", m.ID)
		}
	}
	// The operator sees the hub's answer unchanged.
	op := contactDo(h, "op@x", "GET", dmPath(agentW1, contactUID, "/messages"), "")
	if !strings.Contains(op.Body.String(), "SECRET unrecorded") || !strings.Contains(op.Body.String(), "replyPreviews") {
		t.Fatal("the operator's view must be unfiltered")
	}
}

func TestHistoryFilterHidesAnEditedRow(t *testing.T) {
	hub := historyHub(t, strings.Replace(historyBody, `"msg":"recorded"`, `"msg":"recorded (edited)"`, 1), nil)
	h := agentMsgHandler(t, hub, recordedOnly("recorded"))
	if body := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "").Body.String(); strings.Contains(body, `"a1"`) {
		t.Fatal("an edited row no longer matches its record")
	}
}

func TestHistoryFilterHidesADeletedRow(t *testing.T) {
	hub := historyHub(t, strings.Replace(historyBody, `"msg":"recorded"`, `"msg":""`, 1), nil)
	keepAll := func(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, error) {
		keep := map[string]bool{}
		for _, m := range msgs {
			keep[m.ID] = true
		}
		return keep, nil
	}
	h := agentMsgHandler(t, hub, keepAll)
	body := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "").Body.String()
	if strings.Contains(body, `"a1"`) || !strings.Contains(body, `"a2"`) {
		t.Fatalf("an emptied row is never asked about, so never shown: %s", body)
	}
}

func TestHistoryFilterFailsClosed(t *testing.T) {
	down := func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
		return nil, errors.New("socket down")
	}
	for name, tc := range map[string]struct {
		body   string
		header map[string]string
		match  func(context.Context, string, string, []AgentMessage) (map[string]bool, error)
	}{
		"broker down":     {historyBody, nil, down},
		"gzip":            {historyBody, map[string]string{"Content-Encoding": "gzip"}, recordedOnly("recorded")},
		"not json":        {`SECRET unrecorded`, nil, recordedOnly("recorded")},
		"items key":       {`{"items":[{"id":"a2","sender":"agent:w1","senderId":"id-w1","msg":"SECRET"}]}`, nil, recordedOnly("recorded")},
		"odd row":         {`{"messages":[{"id":["a2"],"sender":"agent:w1","msg":"SECRET"}]}`, nil, recordedOnly("recorded")},
		"gzip, no header": {gzipped(historyBody), nil, recordedOnly("recorded")},
	} {
		t.Run(name, func(t *testing.T) {
			h := agentMsgHandler(t, historyHub(t, tc.body, tc.header), tc.match)
			rw := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "")
			if rw.Code != 200 || strings.Contains(rw.Body.String(), "SECRET") {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
		})
	}
}

func TestHistoryFilterOffIsUnchanged(t *testing.T) {
	h := agentMsgHandler(t, historyHub(t, historyBody, nil), nil)
	if got := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "").Body.String(); got != historyBody {
		t.Fatalf("off: the contact gets the hub's answer byte for byte, got %q", got)
	}
}

// The broker's answer is checked against the question: an id it was not
// asked about (a deleted row, a row with no readable time) stays hidden.
func TestHistoryFilterKeepsOnlyAskedIDs(t *testing.T) {
	body := `{"messages":[
 {"id":"d1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"","createdAt":"2026-10-06T10:00:00Z"},
 {"id":"t1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"SECRET no time","createdAt":"yesterday"},
 {"id":"a1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"recorded","createdAt":"2026-10-06T10:00:00Z"}]}`
	var asked []string
	everything := func(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, error) {
		for _, m := range msgs {
			asked = append(asked, m.ID)
		}
		return map[string]bool{"d1": true, "t1": true, "a1": true, "zz": true}, nil
	}
	h := agentMsgHandler(t, historyHub(t, body, nil), everything)
	got := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "").Body.String()
	if strings.Contains(got, `"d1"`) || strings.Contains(got, "SECRET") || !strings.Contains(got, `"a1"`) {
		t.Fatalf("%s", got)
	}
	if len(asked) != 1 || asked[0] != "a1" {
		t.Fatalf("asked %v, want only a1", asked)
	}
}

// A row id that appears twice on a page hides every row with it: the
// broker binds by id, so a second row could borrow the first one's record.
func TestHistoryFilterHidesDuplicateIDs(t *testing.T) {
	body := `{"messages":[
 {"id":"a1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"recorded","createdAt":"2026-10-06T10:00:00Z"},
 {"id":"a1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"SECRET twin","createdAt":"2026-10-06T10:00:01Z"},
 {"id":"c1","sender":"user:c@x","senderId":"u-contact","type":"instruction","msg":"mine","createdAt":"2026-10-06T09:59:00Z"}]}`
	var asked int
	match := func(ctx context.Context, c, a string, msgs []AgentMessage) (map[string]bool, error) {
		asked += len(msgs)
		return recordedOnly("recorded")(ctx, c, a, msgs)
	}
	h := agentMsgHandler(t, historyHub(t, body, nil), match)
	got := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "").Body.String()
	if strings.Contains(got, `"a1"`) || strings.Contains(got, "SECRET") || !strings.Contains(got, `"c1"`) || asked != 0 {
		t.Fatalf("asked=%d %s", asked, got)
	}
}

func TestHistoryFilterAppliesOnTheRetry(t *testing.T) {
	var calls atomic.Int32
	hub := &contactHub{}
	hub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = io.WriteString(w, `{"id":"`+contactUID+`"}`)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized) // the session lapsed: the gate retries once
			return
		}
		_, _ = io.WriteString(w, historyBody)
	}))
	t.Cleanup(hub.Close)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: &rotatingSession{}, ServeHost: testServeHost,
		AllowedUsers: []string{"op@x", "c@x"}, Contacts: map[string][]string{"c@x": {"w1"}},
		ResolveAgents:  func(context.Context) (map[string]string, error) { return map[string]string{"w1": agentW1}, nil },
		ContactSession: func(string) error { return nil }, MatchAgentMessages: recordedOnly("recorded")})
	rw := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages"), "")
	if calls.Load() != 2 || rw.Code != 200 || strings.Contains(rw.Body.String(), "SECRET") || !strings.Contains(rw.Body.String(), `"a1"`) {
		t.Fatalf("calls=%d %d %s", calls.Load(), rw.Code, rw.Body)
	}
}

func TestDMListPreviewIsRemovedForAContact(t *testing.T) {
	body := `{"dms":[{"conversationKey":"dm:agent:` + agentW1 + `:user:` + contactUID + `","lastMessagePreview":"SECRET","lastMessageSender":"w1",` +
		`"lastMessageId":"SECRET-id","lastActivityAt":"SECRET-time","hasUnread":true,"peerSlug":"w1"}]}`
	h := agentMsgHandler(t, historyHub(t, body, nil), recordedOnly("x"))
	got := contactDo(h, "c@x", "GET", "/api/v1/chat/dms", "").Body.String()
	if strings.Contains(got, "SECRET") || strings.Contains(got, "lastMessageSender") || strings.Contains(got, "hasUnread") ||
		!strings.Contains(got, `"peerSlug":"w1"`) {
		t.Fatalf("%s", got)
	}
	var doc struct {
		DMs []map[string]any `json:"dms"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil || len(doc.DMs) != 1 {
		t.Fatalf("the contact's own DM stays listed: %s", got)
	}
	if op := contactDo(h, "op@x", "GET", "/api/v1/chat/dms", "").Body.String(); !strings.Contains(op, "SECRET") {
		t.Fatal("the operator's DM list is unchanged")
	}
	off := agentMsgHandler(t, historyHub(t, body, nil), nil)
	if got := contactDo(off, "c@x", "GET", "/api/v1/chat/dms", "").Body.String(); !strings.Contains(got, "SECRET") {
		t.Fatal("off: the DM list keeps its previews as in spec 1")
	}
}

func TestAgentRow(t *testing.T) {
	for _, tc := range []struct {
		m    historyRow
		want bool
	}{
		{historyRow{Sender: "user:c@x", SenderID: contactUID}, false},
		{historyRow{Sender: "user:c@x", SenderID: "someone-else"}, true},
		{historyRow{Sender: "agent:w1", SenderID: contactUID}, true},
		{historyRow{Sender: "system", SenderID: "hub", Type: "system"}, false},
		{historyRow{Sender: "system", SenderID: "hub", Type: "state-change"}, false},
		{historyRow{Sender: "agent:w1", SenderID: "hub", Type: "system"}, true},
		{historyRow{Sender: "system", SenderID: agentW1, Type: "system"}, true},
		{historyRow{Sender: "system", SenderID: "", Type: "system"}, true},
		{historyRow{Sender: "system", SenderID: "hub", Type: "instruction"}, true},
	} {
		if got := agentRow(tc.m, contactUID, agentW1); got != tc.want {
			t.Errorf("agentRow(%+v) = %v, want %v", tc.m, got, tc.want)
		}
	}
}

// gzipped is s gzip-compressed: a body the hub sent encoded without saying so.
func gzipped(s string) string {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	_, _ = z.Write([]byte(s))
	_ = z.Close()
	return b.String()
}
