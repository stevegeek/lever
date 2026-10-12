package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stevegeek/lever/internal/testutil"
)

const testSpeakSocket = "/run/lever/tts.sock"

// ttsTool is a broker tools entry running lever-tool-tts with the given
// extra flags.
func ttsTool(extra ...string) Tool {
	cmd := append([]string{"/opt/lever/lever-tool-tts", "-tree", "/t", "-models", "/s/voice-models",
		"-server", "/opt/sherpa/bin/sherpa-onnx-offline-tts", "-speak-socket", testSpeakSocket}, extra...)
	return Tool{Name: "tts", Command: cmd, Backend: "127.0.0.1:3213", Operations: []Op{{Name: "voices"}}}
}

// speechApp is voiceApp with read_aloud: server and a tts tool.
func speechApp() *App {
	a := voiceApp()
	a.Remote.Voice.ReadAloud = ReadAloudServer
	a.Remote.Voice.SpeakSocket = testSpeakSocket
	a.Broker.Tools = append(a.Broker.Tools, ttsTool())
	return a
}

func TestReadAloudYAML(t *testing.T) {
	for in, want := range map[string]ReadAloud{
		"read_aloud: true":   ReadAloudDevice,
		"read_aloud: false":  ReadAloudOff,
		"read_aloud: server": ReadAloudServer,
		"read_aloud: device": ReadAloudDevice,
		"read_aloud: off":    ReadAloudOff,
		"enabled: true":      "",
	} {
		var v Voice
		if err := yaml.Unmarshal([]byte(in), &v); err != nil || v.ReadAloud != want {
			t.Errorf("%s: %q %v", in, v.ReadAloud, err)
		}
	}
	var v Voice
	if err := yaml.Unmarshal([]byte("read_aloud: [server]"), &v); err == nil {
		t.Error("a list was accepted")
	}
	if err := yaml.Unmarshal([]byte("speak_socket: /run/t.sock\nread_aloud: server"), &v); err != nil || v.SpeakSocket != "/run/t.sock" {
		t.Errorf("%+v %v", v, err)
	}
}

func TestEffectiveReadAloud(t *testing.T) {
	a := voiceApp()
	if a.EffectiveReadAloud() != ReadAloudDevice {
		t.Fatal("unset is not device")
	}
	a.Remote.Voice.ReadAloud = ReadAloudServer
	if a.EffectiveReadAloud() != ReadAloudServer {
		t.Fatal("server")
	}
	a.Remote.Voice.Enabled = false
	if a.EffectiveReadAloud() != ReadAloudOff {
		t.Fatal("voice off is not off")
	}
}

func TestRemoteSpeechValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"server":               {func(a *App) {}, ""},
		"device":               {func(a *App) { a.Remote.Voice.ReadAloud = ReadAloudDevice; a.Remote.Voice.SpeakSocket = "" }, ""},
		"bad word":             {func(a *App) { a.Remote.Voice.ReadAloud = "cloud" }, `read_aloud "cloud"`},
		"bad word, voice off":  {func(a *App) { a.Remote.Voice = Voice{ReadAloud: "loud"} }, "read_aloud"},
		"server, voice off":    {func(a *App) { a.Remote.Voice.Enabled = false; a.Remote.Voice.SpeakSocket = "" }, ""},
		"no speak socket":      {func(a *App) { a.Remote.Voice.SpeakSocket = "" }, "voice.speak_socket is required"},
		"relative":             {func(a *App) { a.Remote.Voice.SpeakSocket = "run/t.sock" }, "clean absolute"},
		"is dictation socket":  {func(a *App) { a.Remote.Voice.SpeakSocket = testSocket }, "dictation socket"},
		"not the tool's":       {func(a *App) { a.Remote.Voice.SpeakSocket = "/run/other.sock" }, "not the -speak-socket of any tts tool"},
		"no tool, any socket":  {func(a *App) { a.Broker.Tools = a.Broker.Tools[:1]; a.Remote.Voice.SpeakSocket = "/run/other.sock" }, ""},
		"server without tools": {func(a *App) { a.Broker.Tools = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := speechApp()
			tc.mut(a)
			err := a.validateRemote()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTTSToolValidation(t *testing.T) {
	with := func(flag, v string) func(a *App) {
		return func(a *App) {
			c := slices.Clone(a.Broker.Tools[1].Command)
			if i := slices.Index(c, flag); i >= 0 {
				c[i+1] = v
			} else {
				c = append(c, flag, v)
			}
			a.Broker.Tools[1].Command = c
		}
	}
	without := func(flag string) func(a *App) {
		return func(a *App) {
			c := slices.Clone(a.Broker.Tools[1].Command)
			i := slices.Index(c, flag)
			a.Broker.Tools[1].Command = slices.Delete(c, i, i+2)
		}
	}
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"good": {func(a *App) {}, ""},
		"all flags": {func(a *App) {
			with("-voice", "bm_george")(a)
			with("-speed", "1.1")(a)
			with("-threads", "4")(a)
			with("-max-chars", "800")(a)
		}, ""},
		"piper":            {func(a *App) { with("-model", "piper-en_GB-alba-medium")(a); with("-voice", "alba")(a) }, ""},
		"no socket":        {without("-speak-socket"), "-speak-socket is required"},
		"long socket":      {with("-speak-socket", "/"+strings.Repeat("s", 110)), "at most"},
		"dictation socket": {with("-speak-socket", testSocket), "also a dictation socket"},
		"no models":        {without("-models"), "needs -models and -server"},
		"no server":        {without("-server"), "needs -models and -server"},
		"no tree":          {without("-tree"), "-tree is required"},
		"other tree":       {with("-tree", "/elsewhere"), "not the instance's tree"},
		"unknown model":    {with("-model", "tacotron"), "not in lever's speech model table"},
		"unknown voice":    {with("-voice", "nobody"), "not a voice of"},
		"voice of other":   {with("-voice", "alba"), "not a voice of kokoro"},
		"speed word":       {with("-speed", "fast"), "not a number"},
		"speed over":       {with("-speed", "2.5"), "-speed"},
		"threads over":     {with("-threads", "99"), "-threads"},
		"threads word":     {with("-threads", "many"), "not a number"},
		"chars low":        {with("-max-chars", "5"), "-max-chars"},
		"two tools, one socket": {func(a *App) {
			o := ttsTool()
			o.Name, o.Backend = "tts2", "127.0.0.1:3214"
			a.Broker.Tools = append(a.Broker.Tools, o)
		}, "same -speak-socket"},
		"a renamed wrapper is checked": {func(a *App) { a.Broker.Tools[1].Command[0] = "/opt/my-tts"; without("-models")(a) }, "needs -models"},
	} {
		t.Run(name, func(t *testing.T) {
			a := speechApp()
			tc.mut(a)
			err := a.validateTTSTools()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTTSToolsAndSpeechTool(t *testing.T) {
	a := speechApp()
	a.Broker.Tools[1].Command = append(a.Broker.Tools[1].Command, "-voice=bm_george", "--speed", "1.2", "-model", "kokoro-en-v0_19")
	ts := a.TTSTools()
	if len(ts) != 1 {
		t.Fatalf("%+v", ts)
	}
	w := ts[0]
	if w.Name != "tts" || w.Tree != "/t" || w.Models != "/s/voice-models" || w.Socket != testSpeakSocket || w.Voice != "bm_george" || w.Speed != "1.2" || w.EffectiveModel() != "kokoro-en-v0_19" {
		t.Fatalf("%+v", w)
	}
	if s, ok := a.SpeechTool(); !ok || s.Name != "tts" {
		t.Fatal("no speech tool")
	}
	a.Remote.Voice.ReadAloud = ReadAloudDevice
	if _, ok := a.SpeechTool(); ok {
		t.Fatal("a speech tool with read_aloud: device")
	}
	// Whisper tools are not tts tools, and the other way round.
	if len(voiceApp().TTSTools()) != 0 || len(speechApp().WhisperTools()) != 1 {
		t.Fatal("the tools are mixed up")
	}
	if (TTSTool{}).EffectiveModel() != "kokoro-en-v0_19" {
		t.Fatal("default model")
	}
}

// The tts tool's program, models and socket, and remote.voice.speak_socket,
// must not lie where an agent can write or reach them.
func TestTTSPathsRefusedInsideTree(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	set := func(a *App, flag, v string) {
		c := slices.Clone(a.Broker.Tools[1].Command)
		c[slices.Index(c, flag)+1] = v
		a.Broker.Tools[1].Command = c
	}
	base := func() *App {
		a := speechApp()
		a.Tree, a.dir = tree, root
		a.Broker.Tools = a.Broker.Tools[1:2]
		a.Broker.Tools = append([]Tool{whisperTool()}, a.Broker.Tools...)
		c := slices.Clone(a.Broker.Tools[0].Command)
		c[0] = filepath.Join(root, "lever-tool-whisper")
		c[slices.Index(c, "-models")+1] = filepath.Join(root, "models")
		c[slices.Index(c, "-server")+1] = filepath.Join(root, "whisper-server")
		a.Broker.Tools[0].Command = c
		set(a, "-tree", tree)
		set(a, "-models", filepath.Join(root, "models"))
		set(a, "-server", filepath.Join(root, "sherpa"))
		a.Broker.Tools[1].Command[0] = filepath.Join(root, "lever-tool-tts")
		return a
	}
	if err := base().checkHostPathsOutsideTree(); err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"-server": "install it outside the tree", "-models": "move it outside the tree", "-speak-socket": "move it outside the tree"} {
		a := base()
		set(a, flag, filepath.Join(tree, "bin", "x"))
		testutil.WantErrContaining(t, a.checkHostPathsOutsideTree(), "broker.tools[tts] "+flag, "inside the mounted tree", want)
		a.Manager.ReadOnly = []string{"bin"}
		if err := a.checkHostPathsOutsideTree(); err == nil {
			t.Errorf("%s: a read_only mount excused it", flag)
		}
	}
	a := base()
	a.Remote.Voice.SpeakSocket = filepath.Join(tree, "t.sock")
	testutil.WantErrContaining(t, a.checkHostPathsOutsideTree(), "remote.voice.speak_socket", "inside the mounted tree")
}
