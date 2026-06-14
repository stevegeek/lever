package scion

import (
	"context"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestRunInjectsEnvAndBin(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list", exec.Result{Stdout: "[]"})
	c := New(f, Options{Bin: "scion", HubEndpoint: "http://127.0.0.1:8080", DevToken: "scion_dev_abc"})
	if _, err := c.run(context.Background(), "", "list"); err != nil {
		t.Fatalf("run: %v", err)
	}
	call := f.Calls[0]
	if call.Name != "scion" || call.Args[0] != "list" {
		t.Fatalf("argv=%+v", call)
	}
	if call.Env["SCION_HUB_ENDPOINT"] != "http://127.0.0.1:8080" || call.Env["SCION_DEV_TOKEN"] != "scion_dev_abc" {
		t.Fatalf("env=%+v", call.Env)
	}
}

func TestProjectFlag(t *testing.T) {
	if got := projectFlag(""); len(got) != 0 {
		t.Fatalf("empty project should yield no flag, got %v", got)
	}
	got := projectFlag("/x/groves/a")
	if len(got) != 2 || got[0] != "-g" || got[1] != "/x/groves/a" {
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
