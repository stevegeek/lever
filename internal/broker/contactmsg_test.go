package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/wire"
)

// The contact client@example.org lists the worker "scratch" (hub id
// chatScratchID) and the manager; d@example.org lists only "worker".
func agentMsgOpt(dir string, follow time.Duration) verifyOpt {
	return func(c *Config) {
		c.AgentMessages = AgentMessagesConfig{Enabled: true, FollowUpAfter: follow, MaxChars: 50, LedgerDir: filepath.Join(dir, "agent-ledger"),
			Contacts: []ContactEntry{
				{Login: "client@example.org", Email: "client@example.org", Agents: []string{"scratch", "assistant"}},
				{Login: "d@example.org", Email: "d@example.org", Agents: []string{"worker"}},
			}}
		// The fixture resolves only the manager and scratch; "worker" needs a hub id too.
		ids := map[string]string{"assistant": chatManagerID, "scratch": chatScratchID, "worker": "aaaaaaaa-0000-0000-0000-000000000003"}
		c.Dispatch.ResolveAgentID = func(_ context.Context, slug string) (string, error) {
			if id, ok := ids[slug]; ok {
				return id, nil
			}
			return "", errors.New("no such agent")
		}
	}
}

func (f *verifyFixture) contactMessage(t *testing.T, cn string, req wire.ContactMessageRequest) wire.ContactMessageResponse {
	t.Helper()
	raw, _ := json.Marshal(req)
	rec := callWorker(t, f.b, wire.PathContactMessage, string(raw), cn)
	if rec.Code != http.StatusOK {
		t.Fatalf("contact message = %d %s", rec.Code, rec.Body)
	}
	var out wire.ContactMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestContactsListsOnlyLoginsThatListTheCaller(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), 24*time.Hour))
	rec := callWorker(t, f.b, wire.PathContacts, `{}`, "scratch")
	var out wire.ContactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Enabled || len(out.Contacts) != 1 || out.Contacts[0].Login != "client@example.org" ||
		out.Contacts[0].To != "@client@example.org" || !out.Contacts[0].CanInitiate {
		t.Fatalf("scratch contacts = %s", rec.Body)
	}
	rec = callWorker(t, f.b, wire.PathContacts, `{}`, "manager") // the manager CN; its slug is "assistant"
	if !strings.Contains(rec.Body.String(), "client@example.org") || strings.Contains(rec.Body.String(), "d@example.org") {
		t.Fatalf("manager contacts = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "op@example.com") {
		t.Fatal("an operator login is never a contact")
	}
}

func TestContactMessageInitiateRule(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	first := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "workbook v3 is ready"})
	if !first.OK || first.Kind != agentledger.KindInitiated || first.To != "@client@example.org" || len(first.Ref) != 32 {
		t.Fatalf("first = %+v", first)
	}
	second := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "still there?"})
	if second.OK || second.Reason != "limit" || second.NextAllowedAt == "" {
		t.Fatalf("second = %+v, want limit with next_allowed_at", second)
	}
	other := f.contactMessage(t, "worker", wire.ContactMessageRequest{To: "d@example.org", Text: "hello"})
	if !other.OK {
		t.Fatalf("the limit is per agent and contact: %+v", other)
	}
}

func TestInitiateDecision(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		initiated []time.Time
		last      time.Time
		ok        bool
		next      time.Time
	}{
		"none":               {nil, time.Time{}, true, time.Time{}},
		"one fresh":          {[]time.Time{now.Add(-time.Hour)}, time.Time{}, false, now.Add(23 * time.Hour)},
		"one old = reminder": {[]time.Time{now.Add(-25 * time.Hour)}, time.Time{}, true, time.Time{}},
		"two":                {[]time.Time{now.Add(-50 * time.Hour), now.Add(-25 * time.Hour)}, time.Time{}, false, time.Time{}},
		"answered resets":    {[]time.Time{now.Add(-50 * time.Hour), now.Add(-25 * time.Hour)}, now.Add(-time.Hour), true, time.Time{}},
		"answered then one":  {[]time.Time{now.Add(-50 * time.Hour), now.Add(-30 * time.Minute)}, now.Add(-time.Hour), false, now.Add(-30*time.Minute + day)},
	} {
		ok, next := initiateDecision(tc.initiated, tc.last, day, now)
		if ok != tc.ok || !next.Equal(tc.next) {
			t.Errorf("%s: ok=%v next=%v, want %v %v", name, ok, next, tc.ok, tc.next)
		}
	}
}

func TestContactMessageRefusals(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	for name, tc := range map[string]struct {
		cn   string
		req  wire.ContactMessageRequest
		want string
	}{
		"not listing": {"scratch", wire.ContactMessageRequest{To: "d@example.org", Text: "hi"}, "not-a-contact"},
		"operator":    {"scratch", wire.ContactMessageRequest{To: "op@example.com", Text: "hi"}, "not-a-contact"},
		"unknown":     {"scratch", wire.ContactMessageRequest{To: "x@example.org", Text: "hi"}, "not-a-contact"},
		"at form":     {"scratch", wire.ContactMessageRequest{To: "@client@example.org", Text: "hi"}, "not-a-contact"},
		"empty":       {"scratch", wire.ContactMessageRequest{To: "client@example.org", Text: " \n\t"}, "empty"},
		"too long":    {"scratch", wire.ContactMessageRequest{To: "client@example.org", Text: strings.Repeat("é", 51)}, "too-long"},
		"bad ref":     {"scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "hi", ReplyToRef: "nope"}, "bad-ref"},
	} {
		if got := f.contactMessage(t, tc.cn, tc.req); got.OK || got.Reason != tc.want {
			t.Errorf("%s: %+v, want %s", name, got, tc.want)
		}
	}
	if !strings.Contains(f.audit.String(), "not-a-contact") || strings.Contains(f.audit.String(), "é") {
		t.Fatal("refusals are audited with their word and never with the text")
	}
}

func TestContactMessageRefusesTextTheHubWouldChange(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	for _, text := range []string{"ask @client@example.org", "(@x) hi", "＠x hi", "@start", "a\x00b", "bell\x07", string([]byte{0xff, 'a'})} {
		if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: text}); got.OK || got.Reason != "bad-text" {
			t.Errorf("%q: %+v, want bad-text", text, got)
		}
	}
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "mail bob@example.com\n\tok"}); !got.OK {
		t.Fatalf("an email inside a word is not a mention: %+v", got)
	}
}

func TestContactMessageReplyNeedsAVerifiedContactPost(t *testing.T) {
	post := contactEntry(chatScratchID, "is v3 ready?", "11111111-1111-1111-1111-111111111111")
	f := verifyBroker(t, []chatledger.Entry{post}, agentMsgOpt(t.TempDir(), time.Hour))
	// Not verified yet: bad-ref.
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "yes", ReplyToRef: post.MessageID}); got.Reason != "bad-ref" {
		t.Fatalf("unverified ref: %+v", got)
	}
	f.verify(t, "scratch", wire.MessageVerifyRequest{Timestamp: chatTS, From: contactSender})
	for i := 0; i < 3; i++ { // replies skip the initiate rule
		if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "yes", ReplyToRef: post.MessageID}); !got.OK || got.Kind != agentledger.KindReply {
			t.Fatalf("reply %d: %+v", i, got)
		}
	}
	// Another agent cannot use scratch's ref.
	if got := f.contactMessage(t, "manager", wire.ContactMessageRequest{To: "client@example.org", Text: "yes", ReplyToRef: post.MessageID}); got.Reason != "bad-ref" {
		t.Fatalf("foreign ref: %+v", got)
	}
}

func TestContactMessageHourlyRateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	post := contactEntry(chatScratchID, "q", "22222222-2222-2222-2222-222222222222")
	f := verifyBroker(t, []chatledger.Entry{post}, agentMsgOpt(dir, time.Hour))
	f.verify(t, "scratch", wire.MessageVerifyRequest{Timestamp: chatTS, From: contactSender})
	for i := 0; i < 30; i++ {
		if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "r", ReplyToRef: post.MessageID}); !got.OK {
			t.Fatalf("reply %d: %+v", i, got)
		}
	}
	g := verifyBroker(t, nil, agentMsgOpt(dir, time.Hour)) // a new broker on the same ledger
	if got := g.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "new"}); got.Reason != "rate" {
		t.Fatalf("31st authorization in an hour: %+v, want rate", got)
	}
}

func TestContactMessageOff(t *testing.T) {
	f := verifyBroker(t, nil)
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "hi"}); got.OK || got.Reason != "off" {
		t.Fatalf("off: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.ledger), "agent-ledger")); err == nil {
		t.Fatal("off must write no ledger")
	}
}

// The initiate rule is read from the ledger, so a new broker on the same
// state keeps it (spec security property 3).
func TestContactMessageInitiateRuleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f := verifyBroker(t, nil, agentMsgOpt(dir, time.Hour))
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "first"}); !got.OK {
		t.Fatalf("first: %+v", got)
	}
	g := verifyBroker(t, nil, agentMsgOpt(dir, time.Hour))
	if got := g.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "second"}); got.OK || got.Reason != "limit" {
		t.Fatalf("after a restart: %+v, want limit", got)
	}
}

// A contact's post (in the chat ledger, after the agent's message) lets the
// agent start a new one.
func TestContactMessageContactPostResetsTheLimit(t *testing.T) {
	f := verifyBroker(t, nil, agentMsgOpt(t.TempDir(), time.Hour))
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "first"}); !got.OK {
		t.Fatalf("first: %+v", got)
	}
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "second"}); got.Reason != "limit" {
		t.Fatalf("second: %+v, want limit", got)
	}
	// A post to another agent does not reset scratch's limit.
	other := contactEntry(chatManagerID, "to the manager", "44444444-4444-4444-4444-444444444444")
	other.Recorded = time.Now().UTC().Add(time.Second)
	if err := chatledger.NewWriter(f.ledger).Append(other); err != nil {
		t.Fatal(err)
	}
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "second"}); got.Reason != "limit" {
		t.Fatalf("after a post to another agent: %+v, want limit", got)
	}
	post := contactEntry(chatScratchID, "thanks", "33333333-3333-3333-3333-333333333333")
	post.Recorded = time.Now().UTC().Add(time.Second)
	if err := chatledger.NewWriter(f.ledger).Append(post); err != nil {
		t.Fatal(err)
	}
	if got := f.contactMessage(t, "scratch", wire.ContactMessageRequest{To: "client@example.org", Text: "third"}); !got.OK {
		t.Fatalf("after the contact wrote: %+v", got)
	}
}
