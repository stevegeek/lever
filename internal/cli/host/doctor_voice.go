package host

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
)

// checkVoice reports remote.voice: off, or on with its limits, and that the
// jail cannot reach the voice port (it is in no allow_ports: config
// validation refuses that, and this row says so again).
func checkVoice(app *config.App) checkResult {
	const name = "voice"
	if !app.VoiceOn() {
		return checkResult{name, true, "off (no mic on the chat page, no whisper-server)", ""}
	}
	port := app.EffectiveVoicePort()
	if slices.Contains(app.EffectiveAllowedPorts(), port) {
		return checkResult{name, false, fmt.Sprintf("the voice port %d is one the jail may reach: agents could talk to whisper-server, which has no authentication", port),
			"remove it from manager.allow_ports, or set remote.voice.port to another"}
	}
	lang := app.Remote.Voice.Language
	if lang == "" {
		lang = "detected per clip"
	}
	gpu := "on"
	if !app.VoiceGPU() {
		gpu = "off (--no-gpu)"
	}
	detail := fmt.Sprintf("on: model %s, max %d s a clip, language %s, GPU %s, %d vocabulary words; whisper-server on 127.0.0.1:%d, not reachable from the jail",
		app.EffectiveVoiceModel(), app.EffectiveVoiceMaxSeconds(), lang, gpu, len(app.Remote.Voice.Vocabulary), port)
	if x := app.VoiceExcludedLogins(); len(x) > 0 {
		detail += "; no dictation for " + strings.Join(x, ", ")
	}
	return checkResult{name, true, detail + "; audio and transcripts are never stored", ""}
}

// checkVoiceModel reports the configured model: pinned in lever's table,
// present in the state directory, and of the pinned size and sha256 (the
// whole file is hashed, which takes a few seconds for a large model).
func checkVoiceModel(app *config.App, st state.State) checkResult {
	const name = "voice model"
	if !app.VoiceOn() {
		return checkResult{}
	}
	m, ok := voice.Lookup(app.EffectiveVoiceModel())
	if !ok {
		return checkResult{name, false, fmt.Sprintf("%q is not in lever's model table", app.EffectiveVoiceModel()), "use one of " + strings.Join(voice.Names(), ", ")}
	}
	if why := m.Pinned(); why != "" {
		return checkResult{name, false, why + ": dictation stays off", "use a lever release whose model table pins " + m.Name}
	}
	if brokerctl.StateInsideTree(app, st) {
		return checkResult{name, false, "the state directory is inside the tree, where agents could replace the model: dictation stays off",
			"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"}
	}
	p := voice.ModelPath(st.VoiceModels(), m)
	err := voice.Verify(p, m)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return checkResult{name, false, m.Name + " is not downloaded: dictation stays off", "lever voice fetch " + m.Name + ", then restart the remote proxy"}
	case err != nil:
		return checkResult{name, false, err.Error() + ": dictation stays off", "remove " + p + " and run lever voice fetch " + m.Name}
	}
	return checkResult{name, true, fmt.Sprintf("%s: %s, %d bytes, sha256 matches lever's pin", m.Name, stateRel(st, p), m.Size), ""}
}

// checkVoiceServer reports remote.voice.whisper_server: an executable that
// only you or root can change (voice.CheckServer). Config load already
// refused one inside the tree.
func checkVoiceServer(app *config.App) checkResult {
	const name = "voice whisper-server"
	if !app.VoiceOn() {
		return checkResult{}
	}
	real, err := voice.CheckServer(app.Remote.Voice.WhisperServer)
	if err != nil {
		return checkResult{name, false, err.Error() + ": dictation stays off",
			"install whisper.cpp's whisper-server (built with CUDA or Metal) outside the tree, owned by you or root and writable by no one else, and point remote.voice.whisper_server at it"}
	}
	detail := real
	if real != app.Remote.Voice.WhisperServer {
		detail = app.Remote.Voice.WhisperServer + " → " + real
	}
	return checkResult{name, true, detail + " (executable, writable only by its owner)", ""}
}
