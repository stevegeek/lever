package remoteproxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordingHub is the one fake upstream the proxy, session and jaildial tests
// share. It records every request it receives and answers with whatever the
// test queued in answer (200 "ok" when nothing is queued). Tests that need a
// full scion login flow use fakeScionHub in login_test.go instead.
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

// answerWith queues a fixed status (and optional response headers) for every
// request that follows.
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

// hits is how many requests actually reached the hub; denied requests must
// leave it at zero.
func (h *recordingHub) hits() int {
	return len(h.requests())
}

// lastHeader is the header block of the most recent request, or an empty
// header when nothing has arrived.
func (h *recordingHub) lastHeader() http.Header {
	reqs := h.requests()
	if len(reqs) == 0 {
		return http.Header{}
	}
	return reqs[len(reqs)-1].Header
}

// testPAT is the Config.PAT most tests hand the proxy: the narrow remote PAT
// it must inject as "Bearer scion_pat_x".
func testPAT() string { return "scion_pat_x" }
