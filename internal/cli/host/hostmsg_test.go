package host

import (
	"fmt"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

func TestHostMsgSendToManager(t *testing.T) {
	dir := writeInstanceNamed(t, "assistant", managerYAML+scratchWorkerYAML)
	t.Chdir(dir)

	// Seed the controller PAT so `msg send`'s scion client authenticates with
	// it via HubTokenSource: guards that the source wiring survives (a dropped
	// source would send anonymously against the dev-auth-off hub).
	if err := state.ForConfig(dir).SaveControllerPAT("pat-hostmsg-send"); err != nil {
		t.Fatal(err)
	}

	fr := scionOKRunner()
	root := stubRoot(&stubBackend{runner: fr})
	_, err := clitest.Exec(t, root, "msg", "send", "hello", "there", "--to", "assistant")
	if err != nil {
		t.Fatalf("msg send: %v", err)
	}

	if len(fr.Calls) != 1 {
		t.Fatalf("want exactly 1 scion call, got %d: %+v", len(fr.Calls), fr.Calls)
	}
	gotArgv := append([]string{fr.Calls[0].Name}, fr.Calls[0].Args...)
	// Flags first, then `--`, then the two agent-controlled positionals — see
	// scion.Client.Message: an unterminated body of "-b" would bind to scion's
	// broadcast flag and fan the message out past the recipient check.
	wantArgv := []string{"scion", "message", "-g", "/lever", "--", "agent:assistant", "hello there"}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Fatalf("argv = %v, want %v", gotArgv, wantArgv)
		}
	}
	if got := fr.Calls[0].Env["SCION_HUB_TOKEN"]; got != "pat-hostmsg-send" {
		t.Fatalf("message env SCION_HUB_TOKEN = %q, want %q (HubTokenSource dropped)", got, "pat-hostmsg-send")
	}
}

func TestHostMsgSendToWorkerWithInterrupt(t *testing.T) {
	dir := writeInstanceNamed(t, "assistant", managerYAML+scratchWorkerYAML)
	t.Chdir(dir)

	fr := scionOKRunner()
	root := stubRoot(&stubBackend{runner: fr})
	_, err := clitest.Exec(t, root, "msg", "send", "check", "in", "--to", "scratch", "--interrupt")
	if err != nil {
		t.Fatalf("msg send: %v", err)
	}

	if len(fr.Calls) != 1 {
		t.Fatalf("want exactly 1 scion call, got %d: %+v", len(fr.Calls), fr.Calls)
	}
	got := strings.Join(fr.Calls[0].Args, " ")
	// Single-project model: the worker is an agent in the instance project
	// (/lever), addressed by slug — not a per-worker /lever/workers/<name> project.
	for _, want := range []string{"agent:scratch", "--interrupt", "-g /lever"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestHostMsgSendUnknownRecipientErrors(t *testing.T) {
	dir := writeInstanceNamed(t, "assistant", managerYAML+scratchWorkerYAML)
	t.Chdir(dir)

	fr := proc.NewFakeRunner()
	root := stubRoot(&stubBackend{runner: fr})
	_, err := clitest.Exec(t, root, "msg", "send", "hi", "--to", "nope")
	if err == nil {
		t.Fatal("want error for unknown --to")
	}
	for _, want := range []string{"nope", "assistant", "scratch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
	if len(fr.Calls) != 0 {
		t.Fatalf("scion must never be called on an unknown recipient, got %+v", fr.Calls)
	}
}

func TestHostMsgSendJailDownFailsFast(t *testing.T) {
	dir := writeInstanceNamed(t, "assistant", managerYAML+scratchWorkerYAML)
	t.Chdir(dir)

	fr := proc.NewFakeRunner()
	sb := &stubBackend{runner: fr, resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-assistant")}
	root := stubRoot(sb)
	_, err := clitest.Exec(t, root, "msg", "send", "hi", "--to", "assistant")
	if err == nil {
		t.Fatal("want error when jail is down")
	}
	if !strings.Contains(err.Error(), "lever up") {
		t.Fatalf("error should tell the operator to run `lever up`; got: %v", err)
	}
	if len(fr.Calls) != 0 {
		t.Fatal("msg send must never call scion when the jail is not up")
	}
}
