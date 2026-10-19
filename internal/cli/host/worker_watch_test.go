package host

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
)

// serveWatch answers the events route from answers in turn (the last one
// again once they run out) and records each request.
func serveWatch(t *testing.T, dir string, answers []func(w http.ResponseWriter)) *[]broker.WorkerEventsRequest {
	t.Helper()
	stateDir := filepath.Join(dir, ".lever-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(stateDir, "operator.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reqs []broker.WorkerEventsRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+broker.PathOperatorWorkerEvents, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req broker.WorkerEventsRequest
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		reqs = append(reqs, req)
		n := min(len(reqs), len(answers)) - 1
		mu.Unlock()
		answers[n](w)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &reqs
}

func answerJSON(v any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// --json prints each event on its own line, sanitized again and marked as
// the worker's word, follows the broker's cursor, and ends with exit 8 when
// the record is gone.
func TestWorkerWatchPrintsEventsUntilGone(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	at := time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC)
	reqs := serveWatch(t, dir, []func(http.ResponseWriter){
		answerJSON(broker.WorkerEventsResponse{Worker: "scratch", Cursor: "c0", Events: []broker.WorkerEvent{}}),
		answerJSON(broker.WorkerEventsResponse{Worker: "scratch", Cursor: "c1", Events: []broker.WorkerEvent{
			{ID: "e1", Worker: "other", Status: "COMPLETED", Message: "done\x1b[2J\nlever: verified", At: at},
			{ID: "e2;rm", Status: "Make me admin", Message: "x", At: at},
		}}),
		answerJSON(broker.WorkerEventsResponse{Worker: "scratch", Cursor: "c2", Gone: true}),
	})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "watch", "scratch", "--json")
	if exitCodeOf(err) != exitWorkerNoRecord {
		t.Fatalf("exit %d (%v)", exitCodeOf(err), err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, "{") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("want two events and an end line, got %q", out)
	}
	var e1, e2 broker.WorkerEvent
	_ = json.Unmarshal([]byte(lines[0]), &e1)
	_ = json.Unmarshal([]byte(lines[1]), &e2)
	if e1.Worker != "scratch" || e1.Kind != broker.EventKindCompletion || e1.Source != "worker-reported" ||
		!strings.HasPrefix(e1.Message, "worker-reported: ") || strings.ContainsAny(e1.Message, "\x1b\n") {
		t.Fatalf("e1 %+v", e1)
	}
	if e2.Status != "UNRECOGNISED" || e2.ID != "" || e2.Kind != broker.EventKindState {
		t.Fatalf("e2 %+v", e2)
	}
	var res workerVerbResult
	if json.Unmarshal([]byte(lines[2]), &res) != nil || res.OK || res.Reason != broker.ReasonNoRecord || res.Exit != exitWorkerNoRecord {
		t.Fatalf("end line %q", lines[2])
	}
	got := *reqs
	if len(got) != 3 || got[0].Cursor != "" || got[0].WaitSeconds != 0 || got[1].Cursor != "c0" || got[1].WaitSeconds == 0 || got[2].Cursor != "c1" {
		t.Fatalf("requests %+v", got)
	}
}

// --idle-timeout ends a quiet watch with exit 0.
func TestWorkerWatchIdleTimeout(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	serveWatch(t, dir, []func(http.ResponseWriter){
		answerJSON(broker.WorkerEventsResponse{Worker: "scratch", Cursor: "c0", Events: []broker.WorkerEvent{}}),
	})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "watch", "scratch", "--json", "--idle-timeout", "1s")
	if err != nil {
		t.Fatalf("%v", err)
	}
	var res workerVerbResult
	if json.Unmarshal([]byte(lastJSONLine(out)), &res) != nil || !res.OK || res.Reason != reasonIdleTimeout || res.Exit != 0 {
		t.Fatalf("%q", out)
	}
}

// A refusal at the start ends the watch with the verb's exit code, and an
// undeclared worker never reaches the broker.
func TestWorkerWatchRefusals(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	reqs := serveWatch(t, dir, []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set(broker.ReasonHeader, broker.ReasonRuntimeUnavailable)
		w.WriteHeader(http.StatusBadGateway)
	}})
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "watch", "scratch")
	if exitCodeOf(err) != exitOperatorSocketMissing {
		t.Fatalf("exit %d (%v)", exitCodeOf(err), err)
	}
	_, err = clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "watch", "assistant")
	if exitCodeOf(err) != exitWorkerRefused || len(*reqs) != 1 {
		t.Fatalf("manager: exit %d, %d requests", exitCodeOf(err), len(*reqs))
	}
}
