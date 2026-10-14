package voice

// Text to speech: lever's table of pinned speech models for lever-tool-tts.
//
// A speech model is a hundred files and more (an ONNX model, its token
// table, Kokoro's voice embeddings, espeak-ng's data). sherpa-onnx publishes
// each as one archive in its "tts-models" GitHub release, so lever pins that
// archive's URL, size and sha256, unpacks it, and records every file's size
// and sha256 in a manifest (speechfiles.go). Every file the engine loads is
// checked against that manifest before a run, or refused.

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// SpeechFile is one unpacked file: its slash path below the model's
// directory, size and sha256.
type SpeechFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SpeechVoice is one voice of a speech model: the name the page and -voice
// use, the speaker id (--sid), and its BCP 47 language tag.
type SpeechVoice struct {
	Name string
	SID  int
	Lang string
}

// Speech model kinds: the sherpa-onnx model family decides its flags
// (sherpa.go).
const (
	KindKokoro = "kokoro"
	KindVITS   = "vits" // Piper voices are VITS models
)

type SpeechModel struct {
	Name string
	Kind string
	// ArchiveURL is a .tar.bz2 asset of sherpa-onnx's tts-models release;
	// ArchiveSize and ArchiveSHA256 are its pins.
	ArchiveURL    string
	ArchiveSize   int64
	ArchiveSHA256 string
	// ArchiveRoot is the archive's one top directory; what is below it is
	// unpacked into the model's directory.
	ArchiveRoot string
	// The files sherpa-onnx is given, as paths below the model's directory
	// (DataDir is espeak-ng's data). VoicesFile is Kokoro's only.
	ModelFile, TokensFile, VoicesFile, DataDir string
	Voices                                     []SpeechVoice
	DefaultVoice                               string
	// SampleRate is the model's output rate in Hz; the answer's WAV header
	// is checked against it.
	SampleRate int
}

const speechReleaseURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/"

// Pinned values come from GitHub's API for the assets of sherpa-onnx's
// tts-models release (repos/k2-fsa/sherpa-onnx/releases/tags/tts-models
// and /releases/assets/<id>): each asset's "size" and "digest" (the sha256
// GitHub computed at upload). A re-uploaded asset gets a new digest, which
// `lever voice fetch` refuses until the table is updated.
//
//	kokoro-en-v0_19.tar.bz2               asset 281054203, uploaded 2025-08-10T02:45:48Z
//	vits-piper-en_GB-alba-medium.tar.bz2  asset 268506267, uploaded 2025-06-30T02:23:04Z
//
// Voices and speaker ids are those sherpa-onnx documents for each model
// (k2-fsa.github.io/sherpa/onnx/tts/pretrained_models/kokoro.html) and
// writes into it (scripts/kokoro/v0.19/generate_voices_bin.py,
// add_meta_data.py). The engine does not refuse a wrong id: it speaks an id
// out of range with voice 0.
//
// A value set to Unverified (or a size of 0) makes the model refused by
// fetch, by lever-tool-tts and by doctor.

// DefaultSpeechModel is lever-tool-tts's -model when unset.
const DefaultSpeechModel = "kokoro-en-v0_19"

// SpeechModels is lever's table; tests swap it.
var SpeechModels = []SpeechModel{
	{
		// Kokoro-82M v0.19, English only (Apache-2.0): espeak-ng alone makes
		// the phonemes, so it needs no lexicon or language flag and every
		// sherpa-onnx release with Kokoro runs it.
		Name: "kokoro-en-v0_19", Kind: KindKokoro,
		ArchiveURL: speechReleaseURL + "kokoro-en-v0_19.tar.bz2", ArchiveSize: 319625534,
		ArchiveSHA256: "912804855a04745fa77a30be545b3f9a5d15c4d66db00b88cbcd4921df605ac7",
		ArchiveRoot:   "kokoro-en-v0_19",
		ModelFile:     "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Voices: []SpeechVoice{
			{"af", 0, "en-US"}, {"af_bella", 1, "en-US"}, {"af_nicole", 2, "en-US"}, {"af_sarah", 3, "en-US"},
			{"af_sky", 4, "en-US"}, {"am_adam", 5, "en-US"}, {"am_michael", 6, "en-US"},
			{"bf_emma", 7, "en-GB"}, {"bf_isabella", 8, "en-GB"}, {"bm_george", 9, "en-GB"}, {"bm_lewis", 10, "en-GB"},
		},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	},
	{
		// Piper's en_GB "alba" (medium), the lighter choice: smaller, one
		// voice, less natural than Kokoro. Piper's config says 22050 Hz and
		// one speaker.
		Name: "piper-en_GB-alba-medium", Kind: KindVITS,
		ArchiveURL: speechReleaseURL + "vits-piper-en_GB-alba-medium.tar.bz2", ArchiveSize: 67212349,
		ArchiveSHA256: "fcd45962906933eec4431d3688f7d74aaac8713c87c6717f91fd3b23463aa1a1",
		ArchiveRoot:   "vits-piper-en_GB-alba-medium",
		ModelFile:     "en_GB-alba-medium.onnx", TokensFile: "tokens.txt", DataDir: "espeak-ng-data",
		Voices:       []SpeechVoice{{"alba", 0, "en-GB"}},
		DefaultVoice: "alba", SampleRate: 22050,
	},
}

func SpeechNames() []string {
	out := make([]string, len(SpeechModels))
	for i, m := range SpeechModels {
		out[i] = m.Name
	}
	return out
}

func LookupSpeech(name string) (SpeechModel, bool) {
	i := slices.IndexFunc(SpeechModels, func(m SpeechModel) bool { return m.Name == name })
	if i < 0 {
		return SpeechModel{}, false
	}
	return SpeechModels[i], true
}

func (m SpeechModel) Voice(name string) (SpeechVoice, bool) {
	i := slices.IndexFunc(m.Voices, func(v SpeechVoice) bool { return v.Name == name })
	if i < 0 {
		return SpeechVoice{}, false
	}
	return m.Voices[i], true
}

func (m SpeechModel) VoiceNames() []string {
	out := make([]string, len(m.Voices))
	for i, v := range m.Voices {
		out[i] = v.Name
	}
	return out
}

// cleanSpeechPath accepts a relative slash path of plain components: no
// empty, "." or ".." component, none starting with a dot, no backslash.
func cleanSpeechPath(p string) bool {
	if p == "" || strings.ContainsAny(p, `\`) || path.Clean(p) != p || path.IsAbs(p) {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || strings.HasPrefix(c, ".") {
			return false
		}
		for _, r := range c {
			if r < 0x21 || r > 0x7e {
				return false
			}
		}
	}
	return true
}

// espeakDataFiles are the files sherpa-onnx requires in espeak-ng's data
// directory.
var espeakDataFiles = []string{"phontab", "phonindex", "phondata", "intonations"}

// RequiredFiles are the paths below the model's directory that sherpa-onnx
// is given or requires: an archive without one of them is refused.
func (m SpeechModel) RequiredFiles() []string {
	out := []string{m.ModelFile, m.TokensFile}
	if m.Kind == KindKokoro {
		out = append(out, m.VoicesFile)
	}
	for _, f := range espeakDataFiles {
		out = append(out, m.DataDir+"/"+f)
	}
	return out
}

// Pinned reports why m cannot be used yet ("" = it can): a value is
// Unverified or malformed.
func (m SpeechModel) Pinned() string {
	switch {
	case !cleanSpeechPath(m.Name) || strings.Contains(m.Name, "/"):
		return fmt.Sprintf("speech model %q: the name is not a plain name", m.Name)
	case m.ArchiveSHA256 == Unverified || m.ArchiveSize <= 0:
		return fmt.Sprintf("lever's speech model table entry for %s is not verified yet (the archive's size and sha256 must be pinned)", m.Name)
	case !isHex(m.ArchiveSHA256, 64):
		return fmt.Sprintf("speech model %s: the pinned sha256 is not 64 lowercase hex", m.Name)
	case !strings.HasSuffix(m.ArchiveURL, ".tar.bz2"):
		return fmt.Sprintf("speech model %s: the archive %q is not a .tar.bz2", m.Name, m.ArchiveURL)
	case !speechURLOK(m.ArchiveURL):
		return fmt.Sprintf("speech model %s: the archive %q is not on sherpa-onnx's GitHub releases", m.Name, m.ArchiveURL)
	case !cleanSpeechPath(m.ArchiveRoot) || strings.Contains(m.ArchiveRoot, "/"):
		return fmt.Sprintf("speech model %s: the archive's top directory %q is not a plain name", m.Name, m.ArchiveRoot)
	case m.Kind != KindKokoro && m.Kind != KindVITS:
		return fmt.Sprintf("speech model %s: unknown kind %q", m.Name, m.Kind)
	case m.SampleRate < 8000 || m.SampleRate > 48000:
		return fmt.Sprintf("speech model %s: sample rate %d", m.Name, m.SampleRate)
	case len(m.Voices) == 0:
		return fmt.Sprintf("speech model %s has no voices", m.Name)
	}
	if _, ok := m.Voice(m.DefaultVoice); !ok {
		return fmt.Sprintf("speech model %s: the default voice %q is not one of its voices", m.Name, m.DefaultVoice)
	}
	for _, p := range m.RequiredFiles() {
		if !cleanSpeechPath(p) {
			return fmt.Sprintf("speech model %s: the file path %q is not a plain relative path", m.Name, p)
		}
	}
	return ""
}

// speechURLOK accepts an https URL on github.com, with no user, port,
// query or fragment.
func speechURLOK(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Host == "github.com" && u.RawQuery == "" && u.Fragment == "" &&
		strings.HasPrefix(u.Path, "/") && path.Clean(u.Path) == u.Path
}
