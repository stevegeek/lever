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

const testSocket = "/run/lever/whisper.sock"

// whisperTool is a broker tools entry running lever-tool-whisper with the
// given extra flags.
func whisperTool(extra ...string) Tool {
	cmd := append([]string{"/opt/lever/lever-tool-whisper", "-tree", "/t", "-models", "/s/voice-models",
		"-server", "/opt/w/whisper-server", "-whisper-port", "8448", "-dictate-socket", testSocket}, extra...)
	return Tool{Name: "whisper", Command: cmd, Backend: "127.0.0.1:3212", Operations: []Op{{Name: "transcribe"}}}
}

func voiceApp() *App {
	a := filesApp()
	a.Remote.Voice = Voice{Enabled: true, Socket: testSocket}
	a.Broker.Tools = []Tool{whisperTool()}
	return a
}

func TestRemoteVoiceValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"on":                {func(a *App) {}, ""},
		"off, no socket":    {func(a *App) { a.Remote.Voice = Voice{} }, ""},
		"on console":        {func(a *App) { a.Remote.Landing = "" }, "landing: chat"},
		"no socket":         {func(a *App) { a.Remote.Voice.Socket = "" }, "voice.socket is required"},
		"relative socket":   {func(a *App) { a.Remote.Voice.Socket = "run/w.sock" }, "clean absolute"},
		"unclean socket":    {func(a *App) { a.Remote.Voice.Socket = "/run/../w.sock" }, "clean absolute"},
		"long socket":       {func(a *App) { a.Remote.Voice.Socket = "/" + strings.Repeat("s", 110) }, "at most"},
		"socket mismatch":   {func(a *App) { a.Remote.Voice.Socket = "/run/other.sock" }, "not the -dictate-socket of any whisper tool"},
		"no tool, any path": {func(a *App) { a.Broker.Tools = nil; a.Remote.Voice.Socket = "/run/other.sock" }, ""},
		"max 300":           {func(a *App) { a.Remote.Voice.MaxSeconds = 300 }, ""},
		"max over tool":     {func(a *App) { a.Remote.Voice.MaxSeconds = 600 }, "-max-seconds 300"},
		"max tool raised": {func(a *App) {
			a.Remote.Voice.MaxSeconds = 600
			a.Broker.Tools = []Tool{whisperTool("-max-seconds", "600")}
		}, ""},
		"max tool lowered": {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds=60")} }, "-max-seconds 60"},
		"max over":         {func(a *App) { a.Remote.Voice.MaxSeconds = 601 }, "max_seconds"},
		"max negative":     {func(a *App) { a.Remote.Voice.MaxSeconds = -1 }, "max_seconds"},
		"max over, off":    {func(a *App) { a.Remote.Voice = Voice{MaxSeconds: 9000} }, "max_seconds"},
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

// The whisper tool's -whisper-port is never a port the jail may reach, nor
// one of lever's own listeners: whatever runs, voice on or off.
func TestWhisperToolValidation(t *testing.T) {
	withPort := func(p string) func(a *App) {
		return func(a *App) {
			c := slices.Clone(a.Broker.Tools[0].Command)
			c[slices.Index(c, "-whisper-port")+1] = p
			a.Broker.Tools[0].Command = c
		}
	}
	without := func(flag string) func(a *App) {
		return func(a *App) {
			c := slices.Clone(a.Broker.Tools[0].Command)
			i := slices.Index(c, flag)
			a.Broker.Tools[0].Command = append(c[:i], c[i+2:]...)
		}
	}
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"ok":                {func(a *App) {}, ""},
		"voice off":         {func(a *App) { a.Remote.Voice = Voice{} }, ""},
		"allowlisted":       {func(a *App) { a.Manager.AllowPorts = []int{8448} }, "jail may reach"},
		"allowlisted, off":  {func(a *App) { a.Remote = Remote{}; a.Manager.AllowPorts = []int{8448} }, "jail may reach"},
		"custom, list":      {func(a *App) { withPort("9100")(a); a.Manager.AllowPorts = []int{9100} }, "jail may reach"},
		"custom":            {func(a *App) { withPort("9100")(a); a.Manager.AllowPorts = []int{8448} }, ""},
		"jail":              {withPort("8443"), "jail may reach"},
		"login":             {withPort("8447"), "jail may reach"},
		"proxy":             {withPort("8445"), "remote proxy"},
		"proxy, remote off": {func(a *App) { a.Remote = Remote{}; withPort("8445")(a) }, ""},
		"admin":             {withPort("8444"), "admin"},
		"mirror":            {withPort("8446"), "mirrored"},
		"not a number":      {withPort("x"), "not a port"},
		"too big":           {withPort("70000"), "not a port"},
		"own backend":       {withPort("3212"), "backend port"},
		"other backend": {func(a *App) {
			a.Broker.Tools = append(a.Broker.Tools, Tool{Name: "fizzy", Command: []string{"/x"}, Backend: "http://127.0.0.1:8448"})
		}, "broker tool \"fizzy\""},
		"other tree": {func(a *App) {
			c := slices.Clone(a.Broker.Tools[0].Command)
			c[slices.Index(c, "-tree")+1] = "/elsewhere"
			a.Broker.Tools[0].Command = c
		}, "not the instance's tree"},
		"no port":    {without("-whisper-port"), "-whisper-port is required"},
		"no socket":  {without("-dictate-socket"), "-dictate-socket is required"},
		"no models":  {without("-models"), "-models and -server"},
		"no tree":    {without("-tree"), "-tree is required"},
		"max ok":     {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds", "600")} }, ""},
		"max zero":   {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds", "0")} }, "-max-seconds: max seconds 0"},
		"max over":   {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds=601")} }, "-max-seconds: max seconds 601"},
		"max text":   {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds", "x")} }, "not a number"},
		"max empty":  {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-max-seconds", "-gpu=false")} }, "not a number"},
		"agent ok":   {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-agent-max-seconds", "300")} }, ""},
		"agent over": {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-agent-max-seconds", "301")} }, "-agent-max-seconds"},
		"agent over tool max": {func(a *App) {
			a.Broker.Tools = []Tool{whisperTool("-max-seconds", "60", "-agent-max-seconds", "61")}
		}, "use 1 to 60"},
		"agent zero": {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-agent-max-seconds", "0")} }, "-agent-max-seconds"},
		"agent text": {func(a *App) { a.Broker.Tools = []Tool{whisperTool("-agent-max-seconds", "2m")} }, "not a number"},
		"no server":  {without("-server"), "-models and -server"},
		"two tools, one port": {func(a *App) {
			w := whisperTool()
			w.Name, w.Backend = "whisper2", "127.0.0.1:3213"
			w.Command[len(w.Command)-1] = "/run/lever/w2.sock"
			a.Broker.Tools = append(a.Broker.Tools, w)
		}, "also broker tool"},
		"external is not read": {func(a *App) {
			a.Broker.Tools = append(a.Broker.Tools, Tool{Name: "ext", External: true, Backend: "127.0.0.1:9", Command: nil})
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := voiceApp()
			tc.mut(a)
			err := a.validateWhisperTools()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWhisperToolsAndVoiceTool(t *testing.T) {
	a := voiceApp()
	a.Broker.Tools = append([]Tool{{Name: "fizzy", Command: []string{"/x", "-state", "/s"}, Backend: "127.0.0.1:3211"}},
		whisperTool("--model=large-v3-turbo-q5_0", "-max-seconds", "120"))
	ws := a.WhisperTools()
	if len(ws) != 1 {
		t.Fatalf("%+v", ws)
	}
	w := ws[0]
	if w.Name != "whisper" || w.Port != 8448 || w.Socket != testSocket || w.Models != "/s/voice-models" || w.Server != "/opt/w/whisper-server" ||
		w.EffectiveModel() != "large-v3-turbo-q5_0" || w.EffectiveMaxSeconds() != 120 {
		t.Fatalf("%+v", w)
	}
	if v, ok := a.VoiceTool(); !ok || v.Name != "whisper" {
		t.Fatalf("%+v %v", v, ok)
	}
	if (WhisperTool{}).EffectiveModel() != "large-v3-turbo" || (WhisperTool{}).EffectiveMaxSeconds() != 300 || (WhisperTool{}).EffectiveAgentMaxSeconds() != 120 {
		t.Fatal("defaults")
	}
	if (WhisperTool{MaxSeconds: 60}).EffectiveAgentMaxSeconds() != 60 || (WhisperTool{AgentMaxSeconds: 30}).EffectiveAgentMaxSeconds() != 30 {
		t.Fatal("agent max seconds")
	}
	// Found by the program's base name only: another program with the same
	// flags is not a whisper tool; one behind env is.
	other := whisperTool()
	other.Command[0] = "/opt/lever/my-whisper-wrapper"
	envd := whisperTool()
	envd.Command = append([]string{"env", "CUDA_VISIBLE_DEVICES=0"}, envd.Command...)
	a.Broker.Tools = []Tool{other}
	if ws := a.WhisperTools(); len(ws) != 0 {
		t.Fatalf("by flags: %+v", ws)
	}
	a.Broker.Tools = []Tool{envd}
	if ws := a.WhisperTools(); len(ws) != 1 || ws[0].Socket != testSocket {
		t.Fatalf("behind env: %+v", ws)
	}
	a.Remote.Voice.Socket = "/other.sock"
	if _, ok := a.VoiceTool(); ok {
		t.Fatal("a tool on another socket")
	}
	a.Remote.Voice = Voice{}
	if _, ok := a.VoiceTool(); ok {
		t.Fatal("voice off")
	}
}

func TestVoiceGetters(t *testing.T) {
	a := filesApp()
	if a.VoiceOn() || a.EffectiveVoiceMaxSeconds() != 300 || a.VoiceExcludedLogins() != nil {
		t.Fatal("defaults: off, 300 s")
	}
	no := false
	a.Remote.Voice = Voice{Enabled: true, MaxSeconds: 60}
	if !a.VoiceOn() || a.EffectiveVoiceMaxSeconds() != 60 {
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
	if err := yaml.Unmarshal([]byte("enabled: true\nsocket: /run/w.sock\nmax_seconds: 120\nread_aloud: false\n"), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || v.Socket != "/run/w.sock" || v.MaxSeconds != 120 || v.ReadAloud == nil || *v.ReadAloud {
		t.Fatalf("%+v", v)
	}
}

// The whisper tool's program, models and socket, and remote.voice.socket,
// must not lie where an agent can write or reach them, and a read-only
// mount does not excuse them.
func TestWhisperPathsRefusedInsideTree(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	set := func(a *App, flag, v string) {
		c := slices.Clone(a.Broker.Tools[0].Command)
		c[slices.Index(c, flag)+1] = v
		a.Broker.Tools[0].Command = c
	}
	base := func() *App {
		a := voiceApp()
		a.Tree, a.dir = tree, root
		set(a, "-tree", tree)
		set(a, "-models", filepath.Join(root, "models"))
		set(a, "-server", filepath.Join(root, "whisper-server"))
		a.Broker.Tools[0].Command[0] = filepath.Join(root, "lever-tool-whisper")
		return a
	}
	if err := base().checkHostPathsOutsideTree(); err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"-server": "install it outside the tree", "-models": "move it outside the tree", "-dictate-socket": "move it outside the tree"} {
		a := base()
		set(a, flag, filepath.Join(tree, "bin", "x"))
		testutil.WantErrContaining(t, a.checkHostPathsOutsideTree(), "broker.tools[whisper] "+flag, "inside the mounted tree", want)
		a.Manager.ReadOnly = []string{"bin"}
		if err := a.checkHostPathsOutsideTree(); err == nil {
			t.Errorf("%s: a read_only mount excused it", flag)
		}
	}
	a := base()
	a.Remote.Voice.Socket = filepath.Join(tree, "w.sock")
	testutil.WantErrContaining(t, a.checkHostPathsOutsideTree(), "remote.voice.socket", "inside the mounted tree")
	// Voice off: the key is not read.
	a.Remote.Voice.Enabled = false
	if err := a.checkHostPathsOutsideTree(); err != nil {
		t.Fatal(err)
	}
}
