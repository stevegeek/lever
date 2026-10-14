package config

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/voice"
)

const ttsProgram = "lever-tool-tts"

// ttsToolFlags are read only in a lever-tool-tts command.
var ttsToolFlags = map[string]hostPathKind{
	"server":       hostProgramOutside, // sherpa-onnx-offline-tts, run once per request
	"models":       hostPrivate,        // the model directory, and the runs' output
	"speak-socket": hostPrivate,        // the speech socket (remote.voice.speak_socket)
	"model":        hostUnchecked,
	"voice":        hostUnchecked,
	"speed":        hostUnchecked,
	"threads":      hostUnchecked,
	"max-chars":    hostUnchecked,
}

// isTTSCommand also matches a command that takes -speak-socket, so a renamed
// copy or wrapper keeps the checks.
func isTTSCommand(t Tool) bool {
	if t.External || len(t.Command) == 0 {
		return false
	}
	argv, _ := unwrapEnv(t.Command)
	if len(argv) == 0 {
		return false
	}
	if baseName(argv[0]) == ttsProgram {
		return true
	}
	for _, a := range argv[1:] {
		name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if a != name && (name == "speak-socket" || strings.HasPrefix(name, "speak-socket=")) {
			return true
		}
	}
	return false
}

// TTSTool is what config load reads of a broker tool that runs
// lever-tool-tts, as written. The tool checks its own flags again at start.
type TTSTool struct {
	Name                     string
	Tree                     string // -tree
	Server                   string // -server
	Models                   string // -models
	Model                    string // -model
	Voice                    string // -voice
	Socket                   string // -speak-socket
	Speed, Threads, MaxChars string
}

// EffectiveModel is -model with the tool's default.
func (t TTSTool) EffectiveModel() string {
	if t.Model == "" {
		return voice.DefaultSpeechModel
	}
	return t.Model
}

// TTSTools lists the broker tools that run lever-tool-tts.
func (a *App) TTSTools() []TTSTool {
	var out []TTSTool
	for _, t := range a.Broker.Tools {
		if !isTTSCommand(t) {
			continue
		}
		w := TTSTool{Name: t.Name}
		for _, f := range toolFlags(t) {
			switch f.name {
			case "tree":
				w.Tree = f.val
			case "server":
				w.Server = f.val
			case "models":
				w.Models = f.val
			case "model":
				w.Model = f.val
			case "voice":
				w.Voice = f.val
			case "speak-socket":
				w.Socket = f.val
			case "speed":
				w.Speed = f.val
			case "threads":
				w.Threads = f.val
			case "max-chars":
				w.MaxChars = f.val
			}
		}
		out = append(out, w)
	}
	return out
}

// SpeechTool is the tts tool whose -speak-socket is
// remote.voice.speak_socket.
func (a *App) SpeechTool() (TTSTool, bool) {
	if a.EffectiveReadAloud() != ReadAloudServer {
		return TTSTool{}, false
	}
	for _, t := range a.TTSTools() {
		if t.Socket != "" && t.Socket == a.Remote.Voice.SpeakSocket {
			return t, true
		}
	}
	return TTSTool{}, false
}

// validateTTSTools has no port to check: the tool runs its engine once per
// request as a child, and listens only on its socket and its MCP backend.
func (a *App) validateTTSTools() error {
	tools := a.TTSTools()
	var dictate []string
	for _, w := range a.WhisperTools() {
		dictate = append(dictate, w.Socket)
	}
	if a.RemoteEnabled() && a.Remote.Voice.Socket != "" {
		dictate = append(dictate, a.Remote.Voice.Socket)
	}
	for i, t := range tools {
		key := fmt.Sprintf("broker.tools[%s]", t.Name)
		if t.Socket == "" {
			return fmt.Errorf("config: %s -speak-socket is required: the absolute path of its speech socket, outside the tree (a tool that runs lever-tool-tts, or takes -speak-socket, is checked as the tts tool)", key)
		}
		if err := voice.CheckSocketPath(t.Socket); err != nil {
			return fmt.Errorf("config: %s -speak-socket: %w", key, err)
		}
		if slices.Contains(dictate, t.Socket) {
			return fmt.Errorf("config: %s -speak-socket %s is also a dictation socket; give each tool its own", key, t.Socket)
		}
		for _, o := range tools[i+1:] {
			if o.Socket == t.Socket {
				return fmt.Errorf("config: broker tools %q and %q name the same -speak-socket %s", t.Name, o.Name, t.Socket)
			}
		}
		if t.Models == "" || t.Server == "" {
			return fmt.Errorf("config: %s needs -models and -server (absolute host paths outside the tree)", key)
		}
		if t.Tree == "" {
			return fmt.Errorf("config: %s -tree is required: the instance's tree, %s", key, a.Tree)
		}
		if !sameTree(t.Tree, a.Tree) {
			return fmt.Errorf("config: %s -tree %s is not the instance's tree %s", key, t.Tree, a.Tree)
		}
		m, ok := voice.LookupSpeech(t.EffectiveModel())
		if !ok {
			return fmt.Errorf("config: %s -model %q is not in lever's speech model table; use one of %s", key, t.Model, strings.Join(voice.SpeechNames(), ", "))
		}
		if _, ok := m.Voice(cmp.Or(t.Voice, m.DefaultVoice)); !ok {
			return fmt.Errorf("config: %s -voice %q is not a voice of %s; use one of %s", key, t.Voice, m.Name, strings.Join(m.VoiceNames(), ", "))
		}
		if t.Speed != "" {
			f, err := strconv.ParseFloat(t.Speed, 64)
			if err != nil {
				return fmt.Errorf("config: %s -speed %q is not a number", key, t.Speed)
			}
			if err := voice.CheckSpeed(f); err != nil {
				return fmt.Errorf("config: %s -speed: %w", key, err)
			}
		}
		for _, n := range []struct {
			flag, raw string
			check     func(int) error
		}{{"-threads", t.Threads, voice.CheckThreads}, {"-max-chars", t.MaxChars, voice.CheckSpeakMaxChars}} {
			if n.raw == "" {
				continue
			}
			v, err := strconv.Atoi(n.raw)
			if err != nil {
				return fmt.Errorf("config: %s %s %q is not a number", key, n.flag, n.raw)
			}
			if err := n.check(v); err != nil {
				return fmt.Errorf("config: %s %s: %w", key, n.flag, err)
			}
		}
	}
	return nil
}
