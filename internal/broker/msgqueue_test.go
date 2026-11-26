package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	// Before the temp directories go: notices are sent in the background.
	t.Cleanup(f.b.queueJobs.Wait)
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
	waiting, _, _, _, err := f.b.queue.read(slug)
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
	f.b.queueJobs.Wait()
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
	f.b.queueJobs.Wait() // the notice goes out in the background
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
		f.b.queueJobs.Wait()
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
		f.b.queueJobs.Wait()
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
		f.b.queueJobs.Wait()
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
		f.b.queueJobs.Wait()
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
	f.b.queueJobs.Wait()
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
	waiting, _, _, _, _ := f.b.queue.read("scratch")
	m, err := f.b.queue.beginSend(waiting[0])
	if err != nil {
		t.Fatal(err)
	}
	f.b.queue.sendDone(m) // as a crash leaves it: no send under way
	unlock()
	f.setPhase("scratch", "running")
	f.b.deliverQueued(context.Background(), "scratch")
	f.b.queueJobs.Wait()
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
		f.fakeMsgRuntime.mu.Lock()
		f.fakeMsgRuntime.sent = append(f.fakeMsgRuntime.sent, o)
		f.fakeMsgRuntime.mu.Unlock()
		if f.after != nil {
			f.after()
		}
		return errors.New("scion: message failed")
	}
	return f.fakeMsgRuntime.Message(ctx, o)
}

// A send that failed after it began is never sent again, whether or not
// the agent still runs (a timeout can follow a send the hub took), and the
// messages behind it wait in order.
func TestQueueSendFailure(t *testing.T) {
	for _, suspend := range []bool{false, true} {
		f := queueBroker(t, "suspended")
		for _, body := range []string{"a", "b"} {
			resp, _ := f.send(t, "manager", `{"to":"scratch","body":"`+body+`"}`)
			mustOK(t, resp, body)
		}
		f.setPhase("scratch", "running")
		frt := &failingMsgRuntime{fakeMsgRuntime: f.rt, failTo: "agent:scratch"}
		if suspend {
			frt.after = func() { f.setPhase("scratch", "suspended") }
		}
		f.b.runtime = frt
		f.b.deliverQueued(context.Background(), "scratch")
		f.b.queueJobs.Wait()
		got := f.queued(t, "scratch")
		if len(got) != 1 || got[0].Body != "b" {
			t.Fatalf("suspend=%v: queued %+v, want only b", suspend, got)
		}
		tries := 0
		for _, m := range f.rt.sent {
			if m.To == "agent:scratch" {
				tries++
			}
		}
		if tries != 1 {
			t.Fatalf("suspend=%v: %d sends to scratch, want 1", suspend, tries)
		}
	}
}

// A message from an earlier life of its sender's name (purged or recycled
// since) is not delivered.
func TestQueueSenderRecreated(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	if err := f.b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	f.setPhase("scratch", "running")
	f.b.deliverQueued(context.Background(), "scratch")
	f.b.queueJobs.Wait()
	for _, m := range f.rt.sent {
		if m.To == "agent:scratch" {
			t.Fatalf("delivered from the old life of worker: %q", m.Body)
		}
	}
	if f.b.queue.count("scratch") != 0 {
		t.Fatal("still queued")
	}
}

// Agents cannot fill a queue against the operator or the manager, and a
// worker is told only how many of its own messages wait.
func TestQueueOperatorRoomAndWorkerCount(t *testing.T) {
	f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.MaxMessages = 2 })
	resp, res := f.send(t, "manager", `{"to":"scratch","body":"m"}`)
	mustOK(t, resp, "manager")
	resp, res = f.send(t, "worker", `{"to":"scratch","body":"w"}`)
	mustOK(t, resp, "worker")
	if res.Waiting != 1 {
		t.Fatalf("worker told %d wait, want its own 1", res.Waiting)
	}
	resp, res = f.send(t, "manager", `{"to":"scratch","body":"m2"}`)
	mustOK(t, resp, "the manager's own pool")
	if res.Waiting != 3 {
		t.Fatalf("manager told %d wait, want 3 (its own and the agents')", res.Waiting)
	}
	if rec := callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"m3"}`, "manager"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "its queue for the manager's messages is full (2 messages waiting, the limit is 2)") {
		t.Fatalf("manager past its own cap: %d %s", rec.Code, rec.Body)
	}
	for range 2 {
		if rec := postNote(t, f.b, `{"to":"scratch","body":"op"}`); rec.Code != http.StatusOK {
			t.Fatalf("operator locked out: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := postNote(t, f.b, `{"to":"scratch","body":"op"}`); rec.Code != http.StatusConflict {
		t.Fatalf("operator past its own cap: %d", rec.Code)
	}
	// The operator's notes are not in the manager's count.
	if n, err := f.b.queue.add(context.Background(), queuedMsg{ID: "probe", Recipient: "scratch", Slug: "scratch", Sender: "worker", Kind: sentledger.WorkerKind("worker"), Body: "x", QueuedAt: time.Now()}, "manager"); err == nil {
		t.Fatalf("worker past its share queued (%d)", n)
	}
}

// The pools, at the queue: workers that together fill the agents' pool
// never refuse the manager's sends, a worker refused for a full pool learns
// no counts, and the manager's count leaves out the operator's notes.
func TestQueuePools(t *testing.T) {
	q := newMsgQueue(MessageQueueConfig{Dir: filepath.Join(t.TempDir(), "q"), MaxMessages: 4})
	msg := func(sender string) queuedMsg {
		id, _ := newQueueID()
		return queuedMsg{ID: id, Recipient: "scratch", Slug: "scratch", Sender: sender, Kind: "k", Body: "x", QueuedAt: time.Now()}
	}
	for _, w := range []string{"w1", "w1", "w2", "w2"} {
		if _, err := q.add(context.Background(), msg(w), "manager"); err != nil {
			t.Fatalf("%s: %v", w, err)
		}
	}
	_, err := q.add(context.Background(), msg("w3"), "manager")
	var full *queueFullError
	if !errors.As(err, &full) || err.Error() != "its message queue is full" {
		t.Fatalf("a worker past the agents' pool: %v", err)
	}
	if _, err := q.add(context.Background(), msg(operatorActor), "manager"); err != nil {
		t.Fatalf("operator: %v", err)
	}
	for i := range 4 {
		n, err := q.add(context.Background(), msg("manager"), "manager")
		if err != nil {
			t.Fatalf("the manager's send %d refused for the workers' messages: %v", i, err)
		}
		if want := 4 + i + 1; n != want {
			t.Fatalf("manager told %d wait, want %d (not the operator's note)", n, want)
		}
	}
	if _, err := q.add(context.Background(), msg("manager"), "manager"); !errors.As(err, &full) || !strings.Contains(err.Error(), "manager's messages is full (4 messages waiting") {
		t.Fatalf("manager past its pool: %v", err)
	}
	if n, err := q.add(context.Background(), msg(operatorActor), "manager"); err != nil || n != 10 {
		t.Fatalf("operator: %d %v, want all 10", n, err)
	}
}

// A worker refused for a full queue is told no counts or bytes: they are
// other agents' messages.
func TestQueueFullRefusalTellsAWorkerNoCounts(t *testing.T) {
	f := queueBroker(t, "running", func(c *Config) { c.MessageQueue.MaxMessages = 2 })
	f.setPhase("assistant", "suspended")
	for _, from := range []string{"scratch", "worker"} {
		resp, _ := f.send(t, from, `{"to":"user:manager","body":"x"}`)
		mustOK(t, resp, from)
	}
	rec := callWorker(t, f.b, wire.PathMsgSend, `{"to":"user:manager","body":"x"}`, "worker")
	body := rec.Body.String()
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonQueueFull || !strings.Contains(body, "its message queue is full, so nothing was queued") ||
		strings.ContainsAny(body, "0123456789") {
		t.Fatalf("%d %q", rec.Code, body)
	}
	if !strings.Contains(f.audit.String(), "its message queue is full (2 messages waiting, the limit is 2)") {
		t.Fatalf("the audit log has no counts: %s", f.audit)
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

// blockingMsgRuntime holds every message to blockTo until release closes.
type blockingMsgRuntime struct {
	*fakeMsgRuntime
	blockTo string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingMsgRuntime) Message(ctx context.Context, o scion.MsgOpts) error {
	if o.To == r.blockTo {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	return r.fakeMsgRuntime.Message(ctx, o)
}

// within fails t when fn does not return within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// A notice whose send is slow holds no lock: the sweep that dropped the
// message, a send queued to the same recipient and a destroy all go ahead
// while it is under way, and it goes out after.
func TestQueueSlowNoticeHoldsNoLock(t *testing.T) {
	f := queueBroker(t, "suspended", func(c *Config) { c.MessageQueue.Expiry = time.Hour })
	resp, _ := f.send(t, "worker", `{"to":"scratch","body":"stale"}`)
	mustOK(t, resp, "send")
	rt := &blockingMsgRuntime{fakeMsgRuntime: f.rt, blockTo: "agent:worker", entered: make(chan struct{}), release: make(chan struct{})}
	f.b.runtime = rt
	later := time.Now().Add(2 * time.Hour)
	f.b.clock = func() time.Time { return later }
	within(t, 5*time.Second, "the queue pass", func() { f.b.queuePass(context.Background()) })
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the notice was not sent")
	}
	within(t, 5*time.Second, "a send to the same recipient", func() {
		resp, res := f.send(t, "manager", `{"to":"scratch","body":"fresh"}`)
		mustOK(t, resp, "send while a notice is under way")
		if !res.Queued || res.Waiting != 1 {
			t.Errorf("answer %+v", res)
		}
	})
	within(t, 5*time.Second, "a destroy", func() {
		unlock, err := f.b.lockWorker(context.Background(), "scratch")
		if err != nil {
			t.Error(err)
			return
		}
		defer unlock()
		f.b.pruneAgentState("scratch")
	})
	close(rt.release)
	f.b.queueJobs.Wait()
	var toWorker, toManager int
	for _, m := range f.rt.sent {
		switch {
		case m.To == "agent:worker" && strings.Contains(m.Body, "it waited 1h0m0s"):
			toWorker++
		case m.To == "agent:assistant" && strings.Contains(m.Body, "the recipient was destroyed"):
			toManager++
		}
	}
	if toWorker != 1 || toManager != 1 {
		t.Fatalf("notices %+v", f.rt.sent)
	}
}

// Past queueNoticesPending waiting notices, a new one is in the audit log
// only: the background sender's queue is bounded.
func TestQueueNoticesAreBounded(t *testing.T) {
	f := queueBroker(t, "suspended")
	rt := &blockingMsgRuntime{fakeMsgRuntime: f.rt, blockTo: "agent:worker", entered: make(chan struct{}), release: make(chan struct{})}
	f.b.runtime = rt
	f.b.queueNotice("worker", f.b.senderGeneration("worker"), "first")
	<-rt.entered
	for range queueNoticesPending + 5 {
		f.b.queueNotice("worker", f.b.senderGeneration("worker"), "more")
	}
	f.b.notices.mu.Lock()
	n := len(f.b.notices.pending)
	f.b.notices.mu.Unlock()
	if n != queueNoticesPending || !strings.Contains(f.audit.String(), "too many notices are waiting") {
		t.Fatalf("%d pending", n)
	}
	close(rt.release)
	f.b.queueJobs.Wait()
}

// cancellingMsgRuntime calls cancel after each message to cancelOn: the
// broker stopping while a delivery is under way.
type cancellingMsgRuntime struct {
	*fakeMsgRuntime
	cancelOn string
	cancel   func()
}

func (r *cancellingMsgRuntime) Message(ctx context.Context, o scion.MsgOpts) error {
	err := r.fakeMsgRuntime.Message(ctx, o)
	if o.To == r.cancelOn {
		r.cancel()
	}
	return err
}

// A broker that stops during a delivery leaves the sends it had not begun
// queued, with no notice: only a send under way may or may not arrive.
func TestQueueStopLeavesUnbegunSendsQueued(t *testing.T) {
	f := queueBroker(t, "suspended")
	for _, body := range []string{"a", "b"} {
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"`+body+`"}`)
		mustOK(t, resp, body)
	}
	f.setPhase("scratch", "running")
	ctx, cancel := context.WithCancel(context.Background())
	f.b.runtime = &cancellingMsgRuntime{fakeMsgRuntime: f.rt, cancelOn: "agent:scratch", cancel: cancel}
	if n := f.b.deliverQueued(ctx, "scratch"); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	f.b.queueJobs.Wait()
	if got := f.queued(t, "scratch"); len(got) != 1 || got[0].Body != "b" {
		t.Fatalf("queued %+v, want b waiting", got)
	}
	if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:scratch" {
		t.Fatalf("sent %+v, want only a and no notice", f.rt.sent)
	}
}

// A message sent whose file could not be removed is never sent again, and
// its sender is not told it may not have arrived: a .sent file, and a file
// this broker could not rename, are removed later without a notice.
func TestQueueSentFileLeftIsDroppedSilently(t *testing.T) {
	t.Run(".sent file", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		dir := filepath.Join(f.queueDir, "scratch")
		ents, _ := os.ReadDir(dir)
		sent := strings.TrimSuffix(ents[0].Name(), queueWaiting) + queueSent
		if err := os.Rename(filepath.Join(dir, ents[0].Name()), filepath.Join(dir, sent)); err != nil {
			t.Fatal(err)
		}
		if n := f.b.queue.count("scratch"); n != 0 {
			t.Fatalf("a .sent file counted: %d", n)
		}
		f.setPhase("scratch", "running")
		f.b.queuePass(context.Background())
		f.b.queueJobs.Wait()
		if len(f.rt.sent) != 0 {
			t.Fatalf("sent %+v", f.rt.sent)
		}
		if _, err := os.Stat(filepath.Join(dir, sent)); !errors.Is(err, os.ErrNotExist) || !strings.Contains(f.audit.String(), "messages already sent") {
			t.Fatalf(".sent file left: %v", err)
		}
	})
	t.Run("not removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores a read-only directory")
		}
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		f.setPhase("scratch", "running")
		dir := filepath.Join(f.queueDir, "scratch")
		// The directory turns read-only during the send: neither the remove
		// nor the rename after it can be made.
		f.b.runtime = &cancellingMsgRuntime{fakeMsgRuntime: f.rt, cancelOn: "agent:scratch", cancel: func() { _ = os.Chmod(dir, 0o500) }}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if n := f.b.deliverQueued(context.Background(), "scratch"); n != 1 {
			t.Fatalf("sent %d", n)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f.b.queuePass(context.Background())
		f.b.queueJobs.Wait()
		if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:scratch" {
			t.Fatalf("sent %+v, want the message once and no notice", f.rt.sent)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Fatalf("left %v", ents)
		}
		if len(f.b.queue.delivered) != 0 {
			t.Fatalf("delivered still holds %v", f.b.queue.delivered)
		}
	})
	t.Run("renamed to .sent", func(t *testing.T) {
		f := queueBroker(t, "suspended")
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
		mustOK(t, resp, "send")
		f.setPhase("scratch", "running")
		dir := filepath.Join(f.queueDir, "scratch")
		// The remove after the send fails; the rename to .sent is made, and
		// the delivery's next sweep removes the .sent file.
		f.b.queue.removeFile = func(p string) error {
			if strings.HasSuffix(p, queueSending) {
				return &os.PathError{Op: "remove", Path: p, Err: syscall.EIO}
			}
			return os.Remove(p)
		}
		if n := f.b.deliverQueued(context.Background(), "scratch"); n != 1 {
			t.Fatalf("sent %d", n)
		}
		if a := f.audit.String(); !strings.Contains(a, "marked sent, it is not sent again") || !strings.Contains(a, "removed 1 file(s) of messages already sent") {
			t.Fatalf("audit %s", a)
		}
		if len(f.b.queue.delivered) != 0 {
			t.Fatalf("a renamed file remembered: %v", f.b.queue.delivered)
		}
		f.b.queuePass(context.Background())
		f.b.queueJobs.Wait()
		if len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:scratch" {
			t.Fatalf("sent %+v, want the message once and no notice", f.rt.sent)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 || !strings.Contains(f.audit.String(), "messages already sent") {
			t.Fatalf("left %v", ents)
		}
	})
}

// A delivery whose send is slow holds no queue lock: a send queued to the
// same recipient and the queue list answer while it is under way, a queue
// pass leaves its .sending file alone, and the message queued meanwhile goes
// out after the ones before it.
func TestQueueSlowDeliveryHoldsNoQueueLock(t *testing.T) {
	f := queueBroker(t, "suspended")
	for _, body := range []string{"a", "b"} {
		resp, _ := f.send(t, "manager", `{"to":"scratch","body":"`+body+`"}`)
		mustOK(t, resp, body)
	}
	f.setPhase("scratch", "running")
	rt := &blockingMsgRuntime{fakeMsgRuntime: f.rt, blockTo: "agent:scratch", entered: make(chan struct{}), release: make(chan struct{})}
	f.b.runtime = rt
	delivered := make(chan int)
	go func() { delivered <- f.b.deliverQueued(context.Background(), "scratch") }()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery did not begin")
	}
	f.setPhase("scratch", "suspended")
	within(t, 5*time.Second, "a send to the same recipient", func() {
		resp, res := f.send(t, "manager", `{"to":"scratch","body":"c"}`)
		mustOK(t, resp, "send while a delivery is under way")
		if !res.Queued || res.Waiting != 2 {
			t.Errorf("answer %+v", res)
		}
	})
	within(t, 5*time.Second, "the queue list", func() {
		rec := httpGet(t, f.b.OperatorHandler(), PathOperatorMsgQueue+"?to=scratch")
		var ql QueueListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &ql); err != nil || len(ql.Messages) != 3 || !ql.Messages[0].Sending {
			t.Errorf("list %d %s", rec.Code, rec.Body)
		}
	})
	within(t, 5*time.Second, "a queue pass", func() { f.b.queuePass(context.Background()) })
	if strings.Contains(f.audit.String(), "may or may not have arrived") {
		t.Fatal("a send under way was taken for an interrupted one")
	}
	f.setPhase("scratch", "running")
	close(rt.release)
	if n := <-delivered; n != 3 {
		t.Fatalf("sent %d, want 3", n)
	}
	f.b.queueJobs.Wait()
	var got []string
	for _, m := range f.rt.sent {
		if m.To == "agent:scratch" {
			got = append(got, m.Body[strings.LastIndex(m.Body, "\n")+1:])
		}
	}
	if strings.Join(got, ",") != "a,b,c" || f.b.queue.count("scratch") != 0 {
		t.Fatalf("delivered %q, %d queued", got, f.b.queue.count("scratch"))
	}
}

// add writes nothing for a request whose context ended while it waited.
func TestQueueAddAfterTheRequestEnded(t *testing.T) {
	q := newMsgQueue(MessageQueueConfig{Dir: filepath.Join(t.TempDir(), "q")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := queuedMsg{ID: "x", Recipient: "scratch", Slug: "scratch", Sender: "manager", Kind: sentledger.KindManager, Body: "x", QueuedAt: time.Now()}
	if _, err := q.add(ctx, m, "manager"); !errors.Is(err, context.Canceled) {
		t.Fatalf("add: %v", err)
	}
	if n := q.count("scratch"); n != 0 {
		t.Fatalf("%d queued", n)
	}
}

// A notice about a message from one life of a name does not go to the next:
// the sender recycled since is the case the drop itself names.
func TestQueueNoticeNotToANewLife(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "worker", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	if err := f.b.Supersede("worker"); err != nil {
		t.Fatal(err)
	}
	f.setPhase("scratch", "running")
	f.b.queuePass(context.Background())
	f.b.queueJobs.Wait()
	if len(f.rt.sent) != 0 {
		t.Fatalf("sent %+v", f.rt.sent)
	}
	if a := f.audit.String(); !strings.Contains(a, "its sender was destroyed, purged or recycled") || !strings.Contains(a, "not sent, the name was destroyed, purged or recycled since") {
		t.Fatalf("audit %s", a)
	}
}

// The notices waiting when the broker stops are not sent: one audit line
// counts them.
func TestQueueNoticesNotSentAtStop(t *testing.T) {
	f := queueBroker(t, "suspended")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.b.bgMu.Lock()
	f.b.bgCtx = ctx
	f.b.bgMu.Unlock()
	for range 3 {
		f.b.queueNotice("worker", f.b.senderGeneration("worker"), "x")
	}
	f.b.queueJobs.Wait()
	if len(f.rt.sent) != 0 || !strings.Contains(f.audit.String(), "queue notice(s) not sent: the broker is stopping") {
		t.Fatalf("sent %+v", f.rt.sent)
	}
}

// A queue directory that is not private is audited once, not every pass.
func TestQueueUnreadableAuditedOnce(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	if err := os.Chmod(filepath.Join(f.queueDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.b.queuePass(context.Background())
	}
	if n := strings.Count(f.audit.String(), "left until it can be read"); n != 1 {
		t.Fatalf("audited %d times", n)
	}
}

// One queue directory that is not private is left out of `lever msg list`,
// named in unreadable, and the rest is listed.
func TestQueueListSkipsAnUnreadableQueue(t *testing.T) {
	f := queueBroker(t, "suspended")
	f.setPhase("worker", "suspended")
	for _, to := range []string{"scratch", "worker"} {
		resp, _ := f.send(t, "manager", `{"to":"`+to+`","body":"x"}`)
		mustOK(t, resp, to)
	}
	if err := os.Chmod(filepath.Join(f.queueDir, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := httpGet(t, f.b.OperatorHandler(), PathOperatorMsgQueue)
	var ql QueueListResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ql) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(ql.Messages) != 1 || ql.Messages[0].To != "scratch" || len(ql.Unreadable) != 1 || ql.Unreadable[0] != "worker" {
		t.Fatalf("list %+v", ql)
	}
}

// A resume of an agent that already runs delivers what waits for it now.
func TestQueueResumeOfARunningAgentDelivers(t *testing.T) {
	f := queueBroker(t, "suspended")
	resp, _ := f.send(t, "manager", `{"to":"scratch","body":"x"}`)
	mustOK(t, resp, "send")
	f.fleet.mu.Lock()
	f.fleet.staticPhases = true
	f.fleet.mu.Unlock()
	f.setPhase("scratch", "running")
	rec := callWorker(t, f.b, wire.PathWorkerResume, `{"worker":"scratch"}`, "manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	f.b.queueJobs.Wait()
	if len(f.fleet.resumed) != 0 || len(f.rt.sent) != 1 || f.rt.sent[0].To != "agent:scratch" {
		t.Fatalf("resumed %v, sent %+v", f.fleet.resumed, f.rt.sent)
	}
}

// queueSpawnEnv is a spawn environment with the queue wired and
// worker_to_worker on.
func queueSpawnEnv(t *testing.T) *spawnEnv {
	t.Helper()
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.ManagerMay = true; t.MaxRunning = 3 })
	e.b.queue = newMsgQueue(MessageQueueConfig{Dir: filepath.Join(t.TempDir(), "message-queue")})
	e.b.workerToWorker = true
	t.Cleanup(e.b.queueJobs.Wait)
	return e
}

// setAgentPhase sets slug's phase in e's hub, which then keeps it.
func (e *spawnEnv) setAgentPhase(slug, phase string) {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	e.rt.staticPhases = true
	for i, a := range e.rt.agents[e.b.instanceProject] {
		if a.Slug == slug {
			e.rt.agents[e.b.instanceProject][i].Phase = phase
			return
		}
	}
	e.rt.agents[e.b.instanceProject] = append(e.rt.agents[e.b.instanceProject], scion.Agent{Slug: slug, Phase: phase})
}

// The policy is applied again at delivery to spawned agents too: a peer
// that is no longer of its recipient's spawner, and the manager once the
// template's spawners dropped it, get nothing delivered.
func TestQueueSpawnedPolicyAtDelivery(t *testing.T) {
	t.Run("a peer of another spawner", func(t *testing.T) {
		e := queueSpawnEnv(t)
		rt := &fakeMsgRuntime{WorkerRuntime: e.rt}
		e.b.runtime = rt
		a, c := e.spawn(t, `{"template":"session"}`).Name, e.spawn(t, `{"template":"session"}`).Name
		e.setAgentPhase(a, "running")
		e.setAgentPhase(c, "suspended")
		rec := callWorker(t, e.b, wire.PathMsgSend, `{"to":"`+c+`","body":"x"}`, a)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued":true`) {
			t.Fatalf("peer send: %d %s", rec.Code, rec.Body)
		}
		// As a registry the operator restored with c from another spawner.
		if _, err := e.b.workers.update(c, func(r *AgentRecord) { r.CreatedBy = "ssh:SHA256:other" }); err != nil {
			t.Fatal(err)
		}
		e.setAgentPhase(c, "running")
		e.b.deliverQueued(context.Background(), c)
		e.b.queueJobs.Wait()
		for _, m := range rt.sent {
			if m.To == "agent:"+c {
				t.Fatalf("delivered across spawners: %q", m.Body)
			}
		}
		if e.b.queue.count(c) != 0 || !strings.Contains(fmt.Sprint(rt.sent), "policy no longer lets its sender reach") {
			t.Fatalf("queued %d, sent %+v", e.b.queue.count(c), rt.sent)
		}
	})
	t.Run("a template whose spawners dropped the manager", func(t *testing.T) {
		e := queueSpawnEnv(t)
		rt := &fakeMsgRuntime{WorkerRuntime: e.rt}
		e.b.runtime = rt
		rec := callWorker(t, e.b, "/agent/spawn", `{"template":"session"}`, "test-manager")
		var mine SpawnResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		e.setAgentPhase(mine.Name, "suspended")
		rec = callWorker(t, e.b, wire.PathMsgSend, `{"to":"`+mine.Name+`","body":"x"}`, "test-manager")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued":true`) {
			t.Fatalf("send: %d %s", rec.Code, rec.Body)
		}
		e.b.workers.mu.Lock()
		tm := e.b.workers.templates["session"]
		tm.ManagerMay = false
		e.b.workers.templates["session"] = tm
		e.b.workers.mu.Unlock()
		e.setAgentPhase(mine.Name, "running")
		e.b.deliverQueued(context.Background(), mine.Name)
		e.b.queueJobs.Wait()
		for _, m := range rt.sent {
			if m.To == "agent:"+mine.Name {
				t.Fatalf("delivered for a manager the template dropped: %q", m.Body)
			}
		}
		if e.b.queue.count(mine.Name) != 0 {
			t.Fatal("still queued")
		}
	})
}

// --wake on a spawned agent the manager may not reach wakes nothing: the
// send itself is refused by the policy and nothing is queued, and the wake
// rule refuses it on its own.
func TestQueueWakeOfAnAgentTheManagerDoesNotReach(t *testing.T) {
	e := queueSpawnEnv(t)
	rt := &fakeMsgRuntime{WorkerRuntime: e.rt}
	e.b.runtime = rt
	theirs := e.spawn(t, `{"template":"session"}`).Name
	e.setAgentPhase(theirs, "suspended")
	rec := callWorker(t, e.b, wire.PathMsgSend, `{"to":"`+theirs+`","body":"x","wake":true}`, "test-manager")
	if rec.Code != http.StatusForbidden || e.b.queue.count(theirs) != 0 {
		t.Fatalf("%d %s, %d queued", rec.Code, rec.Body, e.b.queue.count(theirs))
	}
	if why := e.b.wakeRefusal(context.Background(), "test-manager", theirs); !strings.Contains(why, "only the manager and the operator may wake") {
		t.Fatalf("wake rule: %q", why)
	}
	e.b.queueJobs.Wait()
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	for _, r := range e.rt.resumed {
		if r == theirs {
			t.Fatal("woken")
		}
	}
}

// The queue loop's pass, resumes, sends and destroys run at once without a
// race (run with -race) or a deadlock, and nothing is delivered twice.
func TestQueueLoopAgainstResumeAndDestroy(t *testing.T) {
	f := queueBroker(t, "suspended")
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		}()
	}
	run(func() { f.b.queuePass(ctx) })
	run(func() { callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"m"}`, "manager") })
	run(func() { callWorker(t, f.b, wire.PathMsgSend, `{"to":"scratch","body":"w"}`, "worker") })
	run(func() { callWorker(t, f.b, wire.PathWorkerResume, `{"worker":"scratch"}`, "manager") })
	run(func() { f.setPhase("scratch", "suspended") })
	run(func() {
		unlock, err := f.b.lockWorker(ctx, "scratch")
		if err != nil {
			return
		}
		f.b.dropQueue("scratch", "the recipient was destroyed")
		unlock()
	})
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	f.b.queueJobs.Wait()
	f.rt.mu.Lock()
	defer f.rt.mu.Unlock()
	seen := map[string]bool{}
	defer func() { t.Logf("%d delivered", len(seen)) }()
	for _, m := range f.rt.sent {
		if m.To != "agent:scratch" {
			continue
		}
		if ref := refOf(t, m.Body); seen[ref] {
			t.Fatalf("ref %s delivered twice", ref)
		} else {
			seen[ref] = true
		}
	}
}
