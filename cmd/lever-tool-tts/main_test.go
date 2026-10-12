package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/voice"
)

// pinModel swaps lever's speech model table for one pinned test model named
// voice.DefaultSpeechModel, for the test's length.
func pinModel(t *testing.T) {
	t.Helper()
	old := voice.SpeechModels
	voice.SpeechModels = []voice.SpeechModel{{
		Name: voice.DefaultSpeechModel, Kind: voice.KindKokoro,
		ArchiveURL:  "https://github.com/someone/kokoro/releases/download/models/kokoro.tar.bz2",
		ArchiveSize: 1, ArchiveSHA256: strings.Repeat("c", 64), ArchiveRoot: "kokoro",
		ModelFile: "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Voices:       []voice.SpeechVoice{{Name: "bf_emma", SID: 7, Lang: "en-GB"}, {Name: "bm_george", SID: 9, Lang: "en-GB"}},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	}}
	t.Cleanup(func() { voice.SpeechModels = old })
}

// shortTemp is a temporary directory under /tmp, so a socket path in it
// stays within the Unix socket limit however long TMPDIR is.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lt")
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
	return tree, []string{"-tree", tree, "-models", filepath.Join(host, "models"), "-server", filepath.Join(host, "sherpa-onnx-offline-tts"),
		"-speak-socket", filepath.Join(host, "run", "s.sock")}
}

func TestParseFlagsGood(t *testing.T) {
	_, args := goodArgs(t)
	o, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if o.name != "tts" || o.model.Name != voice.DefaultSpeechModel || o.voice != "bf_emma" || o.speed != 1 || o.threads != DefaultThreads || o.maxChars != voice.DefaultSpeakMaxChars {
		t.Fatalf("defaults: %+v", o)
	}
	o, err = parseFlags(append(args, "-voice", "bm_george", "-speed", "1.2", "-threads", "4", "-max-chars", "1000", "-name", "speech"))
	if err != nil || o.voice != "bm_george" || o.speed != 1.2 || o.threads != 4 || o.maxChars != 1000 || o.name != "speech" {
		t.Fatalf("%+v %v", o, err)
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
		"no tree":         {drop("-tree"), "-tree is required"},
		"no models":       {drop("-models"), "-models is required"},
		"no server":       {drop("-server"), "-server is required"},
		"no socket":       {drop("-speak-socket"), "-speak-socket is required"},
		"relative models": {set("-models", "models"), "absolute"},
		"relative server": {set("-server", "bin/tts"), "absolute"},
		"unclean socket":  {set("-speak-socket", "/a/../b.sock"), "clean absolute"},
		"long socket":     {set("-speak-socket", "/"+strings.Repeat("s", 120)), "at most"},
		"models in tree":  {set("-models", filepath.Join(tree, "models")), "-models"},
		"tree as models":  {set("-models", tree), "inside"},
		"server in tree":  {set("-server", filepath.Join(tree, "bin", "tts")), "-server"},
		"socket in tree":  {set("-speak-socket", filepath.Join(tree, "s.sock")), "-speak-socket"},
		"unknown model":   {set("-model", "tacotron"), "pinned speech model table"},
		"unknown voice":   {set("-voice", "af_sky"), "not a voice"},
		"slow":            {set("-speed", "0.2"), "-speed"},
		"fast":            {set("-speed", "3"), "-speed"},
		"not a number":    {set("-speed", "NaN"), "-speed"},
		"threads zero":    {set("-threads", "0"), "-threads"},
		"threads over":    {set("-threads", "17"), "-threads"},
		"chars low":       {set("-max-chars", "10"), "-max-chars"},
		"chars over":      {set("-max-chars", "5000"), "-max-chars"},
		"positional":      {append(append([]string{}, args...), "extra"), "unexpected argument"},
		"a port flag":     {append(append([]string{}, args...), "-tts-port", "1"), "not defined"},
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
	voice.SpeechModels[0].ArchiveSHA256 = voice.Unverified
	if _, err := parseFlags(args); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatal(err)
	}
}

// The shipped table's default model is pinned: the flags parse.
func TestShippedModelIsPinned(t *testing.T) {
	tree, host := shortTemp(t), shortTemp(t)
	args := []string{"-tree", tree, "-models", filepath.Join(host, "m"), "-server", filepath.Join(host, "tts"), "-speak-socket", filepath.Join(host, "s.sock")}
	if _, err := parseFlags(args); err != nil {
		t.Fatal(err)
	}
}

func TestParseFlagsSeesThroughLinks(t *testing.T) {
	tree, args := goodArgs(t)
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shortTemp(t), "bin")
	if err := os.Symlink(filepath.Join(tree, "bin"), link); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-server", "-models", "-speak-socket"} {
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

func TestVoicesBackstop(t *testing.T) {
	if err := voicesBackstop(captool.ValidatedContext{Operation: "voices"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := voicesBackstop(captool.ValidatedContext{Operation: "speak"}, nil); err == nil {
		t.Fatal("backstop must refuse another operation")
	}
}
