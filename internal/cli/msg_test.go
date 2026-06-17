package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestMsgSend(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"msg", "send", "--to", "agent:appa", "--project", "/g/appa", "hello there"})
	if err := root.Execute(); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "message agent:appa hello there") || !strings.Contains(got, "-g /g/appa") {
		t.Fatalf("argv=%q", got)
	}
}

func TestMsgList(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion notifications --json", exec.Result{Stdout: `[{"id":"e1","status":"WAITING_FOR_INPUT","message":"poet needs input"}]`})
	root := newManagerRootWith(clientWith(f))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"msg", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "WAITING_FOR_INPUT") {
		t.Fatalf("out=%q", out.String())
	}
}
