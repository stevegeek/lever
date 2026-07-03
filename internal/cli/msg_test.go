package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withFakeMsgBroker points msgCallFn at a test broker for the duration of the
// test, decoding each request body into a map so assertions can inspect it.
func withFakeMsgBroker(t *testing.T, handle func(w http.ResponseWriter, gotPath string, gotBody map[string]any)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		handle(w, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)

	oldCall := msgCallFn
	msgCallFn = func(ctx context.Context, endpoint string, body any) (json.RawMessage, error) {
		return postBroker[json.RawMessage](ctx, srv.Client(), srv.URL, endpoint, body)
	}
	t.Cleanup(func() { msgCallFn = oldCall })
}

func TestMsgSend_postsBrokerRequestAndPrintsConfirmation(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	withFakeMsgBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	root := newManagerRootWith()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"msg", "send", "hello", "--to", "scratch", "--interrupt"})
	if err := root.Execute(); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotPath != "/msg/send" {
		t.Fatalf("path = %s, want /msg/send", gotPath)
	}
	want := map[string]any{"to": "scratch", "body": "hello", "interrupt": true}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body=%v)", k, gotBody[k], v, gotBody)
		}
	}
	if !strings.Contains(out.String(), "Sent to scratch.") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestMsgSend_bodyIsJoinedArgs(t *testing.T) {
	var gotBody map[string]any
	withFakeMsgBroker(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	root := newManagerRootWith()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"msg", "send", "--to", "scratch", "hello", "there"})
	if err := root.Execute(); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotBody["body"] != "hello there" {
		t.Fatalf("body = %v, want %q", gotBody["body"], "hello there")
	}
	if gotBody["interrupt"] != false {
		t.Fatalf("interrupt = %v, want false", gotBody["interrupt"])
	}
}

func TestMsgList_postsBrokerRequestAndRendersEvents(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	withFakeMsgBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{"id": "e1", "status": "WAITING_FOR_INPUT", "message": "poet needs input"},
			},
		})
	})

	root := newManagerRootWith()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"msg", "list", "--grove", "scratch", "--all"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotPath != "/msg/list" {
		t.Fatalf("path = %s, want /msg/list", gotPath)
	}
	want := map[string]any{"all": true, "grove": "scratch"}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body=%v)", k, gotBody[k], v, gotBody)
		}
	}
	if !strings.Contains(out.String(), "[e1] WAITING_FOR_INPUT poet needs input") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestMsgList_defaultFlagsAreUnreadOwnInbox(t *testing.T) {
	var gotBody map[string]any
	withFakeMsgBroker(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	})

	root := newManagerRootWith()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"msg", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]any{"all": false, "grove": ""}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body=%v)", k, gotBody[k], v, gotBody)
		}
	}
}

func TestMsgList_emptyInboxPrintsFallback(t *testing.T) {
	withFakeMsgBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	})

	root := newManagerRootWith()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"msg", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out.String()) != "Inbox empty." {
		t.Fatalf("out=%q", out.String())
	}
}
