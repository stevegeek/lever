package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

func postWorkerTicket(t *testing.T, b *Broker, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", wire.PathWorkerTicket, bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	b.AdminHandler().ServeHTTP(w, r)
	return w
}

// The admin mint stages the envelope through the guest channel exactly as a
// dispatch does, and answers with the guest PATH — never the ticket value.
func TestWorkerTicketAdminMintsAndStages(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker"}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)

	w := postWorkerTicket(t, b, `{"worker":"worker"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp wire.WorkerTicketResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Worker != "worker" || resp.Path != "/run/user/501/lever/tickets/worker/bootstrap.json" {
		t.Fatalf("response = %+v", resp)
	}
	bs := rt.lastStaged(t, "worker")
	if bs.Ticket == "" || bs.AgentCN != "worker" || bs.BrokerCA != "CA-PEM" || bs.BrokerURL != "https://10.0.0.2:8080" {
		t.Fatalf("staged envelope = %+v", bs)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(bs.Ticket)) {
		t.Fatal("the ticket value must not cross the admin wire")
	}
	// The staged ticket is a real one: it redeems for the worker CN.
	if err := b.tickets.Redeem(bs.Ticket, "worker", b.reenrolNow()); err != nil {
		t.Fatalf("staged ticket does not redeem: %v", err)
	}
}

func TestWorkerTicketAdminRefusesUnknownWorkerAndBadBody(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker"}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)
	if w := postWorkerTicket(t, b, `{"worker":"ghost"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown worker: status = %d, want 404", w.Code)
	}
	if w := postWorkerTicket(t, b, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status = %d, want 400", w.Code)
	}
	if len(rt.staged) != 0 {
		t.Fatal("nothing may be staged for a refused request")
	}
}

func TestWorkerTicketAdminFailsClosedWithoutChannel(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker"}
	b := New(testConfig(t, withManager("test-manager", ""), withRuntime(&fakeRuntime{}, spec),
		func(c *Config) { c.Dispatch.Tickets = nil }))
	if w := postWorkerTicket(t, b, `{"worker":"worker"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("no channel: status = %d, want 500", w.Code)
	}
}
