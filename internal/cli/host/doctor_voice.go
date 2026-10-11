package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/voice"
)

// checkVoice reports remote.voice: off, or on with its socket and limits,
// the lever-tool-whisper entry that serves the socket, and that the jail
// cannot reach the tool's -whisper-port (config validation refuses that,
// and this row says so again).
func checkVoice(app *config.App) checkResult {
	const name = "voice"
	if !app.VoiceOn() {
		return checkResult{name, true, "off (no mic on the chat page)", ""}
	}
	detail := fmt.Sprintf("on: max %d s a clip, through the socket %s", app.EffectiveVoiceMaxSeconds(), app.Remote.Voice.Socket)
	w, ok := app.VoiceTool()
	switch {
	case ok:
		if slices.Contains(app.EffectiveAllowedPorts(), w.Port) {
			return checkResult{name, false, fmt.Sprintf("broker tool %q's -whisper-port %d is one the jail may reach: agents could talk to whisper-server, which has no authentication", w.Name, w.Port),
				"remove it from manager.allow_ports, or give the tool another -whisper-port"}
		}
		detail += fmt.Sprintf(" of broker tool %q; whisper-server on 127.0.0.1:%d, not reachable from the jail", w.Name, w.Port)
	default:
		detail += " (no lever-tool-whisper entry in broker.tools: the tool serving it is yours to run)"
	}
	if x := app.VoiceExcludedLogins(); len(x) > 0 {
		detail += "; no dictation for " + strings.Join(x, ", ")
	}
	return checkResult{name, true, detail + "; audio and transcripts are never stored", ""}
}

// doctorWhisperTool is the whisper tool the `voice tool` and `voice model`
// rows check: with remote.voice on, dictation's (VoiceTool: ok false when
// no lever-tool-whisper entry serves remote.voice.socket); with it off, the
// first lever-tool-whisper entry in broker.tools, which agents may use on
// their own (ok false: none). lost names what a failure costs.
func doctorWhisperTool(app *config.App) (w config.WhisperTool, ok bool, lost string) {
	if app.VoiceOn() {
		w, ok = app.VoiceTool()
		return w, ok, "no dictation"
	}
	if ws := app.WhisperTools(); len(ws) > 0 {
		return ws[0], true, "agents cannot transcribe"
	}
	return config.WhisperTool{}, false, ""
}

// toolLogHint names the log of the whisper tool w (ok false: none
// configured; socket is the one it should serve), for a fix line.
func toolLogHint(w config.WhisperTool, ok bool, socket string) string {
	if ok {
		return "see " + filepath.Join("tool-logs", w.Name+".log") + " in the state directory"
	}
	return "see the log of the lever-tool-whisper that serves " + socket
}

// checkVoiceTool asks lever-tool-whisper's socket for its health: not
// running, running with whisper-server not ready, or ready (with the model).
// With remote.voice on it asks remote.voice.socket; with it off, the
// -dictate-socket of the first whisper tool, so a tool used by agents alone
// is checked too. No whisper tool and voice off: no row.
func checkVoiceTool(ctx context.Context, app *config.App) checkResult {
	const name = "voice tool"
	w, ok, lost := doctorWhisperTool(app)
	socket := app.Remote.Voice.Socket
	switch {
	case app.VoiceOn():
	case !ok || w.Socket == "":
		return checkResult{}
	default:
		socket = w.Socket
	}
	h, err := (&voice.DictateClient{Socket: socket}).Health(ctx)
	switch {
	case err != nil:
		return checkResult{name, false, fmt.Sprintf("not running: %s does not answer (%v): %s", socket, err, lost),
			"start the broker (lever up or lever reload); if the tool is configured, " + toolLogHint(w, ok, socket)}
	case !h.Ready:
		return checkResult{name, false, fmt.Sprintf("running, but whisper-server is not ready (starting, or its program or the model %s failed a check): %s", h.Model, lost),
			toolLogHint(w, ok, socket) + "; fetch the model with lever voice fetch if it is missing"}
	}
	detail := fmt.Sprintf("ready: model %s, at most %d s a clip", h.Model, h.MaxSeconds)
	if !app.VoiceOn() {
		detail += fmt.Sprintf(", agent clips at most %d s (broker tool %q, for agents; remote.voice is off)", w.EffectiveAgentMaxSeconds(), w.Name)
	}
	return checkResult{name, true, detail, ""}
}

// checkVoiceModel reports the tool's model (doctorWhisperTool): pinned in
// lever's table, present in the tool's -models directory, and of the pinned
// size and sha256 (the whole file is hashed, which takes a few seconds for
// a large model). It runs for any configured whisper tool, remote.voice on
// or off.
func checkVoiceModel(app *config.App) checkResult {
	const name = "voice model"
	w, ok, lost := doctorWhisperTool(app)
	if !ok {
		if !app.VoiceOn() {
			return checkResult{}
		}
		return checkResult{name, true, "not checked: no lever-tool-whisper entry in broker.tools serves " + app.Remote.Voice.Socket, ""}
	}
	m, found := voice.Lookup(w.EffectiveModel())
	if !found {
		return checkResult{name, false, fmt.Sprintf("broker tool %q's -model %q is not in lever's model table", w.Name, w.EffectiveModel()), "use one of " + strings.Join(voice.Names(), ", ")}
	}
	if why := m.Pinned(); why != "" {
		return checkResult{name, false, why + ": " + lost, "use a lever release whose model table pins " + m.Name}
	}
	if !filepath.IsAbs(w.Models) {
		return checkResult{name, false, fmt.Sprintf("broker tool %q's -models %q is not an absolute path", w.Name, w.Models), "point -models at the absolute path lever voice fetch downloads into"}
	}
	p := voice.ModelPath(w.Models, m)
	err := voice.Verify(p, m)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return checkResult{name, false, m.Name + " is not downloaded into " + w.Models + ": " + lost, "lever voice fetch " + m.Name + " (the tool picks it up within a minute)"}
	case err != nil:
		return checkResult{name, false, err.Error() + ": " + lost, "remove " + p + " and run lever voice fetch " + m.Name}
	}
	return checkResult{name, true, fmt.Sprintf("%s: %s, %d bytes, sha256 matches lever's pin", m.Name, p, m.Size), ""}
}
