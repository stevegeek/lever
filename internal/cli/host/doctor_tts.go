package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/voice"
)

// doctorTTSTool is the tts tool the tts rows check: with read_aloud: server,
// read-aloud's (ok false when no lever-tool-tts entry serves
// remote.voice.speak_socket); otherwise the first lever-tool-tts entry. lost
// names what a failure costs.
func doctorTTSTool(app *config.App) (t config.TTSTool, ok bool, lost string) {
	if app.EffectiveReadAloud() == config.ReadAloudServer {
		t, ok = app.SpeechTool()
		return t, ok, "read-aloud falls back to the device's voices"
	}
	if ts := app.TTSTools(); len(ts) > 0 {
		return ts[0], true, "no server voices (remote.voice.read_aloud is not server, so the chat page does not use them anyway)"
	}
	return config.TTSTool{}, false, ""
}

// checkTTSTool asks lever-tool-tts's speech socket for its health: with
// read_aloud: server remote.voice.speak_socket, else the first tts tool's
// -speak-socket. Neither: no row.
func checkTTSTool(ctx context.Context, app *config.App) checkResult {
	const name = "tts tool"
	t, ok, lost := doctorTTSTool(app)
	socket := app.Remote.Voice.SpeakSocket
	switch {
	case app.EffectiveReadAloud() == config.ReadAloudServer:
	case !ok || t.Socket == "":
		return checkResult{}
	default:
		socket = t.Socket
	}
	logHint := "see the log of the lever-tool-tts that serves " + socket
	if ok {
		logHint = "see " + filepath.Join("tool-logs", t.Name+".log") + " in the state directory"
	}
	h, err := (&voice.SpeakClient{Socket: socket}).Health(ctx)
	switch {
	case err != nil:
		return checkResult{name, false, fmt.Sprintf("not running: %s does not answer (%v): %s", socket, err, lost),
			"start the broker (lever up or lever reload); if the tool is configured, " + logHint}
	case !h.Ready:
		return checkResult{name, false, fmt.Sprintf("running, but not ready (its sherpa-onnx program or the model %s failed a check): %s", h.Model, lost),
			logHint + "; fetch the model with lever voice fetch " + h.Model + " if it is missing"}
	}
	detail := fmt.Sprintf("ready: model %s, voice %s of %d, at most %d characters a request; one sherpa-onnx run per request, no port, only the private socket %s",
		h.Model, h.Voice, len(h.Voices), h.MaxChars, socket)
	if app.EffectiveReadAloud() != config.ReadAloudServer {
		detail += " (remote.voice.read_aloud is not server: the chat page does not use it)"
	}
	return checkResult{name, true, detail, ""}
}

// checkTTSModel reports the tool's speech model: pinned in lever's table,
// present in -models, every file of the size and sha256 recorded when unpacked
// from the verified archive, and no other file (voice.VerifySpeech).
func checkTTSModel(app *config.App) checkResult {
	const name = "tts model"
	t, ok, lost := doctorTTSTool(app)
	if !ok {
		if app.EffectiveReadAloud() != config.ReadAloudServer {
			return checkResult{}
		}
		return checkResult{name, true, "not checked: no lever-tool-tts entry in broker.tools serves " + app.Remote.Voice.SpeakSocket, ""}
	}
	m, found := voice.LookupSpeech(t.EffectiveModel())
	if !found {
		return checkResult{name, false, fmt.Sprintf("broker tool %q's -model %q is not in lever's speech model table", t.Name, t.EffectiveModel()), "use one of " + strings.Join(voice.SpeechNames(), ", ")}
	}
	if why := m.Pinned(); why != "" {
		return checkResult{name, false, why + ": " + lost, "use a lever release whose speech model table pins " + m.Name}
	}
	if !filepath.IsAbs(t.Models) {
		return checkResult{name, false, fmt.Sprintf("broker tool %q's -models %q is not an absolute path", t.Name, t.Models), "point -models at the absolute path lever voice fetch downloads into"}
	}
	files, err := voice.VerifySpeech(t.Models, m)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return checkResult{name, false, m.Name + " is not downloaded into " + t.Models + ": " + lost, "lever voice fetch " + m.Name + " (the tool picks it up at its next request)"}
	case err != nil:
		return checkResult{name, false, err.Error() + ": " + lost, "remove " + voice.SpeechDir(t.Models, m) + " and run lever voice fetch " + m.Name}
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	return checkResult{name, true, fmt.Sprintf("%s: %s, %d files, %d bytes, each as unpacked from the pinned archive (sha256 %.12s...)", m.Name, voice.SpeechDir(t.Models, m), len(files), size, m.ArchiveSHA256), ""}
}
