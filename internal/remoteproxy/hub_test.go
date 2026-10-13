package remoteproxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordingHub is the shared fake upstream: it records requests and answers
// with what was queued (200 "ok" by default). fakeScionHub in login_test.go
// does the full login flow.
type recordingHub struct {
	*httptest.Server
	mu     sync.Mutex
	seen   []*http.Request
	answer func(w http.ResponseWriter, r *http.Request)
}

func newRecordingHub(t *testing.T) *recordingHub {
	t.Helper()
	h := &recordingHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Clone(r.Context()))
		answer := h.answer
		h.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *recordingHub) answerWith(status int, hdr http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answer = func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
	}
}

func (h *recordingHub) requests() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*http.Request(nil), h.seen...)
}

func (h *recordingHub) hits() int {
	return len(h.requests())
}

func (h *recordingHub) lastHeader() http.Header {
	reqs := h.requests()
	if len(reqs) == 0 {
		return http.Header{}
	}
	return reqs[len(reqs)-1].Header
}

const testCookie = "sess-test"

// testSession: the proxy refuses every request without some session.
func testSession() *stubSession { return &stubSession{cookie: testCookie} }
