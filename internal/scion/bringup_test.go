package scion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestStaleAgent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cannot resume", errors.New("cannot resume agent 'assistant'"), true},
		{"cannot resume uppercase", errors.New("CANNOT RESUME agent 'assistant'"), true},
		{"agent does not exist", errors.New("agent does not exist"), true},
		{"agent does not exist mixed case", errors.New("Agent Does Not Exist"), true},
		{"failed to resume suspended agent", errors.New("Failed to resume suspended agent 'assistant': boom"), true},
		{"failed to resume suspended agent lowercase", errors.New("failed to resume suspended agent"), true},
		{"unrelated error", errors.New("no_runtime_broker: No runtime brokers available"), false},
		{"already running", errors.New("Error: agent already running"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleAgent(tc.err); got != tc.want {
				t.Errorf("StaleAgent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestEnvSetArgvAndProjectScope(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.EnvSet(context.Background(), "/jail/work", "LEVER_LLM_AUTH", "api-key"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "hub env set --project LEVER_LLM_AUTH=api-key" {
		t.Errorf("args = %q", got)
	}
	// Project scope is conveyed by the working directory (bare --project infers it).
	if f.Calls[0].Dir != "/jail/work" {
		t.Errorf("cwd = %q, want /jail/work (project scope)", f.Calls[0].Dir)
	}
}

func TestBringupArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	_ = c.InitMachine(context.Background())
	_ = c.ConfigSetGlobal(context.Background(), "image_registry", "scionlocal")
	_ = c.ServerStart(context.Background())
	_ = c.SecretSet(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-rawtoken")
	all := []string{}
	for _, cc := range f.Calls {
		all = append(all, strings.Join(cc.Args, " "))
	}
	j := strings.Join(all, "|")
	for _, want := range []string{
		"init --machine --non-interactive",
		"config set --global image_registry scionlocal",
		"server start",
		// value is base64-encoded (scion >= da49e14 requires it): b64("sk-ant-rawtoken")
		"hub secret set CLAUDE_CODE_OAUTH_TOKEN c2stYW50LXJhd3Rva2Vu",
	} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing %q in %q", want, j)
		}
	}
}
