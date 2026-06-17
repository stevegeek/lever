package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestMessageArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	if err := c.Message(context.Background(), MsgOpts{To: "agent:a", Body: "hi", Interrupt: true, Project: "/g/a"}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	for _, want := range []string{"message agent:a hi", "--interrupt", "-g /g/a"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestInboxParsesNotifications(t *testing.T) {
	f := exec.NewFakeRunner()
	// `scion notifications --json` returns a bare array of typed events.
	f.Script("scion notifications --json", exec.Result{Stdout: `[{"id":"e1","status":"WAITING_FOR_INPUT"},{"id":"e2","status":"COMPLETED"}]`})
	c := New(f, Options{})
	events, err := c.Inbox(context.Background(), true, "")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(events) != 2 || events[0].ID() != "e1" || events[1]["status"] != "COMPLETED" {
		t.Fatalf("events=%+v", events)
	}
}

func TestInboxUnwrapsItems(t *testing.T) {
	f := exec.NewFakeRunner()
	// Tolerate a wrapped {"items":[...]} shape too (older/alternate scion output).
	f.Script("scion notifications --json", exec.Result{Stdout: `{"items":[{"id":"e1","status":"WAITING_FOR_INPUT"},{"id":"e2","status":"COMPLETED"}]}`})
	c := New(f, Options{})
	events, err := c.Inbox(context.Background(), true, "")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(events) != 2 || events[0].ID() != "e1" || events[1]["status"] != "COMPLETED" {
		t.Fatalf("events=%+v", events)
	}
}

func TestInboxAllFlag(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion notifications --json --all", exec.Result{Stdout: `[]`})
	c := New(f, Options{})
	if _, err := c.Inbox(context.Background(), false, ""); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if !strings.Contains(strings.Join(f.Calls[0].Args, " "), "--all") {
		t.Fatalf("expected --all; got %v", f.Calls[0].Args)
	}
}
