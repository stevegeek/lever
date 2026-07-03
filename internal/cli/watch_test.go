package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestBrokerInboxer_postsFullInboxRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{"id": "e1", "status": "WAITING_FOR_INPUT", "message": "hi"},
			},
		})
	}))
	defer srv.Close()

	oldCall := msgCallFn
	msgCallFn = func(ctx context.Context, endpoint string, body any) (json.RawMessage, error) {
		return postBroker[json.RawMessage](ctx, srv.Client(), srv.URL, endpoint, body)
	}
	defer func() { msgCallFn = oldCall }()

	// Mirrors how bridge.Bridge.PollOnce calls Inbox: unread=false, project="".
	events, err := newBrokerInboxer().Inbox(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/msg/list" {
		t.Fatalf("path = %s, want /msg/list", gotPath)
	}
	want := map[string]any{"all": true, "grove": ""}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}
	if len(events) != 1 || events[0].ID() != "e1" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBrokerInboxer_unreadTrueRequestsAllFalse(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	}))
	defer srv.Close()

	oldCall := msgCallFn
	msgCallFn = func(ctx context.Context, endpoint string, body any) (json.RawMessage, error) {
		return postBroker[json.RawMessage](ctx, srv.Client(), srv.URL, endpoint, body)
	}
	defer func() { msgCallFn = oldCall }()

	if _, err := newBrokerInboxer().Inbox(context.Background(), true, "worker"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"all": false, "grove": "worker"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}
}

func TestWatchCmd_requiresEventsFile(t *testing.T) {
	root := newManagerRootWith()
	root.SetArgs([]string{"watch"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing --events-file")
	}
}
