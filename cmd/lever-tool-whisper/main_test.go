package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/voice"
)

// pinModel swaps lever's model table for one pinned test model named
// voice.DefaultModel, for the test's length.
func pinModel(t *testing.T) {
	t.Helper()
	old := voice.Models
	voice.Models = []voice.Model{{Name: voice.DefaultModel, File: "ggml-test.bin", Revision: strings.Repeat("b", 40), Size: 1, SHA256: strings.Repeat("c", 64)}}
	t.Cleanup(func() { voice.Models = old })
}

// shortTemp is a temporary directory under /tmp: a socket path in it stays
// within the Unix socket limit however long TMPDIR is (macOS), so the tree
// checks are what the tests reach.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// goodArgs is a valid command line: the tree, and models, program and
// socket outside it.
func goodArgs(t *testing.T) (tree string, args []string) {
	t.Helper()
	pinModel(t)
	tree = shortTemp(t)
	host := shortTemp(t)
	return tree, []string{"-tree", tree, "-models", filepath.Join(host, "models"), "-server", filepath.Join(host, "whisper-server"),
		"-whisper-port", "8448", "-dictate-socket", filepath.Join(host, "run", "d.sock")}
}

func TestParseFlagsGood(t *testing.T) {
	_, args := goodArgs(t)
	o, err := parseFlags(append(args, "-language", "en", "-vocabulary", "Lever, Scion", "-gpu=false", "-max-seconds", "600"))
	if err != nil {
		t.Fatal(err)
	}
	if o.name != "whisper" || o.model.Name != voice.DefaultModel || o.prompt != "Lever, Scion" || o.gpu || o.maxSeconds != 600 || o.whisperPort != 8448 {
		t.Fatalf("%+v", o)
	}
	o, err = parseFlags(args)
	if err != nil || !o.gpu || o.maxSeconds != 300 || o.agentMaxSeconds != 120 || o.language != "" || o.prompt != "" {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	// -agent-max-seconds: set, or at most -max-seconds by default.
	if o, err = parseFlags(append(args, "-agent-max-seconds", "30")); err != nil || o.agentMaxSeconds != 30 {
		t.Fatalf("agent max: %+v %v", o, err)
	}
	if o, err = parseFlags(append(args, "-max-seconds", "60")); err != nil || o.agentMaxSeconds != 60 {
		t.Fatalf("agent max within -max-seconds: %+v %v", o, err)
	}
}

func TestParseFlagsRefusals(t *testing.T) {
	tree, args := goodArgs(t)
	set := func(flag, v string) []string {
		out := append([]string{}, args...)
		for i := range out {
			if out[i] == flag {
				out[i+1] = v
				return out
			}
		}
		return append(out, flag, v)
	}
	drop := func(flag string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == flag {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no tree":           {drop("-tree"), "-tree is required"},
		"no models":         {drop("-models"), "-models is required"},
		"no server":         {drop("-server"), "-server is required"},
		"no socket":         {drop("-dictate-socket"), "-dictate-socket is required"},
		"no port":           {drop("-whisper-port"), "-whisper-port is required"},
		"relative models":   {set("-models", "models"), "absolute"},
		"relative server":   {set("-server", "bin/whisper-server"), "absolute"},
		"relative socket":   {set("-dictate-socket", "d.sock"), "absolute"},
		"unclean socket":    {set("-dictate-socket", "/a/../b.sock"), "clean absolute"},
		"long socket":       {set("-dictate-socket", "/"+strings.Repeat("s", 120)), "at most"},
		"models in tree":    {set("-models", filepath.Join(tree, "models")), "-models"},
		"tree as models":    {set("-models", tree), "inside"},
		"server in tree":    {set("-server", filepath.Join(tree, "bin", "whisper-server")), "-server"},
		"socket in tree":    {set("-dictate-socket", filepath.Join(tree, "d.sock")), "-dictate-socket"},
		"unknown model":     {set("-model", "medium"), "pinned model table"},
		"bad language":      {set("-language", "English"), "-language"},
		"bad vocabulary":    {set("-vocabulary", "a,,b"), "-vocabulary"},
		"port zero":         {set("-whisper-port", "0"), "-whisper-port"},
		"port too big":      {set("-whisper-port", "70000"), "-whisper-port"},
		"port is backend":   {append(set("-whisper-port", "3999"), "-backend", "127.0.0.1:3999"), "-backend"},
		"max zero":          {set("-max-seconds", "0"), "-max-seconds"},
		"max over":          {set("-max-seconds", "601"), "-max-seconds"},
		"agent max zero":    {set("-agent-max-seconds", "0"), "-agent-max-seconds"},
		"agent max over":    {set("-agent-max-seconds", "301"), "-agent-max-seconds"},
		"agent over max":    {append(set("-max-seconds", "60"), "-agent-max-seconds", "61"), "-agent-max-seconds"},
		"gpu with a space":  {append(append([]string{}, args...), "-gpu", "false"), "-gpu=false"},
		"positional":        {append(append([]string{}, args...), "extra"), "unexpected argument"},
		"unknown flag":      {append(append([]string{}, args...), "-port", "1"), "not defined"},
		"relative tree arg": {set("-tree", "tree"), "absolute"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseFlagsRefusesUnverifiedModel(t *testing.T) {
	_, args := goodArgs(t)
	voice.Models[0].SHA256 = voice.Unverified
	if _, err := parseFlags(args); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatal(err)
	}
}

// A path outside the tree that reaches into it through a symbolic link is
// inside it.
func TestParseFlagsSeesThroughLinks(t *testing.T) {
	tree, args := goodArgs(t)
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shortTemp(t), "bin")
	if err := os.Symlink(filepath.Join(tree, "bin"), link); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-server", "-models", "-dictate-socket"} {
		a := append([]string{}, args...)
		for i := range a {
			if a[i] == flag {
				a[i+1] = filepath.Join(link, "x")
			}
		}
		if _, err := parseFlags(a); err == nil || !strings.Contains(err.Error(), "inside") {
			t.Errorf("%s through a link: %v", flag, err)
		}
	}
}

func TestTranscribeBackstop(t *testing.T) {
	ok := map[string]string{"file": "note-1.wav"}
	if err := transcribeBackstop(captool.ValidatedContext{Operation: "transcribe"}, ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x.wav", ".x.wav", "a/b.wav", "x.mp3", ""} {
		if err := transcribeBackstop(captool.ValidatedContext{Operation: "transcribe"}, map[string]string{"file": bad}); err == nil {
			t.Errorf("backstop must refuse %q", bad)
		}
	}
	if err := transcribeBackstop(captool.ValidatedContext{Operation: "load"}, ok); err == nil {
		t.Error("backstop must refuse another operation")
	}
}

func TestAsResult(t *testing.T) {
	v, err := asResult(nil, errors.New("limit"))
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["ok"] != false || m["error"] != "limit" {
		t.Fatalf("got %v %v", v, err)
	}
}
