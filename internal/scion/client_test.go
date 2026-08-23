package scion

import (
	"context"
	"fmt"
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

// TestEnvEmitsLazyHubTokenSource: HubTokenSource is read at call time (the
// mint-mid-apply case, where the token isn't known at New() time).
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
// key at all (not even empty string) — keeps subscription-mode (no controller
// PAT) env untouched.
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
