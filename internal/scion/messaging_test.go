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

func TestInboxUnwrapsItems(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion messages --json", exec.Result{Stdout: `{"items":[{"id":"e1","type":"input-needed"},{"id":"e2","type":"state-change"}]}`})
	c := New(f, Options{})
	events, err := c.Inbox(context.Background(), true, "")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(events) != 2 || events[0].ID() != "e1" || events[1]["type"] != "state-change" {
		t.Fatalf("events=%+v", events)
	}
}

func TestInboxAllFlag(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion messages --json --all", exec.Result{Stdout: `{"items":[]}`})
	c := New(f, Options{})
	if _, err := c.Inbox(context.Background(), false, ""); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if !strings.Contains(strings.Join(f.Calls[0].Args, " "), "--all") {
		t.Fatalf("expected --all; got %v", f.Calls[0].Args)
	}
}
