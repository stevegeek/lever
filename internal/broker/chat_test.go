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

// TestChatVerifyBadInput: a timestamp that is not RFC 3339, or a missing
// from, is a 400, never "not verified".
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
		if status, _, raw := postChatVerify(t, client, srv.URL, req); status != http.StatusBadRequest {
			t.Errorf("verify %+v = %d %s, want 400", req, status, raw)
		}
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
	status, _, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender})
	if status != http.StatusBadGateway || strings.Contains(raw, "deploy the fix") {
		t.Fatalf("verify = %d %s, want 502 and no text", status, raw)
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
	if status, _, raw := postChatVerify(t, client, srv.URL, wire.ChatVerifyRequest{Timestamp: chatTS, From: chatSender}); status != http.StatusBadGateway {
		t.Fatalf("verify = %d %s, want 502", status, raw)
	}
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
