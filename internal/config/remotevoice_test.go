package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func voiceApp() *App {
	a := filesApp()
	a.Remote.Voice = Voice{Enabled: true, WhisperServer: "/usr/local/bin/whisper-server"}
	return a
}

func TestRemoteVoiceValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"on":                {func(a *App) {}, ""},
		"off, no program":   {func(a *App) { a.Remote.Voice = Voice{} }, ""},
		"on console":        {func(a *App) { a.Remote.Landing = "" }, "landing: chat"},
		"no program":        {func(a *App) { a.Remote.Voice.WhisperServer = "" }, "whisper_server is required"},
		"relative program":  {func(a *App) { a.Remote.Voice.WhisperServer = "bin/whisper-server" }, "absolute"},
		"unclean program":   {func(a *App) { a.Remote.Voice.WhisperServer = "/usr/local/../bin/whisper-server" }, "clean absolute"},
		"max 600":           {func(a *App) { a.Remote.Voice.MaxSeconds = 600 }, ""},
		"max over":          {func(a *App) { a.Remote.Voice.MaxSeconds = 601 }, "max_seconds"},
		"max negative":      {func(a *App) { a.Remote.Voice.MaxSeconds = -1 }, "max_seconds"},
		"max over, off":     {func(a *App) { a.Remote.Voice = Voice{MaxSeconds: 9000} }, "max_seconds"},
		"model known":       {func(a *App) { a.Remote.Voice.Model = "large-v3-turbo-q5_0" }, ""},
		"model unknown":     {func(a *App) { a.Remote.Voice.Model = "medium" }, "pinned model table"},
		"language":          {func(a *App) { a.Remote.Voice.Language = "en" }, ""},
		"language bad":      {func(a *App) { a.Remote.Voice.Language = "English" }, "language"},
		"language auto":     {func(a *App) { a.Remote.Voice.Language = "auto" }, "language"},
		"vocabulary":        {func(a *App) { a.Remote.Voice.Vocabulary = []string{"Lever", "Scion", "mTLS"} }, ""},
		"vocabulary empty":  {func(a *App) { a.Remote.Voice.Vocabulary = []string{" "} }, "vocabulary"},
		"vocabulary ctrl":   {func(a *App) { a.Remote.Voice.Vocabulary = []string{"a\nb"} }, "vocabulary"},
		"vocabulary long":   {func(a *App) { a.Remote.Voice.Vocabulary = slices.Repeat([]string{strings.Repeat("w", 60)}, 20) }, "prompt"},
		"port allowlisted":  {func(a *App) { a.Manager.AllowPorts = []int{DefaultRemoteVoicePort} }, "jail may reach"},
		"port custom, list": {func(a *App) { a.Remote.Voice.Port = 9100; a.Manager.AllowPorts = []int{9100} }, "jail may reach"},
		"port custom":       {func(a *App) { a.Remote.Voice.Port = 9100; a.Manager.AllowPorts = []int{DefaultRemoteVoicePort} }, ""},
		"port jail":         {func(a *App) { a.Remote.Voice.Port = DefaultBrokerJailPort }, "jail may reach"},
		"port login":        {func(a *App) { a.Remote.Voice.Port = DefaultRemoteLoginPort }, "jail may reach"},
		"port proxy":        {func(a *App) { a.Remote.Voice.Port = DefaultRemotePort }, "remote proxy"},
		"port admin":        {func(a *App) { a.Remote.Voice.Port = DefaultBrokerAdminPort }, "admin"},
		"port mirror":       {func(a *App) { a.Remote.Voice.Port = GuestLoginIssuerPort }, "mirrored"},
		"port bad":          {func(a *App) { a.Remote.Voice.Port = 70000 }, "not a port"},
		"port tool": {func(a *App) {
			a.Broker.Tools = []Tool{{Name: "fizzy", Backend: "http://127.0.0.1:8448"}}
		}, "broker tool"},
	} {
		t.Run(name, func(t *testing.T) {
			a := voiceApp()
			tc.mut(a)
			err := a.validateRemote()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVoiceGetters(t *testing.T) {
	a := filesApp()
	if a.VoiceOn() || a.EffectiveVoiceMaxSeconds() != 300 || a.EffectiveVoicePort() != DefaultRemoteVoicePort || !a.VoiceGPU() ||
		a.EffectiveVoiceModel() != "large-v3-turbo" || a.VoicePrompt() != "" || a.VoiceExcludedLogins() != nil {
		t.Fatal("defaults: off, 300 s, port 8448, GPU on, large-v3-turbo")
	}
	no := false
	a.Remote.Voice = Voice{Enabled: true, MaxSeconds: 60, Port: 9100, GPU: &no, Model: "large-v3-turbo-q5_0", Vocabulary: []string{"Lever", "Fizzy"}}
	if !a.VoiceOn() || a.EffectiveVoiceMaxSeconds() != 60 || a.EffectiveVoicePort() != 9100 || a.VoiceGPU() ||
		a.EffectiveVoiceModel() != "large-v3-turbo-q5_0" || a.VoicePrompt() != "Lever, Fizzy" {
		t.Fatal("set")
	}
	a.Remote.AllowedUsers[1].Voice = &no
	if got := a.VoiceExcludedLogins(); !slices.Equal(got, []string{"c@x"}) {
		t.Fatalf("%v", got)
	}
	if a.Remote.AllowedUsers[1].VoiceAllowed() || !a.Remote.AllowedUsers[0].VoiceAllowed() {
		t.Fatal("VoiceAllowed")
	}
	a.Remote.Landing = ""
	if a.VoiceOn() || a.VoiceExcludedLogins() != nil {
		t.Fatal("voice needs landing chat")
	}
}

func TestVoicePortNeverAllowedForTheJail(t *testing.T) {
	// Whatever validates, the jail's allowlist never holds the voice port.
	a := voiceApp()
	if err := a.validateRemote(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(a.EffectiveAllowedPorts(), a.EffectiveVoicePort()) {
		t.Fatal("the voice port is allowlisted")
	}
}

func TestAllowedUserVoiceKey(t *testing.T) {
	var u struct {
		Users []RemoteUser `yaml:"users"`
	}
	if err := yaml.Unmarshal([]byte("users:\n- {login: c@x, tier: contact, agents: [w1], voice: false}\n- {login: op@x, voice: true}\n- d@x\n"), &u); err != nil {
		t.Fatal(err)
	}
	if u.Users[0].Voice == nil || *u.Users[0].Voice || u.Users[1].Voice == nil || !*u.Users[1].Voice || u.Users[2].Voice != nil {
		t.Fatalf("%+v", u.Users)
	}
	if err := yaml.Unmarshal([]byte("users:\n- {login: c@x, voice: maybe}\n"), &u); err == nil {
		t.Fatal("a non-bool voice was accepted")
	}
	var v Voice
	if err := yaml.Unmarshal([]byte("enabled: true\nwhisper_server: /opt/w\nmodel: large-v3-turbo\nlanguage: en\nvocabulary: [Lever, Scion]\nmax_seconds: 120\ngpu: false\nport: 9100\n"), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || v.WhisperServer != "/opt/w" || v.Language != "en" || len(v.Vocabulary) != 2 || v.MaxSeconds != 120 || v.GPU == nil || *v.GPU || v.Port != 9100 {
		t.Fatalf("%+v", v)
	}
}

// TestWhisperServerRefusedInsideTree: the program the proxy runs must not
// lie where an agent can write, and a read-only mount does not excuse it.
func TestWhisperServerRefusedInsideTree(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := voiceApp()
	a.Tree = tree
	a.dir = root
	a.Remote.Voice.WhisperServer = filepath.Join(tree, "bin", "whisper-server")
	err := a.checkHostPathsOutsideTree()
	if err == nil || !strings.Contains(err.Error(), "remote.voice.whisper_server") {
		t.Fatalf("err = %v", err)
	}
	a.Manager.ReadOnly = []string{"bin"}
	if err := a.checkHostPathsOutsideTree(); err == nil {
		t.Fatal("a read_only mount excused the whisper-server program")
	}
	a.Remote.Voice.WhisperServer = filepath.Join(root, "whisper-server")
	if err := a.checkHostPathsOutsideTree(); err != nil {
		t.Fatal(err)
	}
	// Voice off: the key is not read.
	a.Remote.Voice = Voice{WhisperServer: filepath.Join(tree, "bin", "whisper-server")}
	if err := a.checkHostPathsOutsideTree(); err != nil {
		t.Fatal(err)
	}
}
