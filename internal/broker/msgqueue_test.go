package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/wire"
)

// queueWorkers are msgWorkers with what a resume stages.
var queueWorkers = []WorkerSpec{
	{Name: "scratch", WorkspaceSubdir: "workers/scratch", TicketDir: "/run/user/501/lever/tickets/scratch"},
	{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker"},
}

type queueFixture struct {
	b        *Broker
	rt       *fakeMsgRuntime
	fleet    *fakeRuntime
	sentDir  string
	queueDir string
	audit    *bytes.Buffer
}

// queueBroker is the messaging fixture with the sent ledger and the queue
// wired, scratch in phase, worker_to_worker on.
func queueBroker(t *testing.T, phase string, opts ...configOpt) queueFixture {
	t.Helper()
	f := queueFixture{sentDir: filepath.Join(t.TempDir(), "sent-ledger"), queueDir: filepath.Join(t.TempDir(), "state", "message-queue"), audit: &bytes.Buffer{}}
	f.fleet = fleetWith("scratch", phase)
	f.rt = &fakeMsgRuntime{WorkerRuntime: f.fleet}
	opts = append([]configOpt{withAudit(f.audit), withManager("manager", "assistant"), withRuntime(f.rt, queueWorkers...), withSentLedger(&f.sentDir),
		func(c *Config) {
			c.Dispatch.WorkerToWorker = true
			c.Dispatch.Tickets = f.fleet
			c.MessageQueue = MessageQueueConfig{Dir: f.queueDir}
		}}, opts...)
	f.b = New(testConfig(t, opts...))
	return f
}

// setPhase sets slug's phase in the fake hub.
func (f queueFixture) setPhase(slug, phase string) {
	f.fleet.mu.Lock()
	defer f.fleet.mu.Unlock()
	for i, a := range f.fleet.agents[testInstanceProject] {
		if a.Slug == slug {
			f.fleet.agents[testInstanceProject][i].Phase = phase
		}
	}
}

func (f queueFixture) send(t *testing.T, caller, body string) (*http.Response, wire.MsgSendResponse) {
	t.Helper()
	rec := callWorker(t, f.b, wire.PathMsgSend, body, caller)
	var res wire.MsgSendResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("answer %q: %v", rec.Body, err)
		}
	}
	return rec.Result(), res
}

func (f queueFixture) queued(t *testing.T, slug string) []queuedMsg {
	t.Helper()
	unlock := f.b.queue.lock(slug)
	defer unlock()
	waiting, _, _, err := f.b.queue.read(slug)
	if err != nil {
		t.Fatal(err)
	}
	return waiting
}

func httpGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func mustOK(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("%s: %d %s", what, resp.StatusCode, buf.String())
	}
}

// A message to a suspended agent is queued host-side, 0600 in a 0700
// directory: nothing reaches scion and the sent ledger holds nothing yet.
func TestQueueOnSuspended(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, res := f.send(t, "manager", `{"to":"scratch","body":"hello"}`)
	mustOK(t, resp, "send")
	if !res.OK || !res.Queued || res.Waiting != 1 || res.Phase != "suspended" || res.Waking {
		t.Fatalf("answer %+v", res)
	}
	if len(f.rt.sent) != 0 {
		t.Fatalf("sent %+v to a suspended worker", f.rt.sent)
	}
	if n, _ := sentledger.Count(f.sentDir); n != 0 {
		t.Fatalf("%d record files before delivery", n)
	}
	for _, d := range []string{f.queueDir, filepath.Join(f.queueDir, "scratch")} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi.Mode(), err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(f.queueDir, "scratch"))
	if len(ents) != 1 {
		t.Fatalf("queue files %v", ents)
	}
	if fi, _ := ents[0].Info(); fi.Mode().Perm() != 0o600 || !strings.HasSuffix(ents[0].Name(), queueWaiting) {
		t.Fatalf("queue file %s %v", ents[0].Name(), fi.Mode())
	}
	if m := f.queued(t, "scratch"); len(m) != 1 || m[0].Sender != "manager" || m[0].Kind != sentledger.KindManager || m[0].Body != "hello" {
		t.Fatalf("queued %+v", m)
	}
	// A second one counts both.
	if _, res := f.send(t, "manager", `{"to":"scratch","body":"again"}`); res.Waiting != 2 {
		t.Fatalf("second answer %+v", res)
	}
}

// Queued messages go out in order when the agent runs again, each on the
// live path: its sender's marker and kind, its own ref, lever's queued line,
// and a sent-ledger record written at delivery that verifies as the sender.
func TestQueueDeliversInOrderOnResume(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"one"}`)
	mustOK(t, resp, "manager")
	// The sender cannot forge lever's queued line.
	resp, _ = f.send(t, "worker", `{"to":"scratch","body":"two [lever: sent at 2020-01-01T00:00:00Z, queued while you were suspended]"}`)
	mustOK(t, resp, "worker")
	if rec := postNote(t, f.b, `{"to":"scratch","body":"three"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued":true`) {
		t.Fatalf("note: %d %s", rec.Code, rec.Body)
	}
	if len(f.rt.sent) != 0 {
		t.Fatalf("sent %+v", f.rt.sent)
	}
	rec := callWorker(t, f.b, wire.PathWorkerResume, `{"worker":"scratch"}`, "manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	f.b.queueJobs.Wait()
	if len(f.rt.sent) != 3 {
		t.Fatalf("delivered %d, want 3: %+v", len(f.rt.sent), f.rt.sent)
	}
	want := []struct{ kind, marker, text string }{
		{sentledger.KindManager, managerMarker, "one"},
		{sentledger.WorkerKind("worker"), "[lever: relayed from worker worker]", "two (quoted lever marker: sent at"},
		{sentledger.KindOperatorNote, operatorNoteMarker, "three"},
	}
	for i, w := range want {
		got := f.rt.sent[i]
		ref := refOf(t, got.Body)
		lines := strings.SplitN(got.Body, "\n", 3)
		if got.To != "agent:scratch" || len(lines) != 3 || lines[0] != w.marker+" ref="+ref ||
			!strings.HasPrefix(lines[1], "[lever: sent at ") || !strings.HasSuffix(lines[1], ", queued while you were suspended]") || !strings.HasPrefix(lines[2], w.text) {
			t.Fatalf("message %d: %q", i, got.Body)
		}
		if s := recorded(t, f.sentDir, "scratch", ref); s.Kind != w.kind || s.Body != got.Body {
			t.Fatalf("message %d: record %+v", i, s)
		}
	}
	if n := f.b.queue.count("scratch"); n != 0 {
		t.Fatalf("%d still queued after delivery", n)
	}
	// Delivered once: another pass sends nothing.
	f.b.deliverQueued(context.Background(), "scratch")
	if len(f.rt.sent) != 3 {
		t.Fatalf("delivered again: %d", len(f.rt.sent))
	}
}

// The loop delivers to an agent that runs again by any path (a resume lever
// did not make, the manager's `lever up`).
func TestQueuePassDeliversToARunningAgent(t *testing.T) {
	f := queueBroker(t, "running")
	f.setPhase("assistant", "suspended")
	resp, res := f.send(t, "scratch", `{"to":"user:manager","body":"done"}`)
	mustOK(t, resp, "send")
	if !res.Queued {
		t.Fatalf("answer %+v", res)
	}
	f.b.queuePass(context.Background())
	if len(f.rt.sent) != 0 {
		t.Fatalf("delivered to a suspended manager: %+v", f.rt.sent)
	}
	f.setPhase("assistant", "running")
	f.b.queuePass(context.Background())
	if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:assistant" {
		t.Fatalf("sent %+v", f.rt.sent)
	}
	if s := recorded(t, f.sentDir, "manager", refOf(t, f.rt.sent[0].Body)); s.Kind != sentledger.WorkerKind("scratch") {
		t.Fatalf("record %+v", s)
	}
}

// A full queue refuses the next message by name and queues nothing: by count,
// by bytes, and by one sender's share.
func TestQueueCaps(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.MaxMessages = 2 })
		for range 2 {
			if rec := postNote(t, f.b, `{"to":"scratch","body":"x"}`); rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		}
		rec := postNote(t, f.b, `{"to":"scratch","body":"x"}`)
		if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonQueueFull || !strings.Contains(rec.Body.String(), "full (2 messages waiting, the limit is 2)") {
			t.Fatalf("third: %d %q %s", rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
		}
		if n := f.b.queue.count("scratch"); n != 2 {
			t.Fatalf("%d queued", n)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.MaxBytes = 10 })
		if rec := postNote(t, f.b, `{"to":"scratch","body":"0123456789"}`); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		rec := postNote(t, f.b, `{"to":"scratch","body":"x"}`)
		if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonQueueFull || !strings.Contains(rec.Body.String(), "no room for 1 more bytes (10 of 10") {
			t.Fatalf("over bytes: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("share", func(t *testing.T) {
		f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.MaxMessages = 4 })
		for range 2 {
			resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
			mustOK(t, resp, "worker")
		}
		rec := callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"x"}`, "worker")
		if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonQueueFull || !strings.Contains(rec.Body.String(), "your share") {
			t.Fatalf("over share: %d %s", rec.Code, rec.Body)
		}
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "another sender")
	})
}

// A message past the expiry is dropped, never delivered, and its sender is
// told through a recorded queue notice.
func TestQueueExpiry(t *testing.T) {
	f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.Expiry = time.Hour })
	resp, _ := f.send(t, "worker", `{"to":"scratch","body":"stale"}`)
	mustOK(t, resp, "send")
	later := time.Now().Add(2 * time.Hour)
	f.b.clock = func() time.Time { return later }
	f.setPhase("scratch", "running")
	f.b.queuePass(context.Background())
	if f.b.queue.count("scratch") != 0 {
		t.Fatal("expired message still queued")
	}
	if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:worker" || !strings.Contains(f.rt.sent[0].Body, "was dropped and not delivered: it waited 1h0m0s") {
		t.Fatalf("sent %+v, want only the notice to worker", f.rt.sent)
	}
	if s := recorded(t, f.sentDir, "worker", refOf(t, f.rt.sent[0].Body)); s.Kind != sentledger.KindQueueNotice {
		t.Fatalf("notice record %+v", s)
	}
}

// Queuing never widens the policy: a send the policy refuses is refused, not
// queued, and a message whose sender lost the right, or whose recipient is a
// new life of its name, is dropped at delivery.
func TestQueuePolicyNotWidened(t *testing.T) {
	t.Run("refused at send", func(t *testing.T) {
		f := queueBroker(t, "suspended", func(c *Config) { c.Dispatch.WorkerToWorker = false })
		rec := callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"x"}`, "worker")
		if rec.Code != http.StatusForbidden || f.b.queue.count("scratch") != 0 {
			t.Fatalf("%d %s, %d queued", rec.Code, rec.Body, f.b.queue.count("scratch"))
		}
	})
	t.Run("rights removed", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		f.b.workerToWorker = false // as a broker restarted on the new config
		f.setPhase("scratch", "running")
		f.b.deliverQueued(context.Background(), "scratch")
		if f.b.queue.count("scratch") != 0 {
			t.Fatal("still queued")
		}
		if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:worker" || !strings.Contains(f.rt.sent[0].Body, "policy no longer lets its sender reach") {
			t.Fatalf("sent %+v, want only the notice", f.rt.sent)
		}
	})
	t.Run("sender revoked", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		f.b.Revoke("worker")
		f.setPhase("scratch", "running")
		f.b.deliverQueued(context.Background(), "scratch")
		if len(f.rt.sent) != 0 || f.b.queue.count("scratch") != 0 {
			t.Fatalf("sent %+v, %d queued", f.rt.sent, f.b.queue.count("scratch"))
		}
	})
	t.Run("recipient recreated", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		// A purge or recycle moves the certificate cutoff.
		if err := f.b.Supersede("scratch"); err != nil {
			t.Fatal(err)
		}
		f.setPhase("scratch", "running")
		f.b.deliverQueued(context.Background(), "scratch")
		if f.b.queue.count("scratch") != 0 {
			t.Fatal("still queued")
		}
		for _, m := range f.rt.sent {
			if m.To == "agent:scratch" {
				t.Fatalf("delivered to the new life of scratch: %q", m.Body)
			}
		}
	})
	t.Run("a queued file cannot change its sender's tier", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		// The kind is checked against the sender again at delivery.
		dir := filepath.Join(f.queueDir, "scratch")
		ents, _ := os.ReadDir(dir)
		p := filepath.Join(dir, ents[0].Name())
		raw, _ := os.ReadFile(p)
		raw = bytes.Replace(raw, []byte(`"kind":"worker:worker"`), []byte(`"kind":"operator-note"`), 1)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		f.setPhase("scratch", "running")
		f.b.deliverQueued(context.Background(), "scratch")
		for _, m := range f.rt.sent {
			if m.To == "agent:scratch" {
				t.Fatalf("delivered with a changed kind: %q", m.Body)
			}
		}
	})
}

// --wake resumes the recipient for a caller that may resume it and delivers;
// otherwise the message stays queued and the answer says why.
func TestQueueWake(t *testing.T) {
	t.Run("manager wakes", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, res := f.send(t, "manager", `{"to":"scratch","body":"now","wake":true}`)
		mustOK(t, resp, "send")
		if !res.Queued || !res.Waking {
			t.Fatalf("answer %+v", res)
		}
		f.b.queueJobs.Wait()
		if len(f.fleet.resumed) != 1 || len(f.rt.sent) != 1 || !strings.HasSuffix(f.rt.sent[0].Body, "\nnow") {
			t.Fatalf("resumed %v, sent %+v", f.fleet.resumed, f.rt.sent)
		}
	})
	t.Run("refused by admission", func(t *testing.T) {
		// worker runs and the cap is 1: scratch may not start.
		f := queueBroker(t, "suspended", func(c *Config) { c.Dispatch.MaxRunningWorkers = 1 })
		resp, res := f.send(t, "manager", `{"to":"scratch","body":"now","wake":true}`)
		mustOK(t, resp, "send")
		if !res.Queued || res.Waking || !strings.Contains(res.Note, "broker.max_running_workers is 1") {
			t.Fatalf("answer %+v", res)
		}
		f.b.queueJobs.Wait()
		if len(f.fleet.resumed) != 0 || f.b.queue.count("scratch") != 1 {
			t.Fatalf("resumed %v, %d queued", f.fleet.resumed, f.b.queue.count("scratch"))
		}
	})
	t.Run("a worker may not wake", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, res := f.send(t, "worker", `{"to":"scratch","body":"now","wake":true}`)
		mustOK(t, resp, "send")
		if res.Waking || !strings.Contains(res.Note, "only the manager and the operator may wake") {
			t.Fatalf("answer %+v", res)
		}
		f.b.queueJobs.Wait()
		if len(f.fleet.resumed) != 0 {
			t.Fatalf("resumed %v", f.fleet.resumed)
		}
	})
	t.Run("the manager is not woken", func(t *testing.T) {
		f := queueBroker(t, "running")
		f.setPhase("assistant", "suspended")
		rec := postNote(t, f.b, `{"to":"manager","body":"x","wake":true}`)
		var res wire.OperatorNoteResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusOK || !res.Queued || res.Waking || !strings.Contains(res.Note, "lever up") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("the operator wakes", func(t *testing.T) {
		f := queueBroker(t, "stopped")
		rec := postNote(t, f.b, `{"to":"scratch","body":"x","wake":true}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"waking":true`) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		f.b.queueJobs.Wait()
		if len(f.rt.sent) != 1 || !strings.HasPrefix(f.rt.sent[0].Body, operatorNoteMarker) || !strings.Contains(f.rt.sent[0].Body, "queued while you were stopped") {
			t.Fatalf("sent %+v", f.rt.sent)
		}
	})
}

// Destroy drops the agent's queue (it does not go into the archive) and its
// senders are told.
func TestQueueDroppedWithItsAgent(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	f.b.pruneAgentState("scratch")
	if f.b.queue.count("scratch") != 0 {
		t.Fatal("still queued")
	}
	if _, err := os.Stat(filepath.Join(f.queueDir, "scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queue directory left: %v", err)
	}
	if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:assistant" || !strings.Contains(f.rt.sent[0].Body, "the recipient was destroyed") {
		t.Fatalf("sent %+v", f.rt.sent)
	}
}

// A send that a crash interrupted is never sent again: its sender is told
// it may or may not have arrived.
func TestQueueInterruptedSendIsNotRepeated(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	unlock := f.b.queue.lock("scratch")
	waiting, _, _, _ := f.b.queue.read("scratch")
	if _, err := f.b.queue.beginSend(waiting[0]); err != nil {
		t.Fatal(err)
	}
	unlock()
	f.setPhase("scratch", "running")
	f.b.deliverQueued(context.Background(), "scratch")
	for _, s := range f.rt.sent {
		if s.To == "agent:scratch" {
			t.Fatalf("an interrupted send went out again: %q", s.Body)
		}
	}
	if f.b.queue.count("scratch") != 0 || len(f.rt.sent) != 1 || !strings.Contains(f.rt.sent[0].Body, "may or may not have arrived") {
		t.Fatalf("sent %+v, %d queued", f.rt.sent, f.b.queue.count("scratch"))
	}
}

// failingMsgRuntime fails every message to failTo.
type failingMsgRuntime struct {
	*fakeMsgRuntime
	failTo string
	after  func()
}

func (f *failingMsgRuntime) Message(ctx context.Context, o scion.MsgOpts) error {
	if o.To == f.failTo {
		f.fakeMsgRuntime.sent = append(f.fakeMsgRuntime.sent, o)
		if f.after != nil {
			f.after()
		}
		return errors.New("scion: message failed")
	}
	return f.fakeMsgRuntime.Message(ctx, o)
}

// A send the hub refused because the agent stopped running waits again in
// its place; one that failed while the agent still runs is not sent twice.
func TestQueueSendFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		suspend   bool
		wantQueue int
	}{{"agent went away", true, 2}, {"agent still runs", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := queueBroker(t, "suspended")
			for _, body := range []string{"a", "b"} {
				resp, _ := f.send(t, "manager", `{"to":"scratch","body":"`+body+`"}`)
				mustOK(t, resp, body)
			}
			f.setPhase("scratch", "running")
			frt := &failingMsgRuntime{fakeMsgRuntime: f.rt, failTo: "agent:scratch"}
			if tc.suspend {
				frt.after = func() { f.setPhase("scratch", "suspended") }
			}
			f.b.runtime = frt
			f.b.deliverQueued(context.Background(), "scratch")
			if n := f.b.queue.count("scratch"); n != tc.wantQueue {
				t.Fatalf("%d queued, want %d", n, tc.wantQueue)
			}
			if got := f.queued(t, "scratch"); got[len(got)-1].Body != "b" || (tc.wantQueue == 2 && got[0].Body != "a") {
				t.Fatalf("order lost: %+v", got)
			}
		})
	}
}

// The lists count each agent's queue, and `lever msg list` shows it.
func TestQueueListCounts(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	rec := httpGet(t, f.b.OperatorHandler(), PathOperatorWorkerList)
	var wl WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wl); err != nil {
		t.Fatal(err)
	}
	for _, e := range wl.Workers {
		if want := map[string]int{"scratch": 1}[e.Name]; e.QueuedMessages != want {
			t.Fatalf("%s: queued_messages %d, want %d", e.Name, e.QueuedMessages, want)
		}
	}
	rec = httpGet(t, f.b.OperatorHandler(), PathOperatorMsgQueue+"?to=scratch")
	var ql QueueListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ql); err != nil {
		t.Fatal(err)
	}
	if len(ql.Messages) != 1 || ql.Messages[0].To != "scratch" || ql.Messages[0].Kind != sentledger.KindManager || ql.Messages[0].Body != "x" ||
		ql.MaxMessages != defaultQueueMaxMessages || ql.Expiry != defaultQueueExpiry.String() {
		t.Fatalf("queue list %+v", ql)
	}
	if rec := httpGet(t, f.b.OperatorHandler(), PathOperatorMsgQueue+"?to=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", rec.Code)
	}
}

// With no queue (the state directory inside the tree) the send is refused
// as before.
func TestQueueOffRefuses(t *testing.T) {
	f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue = MessageQueueConfig{} })
	rec := callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"x"}`, "manager")
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonNotRunning {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httpGet(t, f.b.OperatorHandler(), PathOperatorMsgQueue)
	if !strings.Contains(rec.Body.String(), `"off":true`) {
		t.Fatalf("list: %s", rec.Body)
	}
}
