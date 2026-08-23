package manager

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/testutil"
)

func TestMsgSend_postsBrokerRequestAndPrintsConfirmation(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))

	out, err := clitest.Exec(t, root, "msg", "send", "hello", "--to", "scratch", "--interrupt")
	if err != nil {
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
	if !strings.Contains(out, "Sent to scratch.") {
		t.Fatalf("out=%q", out)
	}
}

func TestMsgSend_bodyIsJoinedArgs(t *testing.T) {
	var gotBody map[string]any
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))

	if _, err := clitest.Exec(t, root, "msg", "send", "--to", "scratch", "hello", "there"); err != nil {
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
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{"id": "e1", "status": "WAITING_FOR_INPUT", "message": "poet needs input"},
			},
		})
	}))

	out, err := clitest.Exec(t, root, "msg", "list", "--worker", "scratch", "--all")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotPath != "/msg/list" {
		t.Fatalf("path = %s, want /msg/list", gotPath)
	}
	want := map[string]any{"all": true, "worker": "scratch"}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body=%v)", k, gotBody[k], v, gotBody)
		}
	}
	if !strings.Contains(out, "[e1] WAITING_FOR_INPUT poet needs input") {
		t.Fatalf("out=%q", out)
	}
}

func TestMsgList_defaultFlagsAreUnreadOwnInbox(t *testing.T) {
	var gotBody map[string]any
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	}))

	if _, err := clitest.Exec(t, root, "msg", "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]any{"all": false, "worker": ""}
	for k, v := range want {
		if gotBody[k] != v {
			t.Fatalf("body[%s] = %v, want %v (body=%v)", k, gotBody[k], v, gotBody)
		}
	}
}

func TestMsgList_malformedResponseIsAnError(t *testing.T) {
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		// Valid JSON (passes the raw-message transport) but the wrong shape:
		// "events" is a string, not an array. Must surface as an error, NOT
		// render as "Inbox empty." (a silent-empty inbox hides broker faults).
		_, _ = w.Write([]byte(`{"events": "not-an-array"}`))
	}))

	out, err := clitest.Exec(t, root, "msg", "list")
	if err == nil {
		t.Fatalf("expected decode error, got nil (out=%q)", out)
	}
	testutil.WantErrContaining(t, err, "decode /msg/list response")
	if strings.Contains(out, "Inbox empty.") {
		t.Fatalf("malformed response must not render as an empty inbox; out=%q", out)
	}
}

func TestMsgList_emptyInboxPrintsFallback(t *testing.T) {
	root := newRoot(fakeBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	}))

	out, err := clitest.Exec(t, root, "msg", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out) != "Inbox empty." {
		t.Fatalf("out=%q", out)
	}
}
