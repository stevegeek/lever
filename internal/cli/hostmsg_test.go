package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
)

// hostMsgInstanceDir writes a canonical lever.yaml declaring a "scratch" grove
// alongside the manager, so --to can resolve both branches of attachTarget.
func hostMsgInstanceDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "name: " + name + `
backend: orbstack
tree: workspace
broker:
  llm_auth: subscription
manager:
  image: img:1
groves:
  - name: scratch
    dir: groves/scratch
`
	if err := os.WriteFile(filepath.Join(dir, config.CanonicalName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHostMsgSendToManager(t *testing.T) {
	dir := hostMsgInstanceDir(t, "assistant")
	t.Chdir(dir)

	fr := leverexec.NewFakeRunner()
	fr.Script("scion", leverexec.Result{})
	sb := &stubBackend{runner: fr}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	root.SetArgs([]string{"msg", "send", "hello", "there", "--to", "assistant"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	if err := root.Execute(); err != nil {
		t.Fatalf("msg send: %v", err)
	}

	if len(fr.Calls) != 1 {
		t.Fatalf("want exactly 1 scion call, got %d: %+v", len(fr.Calls), fr.Calls)
	}
	gotArgv := append([]string{fr.Calls[0].Name}, fr.Calls[0].Args...)
	wantArgv := []string{"scion", "message", "agent:assistant", "hello there", "-g", "/lever"}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Fatalf("argv = %v, want %v", gotArgv, wantArgv)
		}
	}
}

func TestHostMsgSendToGroveWithInterrupt(t *testing.T) {
	dir := hostMsgInstanceDir(t, "assistant")
	t.Chdir(dir)

	fr := leverexec.NewFakeRunner()
	fr.Script("scion", leverexec.Result{})
	sb := &stubBackend{runner: fr}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	root.SetArgs([]string{"msg", "send", "check", "in", "--to", "scratch", "--interrupt"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	if err := root.Execute(); err != nil {
		t.Fatalf("msg send: %v", err)
	}

	if len(fr.Calls) != 1 {
		t.Fatalf("want exactly 1 scion call, got %d: %+v", len(fr.Calls), fr.Calls)
	}
	got := strings.Join(fr.Calls[0].Args, " ")
	for _, want := range []string{"agent:scratch", "--interrupt", "-g /lever/groves/scratch"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestHostMsgSendUnknownRecipientErrors(t *testing.T) {
	dir := hostMsgInstanceDir(t, "assistant")
	t.Chdir(dir)

	fr := leverexec.NewFakeRunner()
	sb := &stubBackend{runner: fr}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	root.SetArgs([]string{"msg", "send", "hi", "--to", "nope"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	err := root.Execute()
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
	dir := hostMsgInstanceDir(t, "assistant")
	t.Chdir(dir)

	fr := leverexec.NewFakeRunner()
	sb := &stubBackend{runner: fr, resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-assistant")}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	root.SetArgs([]string{"msg", "send", "hi", "--to", "assistant"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	err := root.Execute()
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
