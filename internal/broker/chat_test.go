package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/wire"
)

// Hub agent ids of the chat fixtures: the manager (slug "assistant") and the
// worker "scratch".
const (
	chatManagerID = "aaaaaaaa-0000-0000-0000-000000000001"
	chatScratchID = "aaaaaaaa-0000-0000-0000-000000000002"
	chatSender    = "user:op@example.com"
	chatTS        = "2026-09-28T10:15:02Z"
)

// chatBroker builds a broker with verified chat reading ledger (""= off),
// the msgBroker identities, and an agent id resolver over the two fixtures.
func chatBroker(t *testing.T, ledger string) (*Broker, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	ids := map[string]string{"assistant": chatManagerID, "scratch": chatScratchID}
	b := New(testConfig(t, withAudit(&buf), withManager("manager", "assistant"), withRuntime(nil, msgWorkers...),
		func(c *Config) {
			c.Chat.LedgerPath = ledger
			c.Dispatch.ResolveAgentID = func(_ context.Context, slug string) (string, error) {
				if id, ok := ids[slug]; ok {
					return id, nil
				}
				return "", errors.New("no such agent")
			}
		}))
	return b, &buf
}

// seedLedger writes one entry per (agent id, text) at chatTS from chatSender.
func seedLedger(t *testing.T, entries ...chatledger.Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	w := chatledger.NewWriter(p)
	for _, e := range entries {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func ledgerEntry(agentID, text, id string) chatledger.Entry {
	return chatledger.Entry{Recorded: time.Now().UTC(), Login: "op@example.com", Tier: chatledger.TierOperator,
		Conversation: "dm:agent:" + agentID + ":user:u1", AgentID: agentID, MessageID: id,
		Sender: chatSender, CreatedAt: chatTS, Text: text}
}

func postChatVerify(t *testing.T, client *http.Client, url string, body any) (int, wire.ChatVerifyResponse, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := client.Post(url+wire.PathChatVerify, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	var out wire.ChatVerifyResponse
	_ = json.Unmarshal(got, &out)
	return resp.StatusCode, out, string(got)
}

// TestChatVerifyReturnsTheRecordedText: a message the proxy recorded for the
// caller's own DM verifies, and the answer carries the text as posted.
func TestChatVerifyReturnsTheRecordedText(t *testing.T) {
	ledger := seedLedger(t, ledgerEntry(chatManagerID, "deploy the fix", "m1"))
	b, audit := chatBroker(t, ledger)
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	// An offset timestamp and fractional seconds name the same second.
	status, resp, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: "2026-09-28T12:15:02.5+02:00", From: chatSender})
	if status != http.StatusOK || !resp.Enabled || !resp.Verified || len(resp.Messages) != 1 {
		t.Fatalf("verify = %d %s, want one verified message", status, raw)
	}
	m := resp.Messages[0]
	if m.Text != "deploy the fix" || m.Login != "op@example.com" || m.Tier != chatledger.TierOperator ||
		m.From != chatSender || m.Timestamp != chatTS || m.MessageID != "m1" {
		t.Fatalf("message = %+v", m)
	}
	if !strings.Contains(audit.String(), "allow") || !strings.Contains(audit.String(), "m1") {
		t.Fatalf("audit does not record the allow: %s", audit)
	}
}

// TestChatVerifyIsBoundToTheCallersOwnDM: a message recorded for another
// agent's DM does not verify for the caller — a worker cannot read the
// manager's chat, nor the reverse — and a wrong sender or second does not
// verify either.
func TestChatVerifyIsBoundToTheCallersOwnDM(t *testing.T) {
	ledger := seedLedger(t, ledgerEntry(chatManagerID, "manager only", "m1"))
	b, audit := chatBroker(t, ledger)
	srv := jailServer(t, b)
	defer srv.Close()

	cases := []struct {
		name, cn string
		req      wire.ChatVerifyRequest
	}{
		{"worker asks about the manager's message", "scratch", wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender}},
		{"other sender", "manager", wire.ChatVerifyRequest{Timestamp: chatTS, From: "user:someone@example.com"}},
		{"other second", "manager", wire.ChatVerifyRequest{Timestamp: "2026-09-28T10:15:03Z", From: chatSender}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := agentClient(t, b, signedCert(t, b, tc.cn))
			status, resp, raw := postChatVerify(t, client, srv.URL, tc.req)
			if status != http.StatusOK || !resp.Enabled || resp.Verified || len(resp.Messages) != 0 {
				t.Fatalf("verify = %d %s, want enabled and not verified", status, raw)
			}
			if strings.Contains(raw, "manager only") {
				t.Fatalf("the answer leaks another message's text: %s", raw)
			}
		})
	}
	if !strings.Contains(audit.String(), "no record") {
		t.Fatalf("audit does not record the denials: %s", audit)
	}
}

// TestChatVerifyWhenOff: with no ledger configured the route says so, so the
// agent can tell "off here" from "not verified".
func TestChatVerifyWhenOff(t *testing.T) {
	b, _ := chatBroker(t, "")
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	status, resp, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender})
	if status != http.StatusOK || resp.Enabled || resp.Verified || resp.Note == "" {
		t.Fatalf("verify = %d %s, want enabled=false with a note", status, raw)
	}
}

// assertUnverified: every failure once verified chat is on answers 200,
// enabled, not verified, with a note — never an error an agent could read as
// "the old rules apply".
func assertUnverified(t *testing.T, what string, status int, resp wire.ChatVerifyResponse, raw string) {
	t.Helper()
	if status != http.StatusOK || !resp.Enabled || resp.Verified || resp.Note == "" || len(resp.Messages) != 0 {
		t.Fatalf("%s: verify = %d %s, want 200 enabled, not verified, with a note", what, status, raw)
	}
}

// TestChatVerifyBadInput: a timestamp that is not RFC 3339, or a missing
// from, answers "not verified".
func TestChatVerifyBadInput(t *testing.T) {
	b, _ := chatBroker(t, seedLedger(t))
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	for _, req := range []wire.ChatVerifyRequest{
		{Timestamp: "yesterday", From: chatSender},
		{Timestamp: chatTS},
		{Timestamp: chatTS, From: strings.Repeat("x", maxChatFromLen+1)},
	} {
		status, resp, raw := postChatVerify(t, client, srv.URL, req)
		assertUnverified(t, "bad input", status, resp, raw)
	}
}

// TestChatVerifyRefusesAnUnsafeLedger: a ledger another user can write
// proves nothing, so the broker answers an error, not "verified".
func TestChatVerifyRefusesAnUnsafeLedger(t *testing.T) {
	ledger := seedLedger(t, ledgerEntry(chatManagerID, "deploy the fix", "m1"))
	if err := os.Chmod(ledger, 0o666); err != nil {
		t.Fatal(err)
	}
	b, _ := chatBroker(t, ledger)
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	status, resp, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender})
	assertUnverified(t, "unsafe ledger", status, resp, raw)
	if strings.Contains(raw, "deploy the fix") {
		t.Fatalf("the answer carries the text: %s", raw)
	}
}

// TestChatVerifyNeedsTheAgentIDResolver: without a way to learn the caller's
// hub id there is no safe answer.
func TestChatVerifyNeedsTheAgentIDResolver(t *testing.T) {
	b, _ := chatBroker(t, seedLedger(t, ledgerEntry(chatManagerID, "x", "m1")))
	b.resolveAgentID = nil
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	status, resp, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender})
	assertUnverified(t, "no resolver", status, resp, raw)
}

// TestChatVerifyRevokedCallerDenied: the route has the same mTLS and
// revocation preamble as every jail route.
func TestChatVerifyRevokedCallerDenied(t *testing.T) {
	b, _ := chatBroker(t, seedLedger(t, ledgerEntry(chatManagerID, "x", "m1")))
	b.revoked["manager"] = true
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	if status, _, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender}); status != http.StatusForbidden {
		t.Fatalf("verify = %d %s, want 403", status, raw)
	}
}

// TestChatVerifyOnceOnly: a message verifies once. A later copy of the same
// envelope (quoted in an email or a worker's message) cannot re-use an old
// approval.
func TestChatVerifyOnceOnly(t *testing.T) {
	b, audit := chatBroker(t, seedLedger(t, ledgerEntry(chatManagerID, "yes, go ahead", "m1")))
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	req := wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender}
	if _, resp, raw := postChatVerify(t, client, srv.URL, req); !resp.Verified {
		t.Fatalf("first verify = %s, want verified", raw)
	}
	// Within the grace period a repeat verifies, marked as a repeat.
	if _, resp, raw := postChatVerify(t, client, srv.URL, req); !resp.Verified || !resp.Messages[0].Repeat || resp.Messages[0].FirstVerified == "" {
		t.Fatalf("repeat verify = %s, want verified with repeat=true", raw)
	}
	// After it, the message no longer verifies.
	k := "manager\x00m1"
	b.chatUses.mu.Lock()
	b.chatUses.used[k] = time.Now().Add(-chatRepeatGrace - time.Minute)
	b.chatUses.mu.Unlock()
	status, resp, raw := postChatVerify(t, client, srv.URL, req)
	assertUnverified(t, "late second verify", status, resp, raw)
	if !strings.Contains(resp.Note, "already verified") || strings.Contains(raw, "go ahead") {
		t.Fatalf("late verify = %s, want 'already verified' and no text", raw)
	}
	if !strings.Contains(audit.String(), "already verified") {
		t.Fatalf("audit does not record the replay: %s", audit)
	}
}

// TestChatVerifyRefusesAnOldRecord: a post recorded more than
// chatVerifyWindow ago no longer verifies.
func TestChatVerifyRefusesAnOldRecord(t *testing.T) {
	old := ledgerEntry(chatManagerID, "push it", "m1")
	old.Recorded = time.Now().Add(-chatVerifyWindow - time.Minute)
	b, _ := chatBroker(t, seedLedger(t, old))
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	status, resp, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender})
	assertUnverified(t, "old record", status, resp, raw)
	if !strings.Contains(resp.Note, "older than") {
		t.Fatalf("note %q does not say why", resp.Note)
	}
}

// TestChatVerifyRateLimitAnswersUnverified: exhausting the rate limit must
// not turn into an error an agent reads as "rules as before".
func TestChatVerifyRateLimitAnswersUnverified(t *testing.T) {
	b, _ := chatBroker(t, seedLedger(t))
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	req := wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender}
	for i := 0; i < directiveRateLimit; i++ {
		postChatVerify(t, client, srv.URL, req)
	}
	status, resp, raw := postChatVerify(t, client, srv.URL, req)
	assertUnverified(t, "rate limited", status, resp, raw)
	if !strings.Contains(resp.Note, "too many") {
		t.Fatalf("note %q", resp.Note)
	}
}

// TestChatUsesSurviveARestart: a broker restart must not re-open a message
// that was verified inside its window.
func TestChatUsesSurviveARestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-verified.jsonl")
	now := time.Now()
	u := newChatUses(p, now)
	if _, fresh, err := u.take("manager", "m1", now, now); !fresh || err != nil {
		t.Fatalf("first take refused: %v", err)
	}
	u2 := newChatUses(p, now.Add(time.Minute))
	if _, fresh, _ := u2.take("manager", "m1", now, now.Add(time.Minute)); fresh {
		t.Fatal("a restarted broker verified m1 again")
	}
	if _, fresh, _ := u2.take("scratch", "m1", now, now.Add(time.Minute)); !fresh {
		t.Fatal("uses are per caller")
	}
	// Uses older than twice the window are dropped at load.
	u3 := newChatUses(p, now.Add(3*chatVerifyWindow))
	if len(u3.used) != 0 {
		t.Fatalf("stale uses kept: %v", u3.used)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("uses file: %v, %v", fi, err)
	}
}

// TestChatUsesSeeAnotherBroker: a use recorded by a second broker process
// after this one started still counts.
func TestChatUsesSeeAnotherBroker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-verified.jsonl")
	now := time.Now()
	a, b := newChatUses(p, now), newChatUses(p, now)
	if _, fresh, err := a.take("manager", "m1", now, now); !fresh || err != nil {
		t.Fatal("first take refused")
	}
	if _, fresh, _ := b.take("manager", "m1", now, now); fresh {
		t.Fatal("the second broker verified m1 again")
	}
}

// TestChatUsesFailClosed: a use that cannot be written is not granted, and
// an unreadable record is neither truncated nor trusted.
func TestChatUsesFailClosed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chat-verified.jsonl")
	now := time.Now()
	if err := os.WriteFile(p, []byte(`{"caller":"manager","id":"m1","at":"`+now.Format(time.RFC3339Nano)+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	u := newChatUses(p, now)
	_ = os.Chmod(p, 0o600)
	if b, _ := os.ReadFile(p); len(b) == 0 {
		t.Fatal("an unreadable record was truncated")
	}
	// Entries recorded before the start are refused while degraded.
	if _, _, err := u.take("manager", "m2", now.Add(-time.Minute), now); err == nil {
		t.Fatal("a degraded broker verified an entry recorded before it started")
	}
	// A write failure refuses the use.
	w := newChatUses(filepath.Join(dir, "missing-dir", "chat-verified.jsonl"), now)
	if _, fresh, err := w.take("manager", "m3", now, now); fresh || !errors.Is(err, errUseNotRecorded) {
		t.Fatalf("fresh=%v err=%v, want the use refused", fresh, err)
	}
}
