package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestBrokerInboxer_postsFullInboxRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := fakeBroker(t, func(w http.ResponseWriter, path string, body map[string]any) {
		gotPath, gotBody = path, body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{"id": "e1", "status": "WAITING_FOR_INPUT", "message": "hi"},
			},
		})
	})

	// Mirrors how bridge.Bridge.PollOnce calls Inbox: unread=false, project="".
	events, err := newBrokerInboxer(c).Inbox(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/msg/list" {
		t.Fatalf("path = %s, want /msg/list", gotPath)
	}
	want := map[string]any{"all": true, "worker": ""}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}
	if len(events) != 1 || events[0].ID() != "e1" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBrokerInboxer_unreadTrueRequestsAllFalse(t *testing.T) {
	var gotBody map[string]any
	c := fakeBroker(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
	})

	if _, err := newBrokerInboxer(c).Inbox(context.Background(), true, "worker"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"all": false, "worker": "worker"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}
}

func TestBrokerInboxer_malformedResponseIsAnError(t *testing.T) {
	c := fakeBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		// Valid JSON but the wrong shape ("events" not an array): the adapter
		// must return the decode error so bridge.PollOnce fails loudly instead
		// of treating a broken broker as "no new events" forever.
		_, _ = w.Write([]byte(`{"events": 42}`))
	})

	events, err := newBrokerInboxer(c).Inbox(context.Background(), false, "")
	if err == nil {
		t.Fatalf("expected decode error, got nil (events=%v)", events)
	}
}

func TestWatchCmd_requiresEventsFile(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"watch"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing --events-file")
	}
}
