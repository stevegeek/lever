package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/wire"
)

func postMatch(t *testing.T, b *Broker, req wire.AgentMessagesMatchRequest) (*httptest.ResponseRecorder, wire.AgentMessagesMatchResponse) {
	t.Helper()
	raw, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, wire.PathOperatorAgentMessagesMatch, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	b.OperatorHandler().ServeHTTP(rec, r)
	var out wire.AgentMessagesMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestOperatorMatchKeepsOnlyRecordedText(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	ok := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "v3 is ready"})
	now := time.Now().UTC()
	rec, out := postMatch(t, f.b, wire.AgentMessagesMatchRequest{Contact: "client@example.org", Agent: "scratch", Messages: []wire.AgentMessageRef{
		{ID: "11111111-0000-0000-0000-000000000001", SHA256: agentledger.HashText("v3 is ready"), CreatedAt: now},
		{ID: "11111111-0000-0000-0000-000000000002", SHA256: agentledger.HashText("unrecorded"), CreatedAt: now},
	}})
	if rec.Code != http.StatusOK || len(out.Keep) != 1 || out.Keep[0] != "11111111-0000-0000-0000-000000000001" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(f.audit.String(), ok.Ref) {
		t.Fatal("a new binding is audited with its record id")
	}
}

func TestOperatorMatchRefusals(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	for name, req := range map[string]wire.AgentMessagesMatchRequest{
		"not a contact":    {Contact: "op@example.com", Agent: "scratch"},
		"agent not listed": {Contact: "d@example.org", Agent: "scratch"},
		"too many":         {Contact: "client@example.org", Agent: "scratch", Messages: make([]wire.AgentMessageRef, 201)},
	} {
		if rec, _ := postMatch(t, f.b, req); rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	off := verifyBroker(t, nil)
	if rec, _ := postMatch(t, off.b, wire.AgentMessagesMatchRequest{Contact: "client@example.org", Agent: "scratch"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("off: %d", rec.Code)
	}
}

func TestMatchRouteIsOnlyOnTheOperatorSocket(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	if rec := callWorker(t, f.b, wire.PathOperatorAgentMessagesMatch, `{}`, "scratch"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("jail listener answered %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodPost, wire.PathOperatorAgentMessagesMatch, strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	f.b.AdminHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("admin listener answered %d", rec.Code)
	}
}

// TestOperatorMatchPeekBindsNothing: two rows of the same text, one record.
// A peek that sees only the later row would keep it, but binds nothing, so
// the contact's later read still binds the earliest.
func TestOperatorMatchPeekBindsNothing(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "v3 is ready"})
	now := time.Now().UTC()
	m1 := wire.AgentMessageRef{ID: "11111111-0000-0000-0000-000000000001", SHA256: agentledger.HashText("v3 is ready"), CreatedAt: now}
	m2 := wire.AgentMessageRef{ID: "11111111-0000-0000-0000-000000000002", SHA256: agentledger.HashText("v3 is ready"), CreatedAt: now.Add(time.Second)}
	audit := f.audit.String()
	rec, out := postMatch(t, f.b, wire.AgentMessagesMatchRequest{Contact: "client@example.org", Agent: "scratch", Peek: true, Messages: []wire.AgentMessageRef{m2}})
	if rec.Code != http.StatusOK || len(out.Keep) != 1 || out.Keep[0] != m2.ID || len(out.Pending) != 1 || out.Pending[0] != m2.ID {
		t.Fatalf("peek: %d %s, want the row the contact's read would keep, pending", rec.Code, rec.Body)
	}
	if f.audit.String() != audit {
		t.Fatal("a peek binds nothing, so it audits no binding")
	}
	rec, out = postMatch(t, f.b, wire.AgentMessagesMatchRequest{Contact: "client@example.org", Agent: "scratch", Messages: []wire.AgentMessageRef{m2, m1}})
	if rec.Code != http.StatusOK || len(out.Keep) != 1 || out.Keep[0] != m1.ID || out.Pending != nil {
		t.Fatalf("the contact's read after the peek: %d %s, want the earliest bound", rec.Code, rec.Body)
	}
	// A peek after the binding: bound, so not pending.
	rec, out = postMatch(t, f.b, wire.AgentMessagesMatchRequest{Contact: "client@example.org", Agent: "scratch", Peek: true, Messages: []wire.AgentMessageRef{m2, m1}})
	if rec.Code != http.StatusOK || len(out.Keep) != 1 || out.Keep[0] != m1.ID || len(out.Pending) != 0 {
		t.Fatalf("peek after the binding: %d %s", rec.Code, rec.Body)
	}
}
