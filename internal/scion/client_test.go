package scion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestRunInjectsEnvAndBin(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list", proc.Result{Stdout: "[]"})
	c := New(f, Options{Bin: "scion", HubEndpoint: "http://127.0.0.1:8080", HubTokenSource: func() string { return "pat123" }})
	if _, err := c.run(context.Background(), "", "list"); err != nil {
		t.Fatalf("run: %v", err)
	}
	call := f.Calls[0]
	if call.Name != "scion" || call.Args[0] != "list" {
		t.Fatalf("argv=%+v", call)
	}
	if call.Env["SCION_HUB_ENDPOINT"] != "http://127.0.0.1:8080" || call.Env["SCION_HUB_TOKEN"] != "pat123" {
		t.Fatalf("env=%+v", call.Env)
	}
}

func TestEnvAlwaysEnablesHub(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080"})
	if got := c.env()["SCION_HUB_ENABLED"]; got != "true" {
		t.Fatalf("expected SCION_HUB_ENABLED=true, got %q", got)
	}
}

// TestEnvEmitsLazyHubTokenSource: HubTokenSource is read at call time.
func TestEnvEmitsLazyHubTokenSource(t *testing.T) {
	f := proc.NewFakeRunner()
	tok := ""
	c := New(f, Options{HubTokenSource: func() string { return tok }})
	if _, ok := c.env()["SCION_HUB_TOKEN"]; ok {
		t.Fatalf("expected no SCION_HUB_TOKEN key before the mint, got %q", c.env()["SCION_HUB_TOKEN"])
	}
	tok = "dyn"
	if got := c.env()["SCION_HUB_TOKEN"]; got != "dyn" {
		t.Fatalf("expected SCION_HUB_TOKEN=dyn (read at call time), got %q", got)
	}
}

// TestEnvOmitsHubTokenWhenUnset: no HubTokenSource means no SCION_HUB_TOKEN
// key at all, not even an empty one.
func TestEnvOmitsHubTokenWhenUnset(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080"})
	if _, ok := c.env()["SCION_HUB_TOKEN"]; ok {
		t.Fatalf("expected no SCION_HUB_TOKEN key, got %q", c.env()["SCION_HUB_TOKEN"])
	}
}

func TestProjectFlag(t *testing.T) {
	if got := projectFlag(""); len(got) != 0 {
		t.Fatalf("empty project should yield no flag, got %v", got)
	}
	got := projectFlag("/x/workers/a")
	if len(got) != 2 || got[0] != "-g" || got[1] != "/x/workers/a" {
		t.Fatalf("got %v", got)
	}
}

func TestParseJSONStripsAnsiAndBanner(t *testing.T) {
	raw := "\x1b[33mWARNING: Development authentication enabled\x1b[0m\n[{\"slug\":\"a\"}]\n"
	var out []map[string]any
	if err := parseJSON(raw, &out); err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	if len(out) != 1 || out[0]["slug"] != "a" {
		t.Fatalf("out=%+v", out)
	}
}

func TestParseJSONEmptyIsNoError(t *testing.T) {
	var out []map[string]any
	if err := parseJSON("WARNING: dev auth\n", &out); err != nil {
		t.Fatalf("empty body should not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want empty, got %+v", out)
	}
}

func TestDefaultHubEndpointDerivesFromPort(t *testing.T) {
	if want := fmt.Sprintf("http://127.0.0.1:%d", DefaultHubPort); DefaultHubEndpoint != want {
		t.Fatalf("DefaultHubEndpoint = %q, want %q", DefaultHubEndpoint, want)
	}
}

// scion's runtime error repeats the podman run argv, -e KEY=VALUE pairs
// included, which can carry the operator's OAuth token: run's error must
// elide env values and mask Anthropic-shaped tokens.
func TestRunErrorRedactsRuntimeEnvValuesAndTokens(t *testing.T) {
	const token = "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789"
	f := &failingRunner{FakeRunner: proc.NewFakeRunner()}
	f.Script("scion resume worker", proc.Result{Code: 1, Stderr: "Error: container run failed: podman run -d -e CLAUDE_CODE_OAUTH_TOKEN=" + token +
		" -e SCION_AGENT_SLUG=worker --env ANTHROPIC_API_KEY=sk-ant-api03-zzzzzzzzzzzzzzzzzzzz -e=FOO=bar --name w img failed: exit status 125 (output: statfs /run/user/501/lever/tickets/worker: no such file or directory)\nbare " + token + " again\n"})
	c := New(f, Options{Bin: "scion"})
	err := c.Resume(context.Background(), "worker", "/lever")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, leak := range []string{token, "sk-ant-api03-zzzz", "=worker ", "=bar"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error leaks %q:\n%s", leak, msg)
		}
	}
	for _, keep := range []string{"-e CLAUDE_CODE_OAUTH_TOKEN=***", "-e SCION_AGENT_SLUG=***", "--env ANTHROPIC_API_KEY=***", "-e=FOO=***", "statfs", "exit status 125", "bare sk-ant-*** again"} {
		if !strings.Contains(msg, keep) {
			t.Fatalf("error should keep %q:\n%s", keep, msg)
		}
	}
}

func TestRedactSecretsLeavesOrdinaryTextAlone(t *testing.T) {
	in := "scion start w: Error: agent \"w\" already exists (phase running); use -e to pass env"
	if got := RedactSecrets(in); got != in {
		t.Fatalf("ordinary text changed: %q", got)
	}
}

// failingRunner adds a non-nil error to every scripted Result, as a real
// non-zero exit arrives; FakeRunner never errs on a scripted command.
type failingRunner struct{ *proc.FakeRunner }

func (f *failingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := f.FakeRunner.RunIn(ctx, dir, env, name, args...)
	if err != nil {
		return res, err
	}
	return res, errors.New("exit status " + fmt.Sprint(res.Code))
}

func (f *failingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return f.RunIn(ctx, "", env, name, args...)
}

// Redaction must never touch output lever parses: relayed messages must stay
// verbatim.
func TestParseJSONDoesNotRedactParsedOutput(t *testing.T) {
	raw := `[{"id":"e9","message":"restart with -e DEBUG=true"}]`
	var got []struct{ Message string }
	if err := parseJSON(raw, &got); err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	if len(got) != 1 || got[0].Message != "restart with -e DEBUG=true" {
		t.Fatalf("parsed output was altered: %+v", got)
	}
}
