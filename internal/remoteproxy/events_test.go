package remoteproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const dmEvent = "id: 7\nevent: update\ndata: {\"subject\":\"user.u-contact.chat.dm\",\"data\":{\"id\":\"m-1\",\"sender\":\"agent:w1\",\"senderId\":\"id-w1\",\"msg\":\"SECRET text\",\"threadId\":\"dm:agent:id-w1:user:u-contact\",\"createdAt\":\"2026-10-06T10:00:00.000Z\"}}\n\n"

func reduce(t *testing.T, in string) string {
	t.Helper()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(in))}
	reduceEvents("u-contact")(resp)
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

// reducedDM is dmEvent as a contact gets it.
const reducedDM = "id: 7\nevent: update\ndata: {\"data\":{},\"subject\":\"user.u-contact.chat.dm\"}\n\n"

func TestEventReducerKeepsOnlyTheSubject(t *testing.T) {
	got := reduce(t, ":heartbeat 1700000000000\n\n"+dmEvent+"event: reconnect\ndata: {}\n\n")
	if want := ":heartbeat 1700000000000\n\n" + reducedDM + "event: reconnect\ndata: {}\n\n"; got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestEventReducerDropsWhatItCannotRead(t *testing.T) {
	big := "id: 1\nevent: update\ndata: {\"subject\":\"user.u-contact.chat.dm\",\"data\":{\"msg\":\"" + strings.Repeat("S", 2<<20) + "\"}}\n\n"
	many := "event: update\n" + strings.Repeat("data: "+strings.Repeat("S", 600<<10)+"\n", 4) + "\n"
	for name, in := range map[string]string{
		"not json":         "event: update\ndata: SECRET\n\n",
		"other uid":        "event: update\ndata: {\"subject\":\"user.someone.chat.dm\",\"data\":{\"msg\":\"SECRET\"}}\n\n",
		"uid prefix":       "event: update\ndata: {\"subject\":\"user.u-contact-2.chat.dm\",\"data\":{\"msg\":\"SECRET\"}}\n\n",
		"odd comment":      ": SECRET\n\n",
		"unknown event":    "event: SECRET\ndata: {}\n\n",
		"two data lines":   "event: update\ndata: {\"subject\":\"user.u-contact.chat.dm\",\ndata: \"data\":{\"msg\":\"SECRET\"}}\n\n",
		"subject not text": "event: update\ndata: {\"subject\":\"user.u-contact.chat.SECRET words\",\"data\":{}}\n\n",
		"oversized line":   big,
		"oversized sum":    many,
		"crlf":             "event: update\r\ndata: {\"subject\":\"user.u-contact.chat.dm\",\"data\":{\"msg\":\"SECRET\"}}\r\n\r\n",
	} {
		if got := reduce(t, in+dmEvent); strings.Contains(got, "SECRET") || !strings.HasSuffix(got, reducedDM) {
			t.Errorf("%s: %q", name, got[:min(len(got), 300)])
		}
	}
}

func TestEventReducerStreams(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}
	reduceEvents("u-contact")(resp)
	go func() { _, _ = io.WriteString(pw, dmEvent) }() // the stream stays open
	buf := make([]byte, 4096)
	done := make(chan string, 1)
	go func() { n, _ := resp.Body.Read(buf); done <- string(buf[:n]) }()
	select {
	case got := <-done:
		if got != reducedDM {
			t.Fatalf("%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event must come out before the stream ends")
	}
	_ = pw.Close()
}

func TestEventReducerFailsClosedOnEncoding(t *testing.T) {
	for name, h := range map[string]http.Header{
		"gzip":    {"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}},
		"not sse": {"Content-Type": {"application/json"}},
		"no type": {},
	} {
		resp := &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(dmEvent))}
		reduceEvents("u-contact")(resp)
		if out, _ := io.ReadAll(resp.Body); len(out) != 0 {
			t.Fatalf("%s: %q", name, out)
		}
	}
}

func TestEventsReducedOnlyForAContactWhileOn(t *testing.T) {
	hub := historyHub(t, dmEvent, map[string]string{"Content-Type": "text/event-stream"})
	on := agentMsgHandler(t, hub, recordedOnly("x"))
	if body := contactDo(on, "c@x", "GET", "/events?sub=user.u-contact.chat.%3E", "").Body.String(); body != reducedDM {
		t.Fatalf("contact, on: %q", body)
	}
	if body := contactDo(on, "op@x", "GET", "/events?sub=x", "").Body.String(); body != dmEvent {
		t.Fatalf("operator: unchanged, got %q", body)
	}
	off := agentMsgHandler(t, hub, nil)
	if body := contactDo(off, "c@x", "GET", "/events?sub=x", "").Body.String(); body != dmEvent {
		t.Fatalf("off: byte for byte, got %q", body)
	}
}

func TestEventsReducedOnTheRetry(t *testing.T) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, dmEvent)
	}))
	t.Cleanup(hub.Close)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: &rotatingSession{}, ServeHost: testServeHost,
		AllowedUsers: []string{"op@x", "c@x"}, Contacts: map[string][]string{"c@x": {"w1"}},
		ResolveAgents:  func(context.Context) (map[string]string, error) { return map[string]string{"w1": agentW1}, nil },
		ContactSession: func(string) error { return nil }, MatchAgentMessages: recordedOnly("recorded")})
	rw := contactDo(h, "c@x", "GET", "/events?sub=x", "")
	if calls.Load() != 2 || rw.Code != 200 || rw.Body.String() != reducedDM {
		t.Fatalf("calls=%d %d %q", calls.Load(), rw.Code, rw.Body)
	}
}
