package voice

// Text to speech: lever's table of pinned speech models for lever-tool-tts.
//
// A speech model is not one file, as a Whisper model is: sherpa-onnx loads
// an ONNX model, its token table, (for Kokoro) its voice embeddings, and
// espeak-ng's data directory for the phonemes. So each table entry is a set
// of files from one Hugging Face repository at one commit, each with its
// size and sha256, laid out on disk under <models>/<model name>/ as in the
// repository.

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// SpeechFile is one pinned file of a speech model: its path in the
// repository (and below the model's directory on disk), size and sha256.
type SpeechFile struct {
	Path   string
	Size   int64
	SHA256 string
}

// SpeechVoice is one voice of a speech model: the name the page and the
// tool's -voice use, the speaker id sherpa-onnx takes (--sid), and its
// language as a BCP 47 tag.
type SpeechVoice struct {
	Name string
	SID  int
	Lang string
}

// Speech model kinds: the sherpa-onnx model family, which decides its flags
// (sherpa.go).
const (
	KindKokoro = "kokoro"
	KindVITS   = "vits" // Piper voices are VITS models
)

// SpeechModel is one pinned speech model.
type SpeechModel struct {
	Name string
	Kind string
	// Repo is the Hugging Face repository, "owner/name".
	Repo string
	// Revision is the repository commit every file is pinned to (40 hex).
	Revision string
	// The roles of the files sherpa-onnx is given: paths in Files (DataDir
	// is a directory, the common prefix of the espeak-ng data files).
	// VoicesFile is Kokoro's only.
	ModelFile, TokensFile, VoicesFile, DataDir string
	Files                                      []SpeechFile
	Voices                                     []SpeechVoice
	DefaultVoice                               string
	// SampleRate is the model's output rate, in Hz (the answer's WAV header
	// is checked against it).
	SampleRate int
}

const speechRepoHost = "https://huggingface.co/"

// ---------------------------------------------------------------------------
// PINNED VALUES. Every value below that reads Unverified must be replaced by
// the repository's own, read on a machine with internet access:
//
//	git clone https://huggingface.co/<Repo> && cd <name> && git checkout <commit>
//	go run ./internal/voice/genspeechpins -dir <that clone> -revision <commit>
//
// genspeechpins prints the Files list (every file the model needs, the
// espeak-ng data directory included, with its size and sha256) to paste
// over the one below. Until then the model is refused by `lever voice
// fetch`, by lever-tool-tts and by `lever doctor`: nothing runs unpinned.
//
// The voices and their speaker ids are those sherpa-onnx documents for each
// model (kokoro-en-v0_19: 11 English voices, ids 0 to 10).
// ---------------------------------------------------------------------------

// DefaultSpeechModel is lever-tool-tts's -model when unset.
const DefaultSpeechModel = "kokoro-en-v0_19"

// SpeechModels is lever's table; every lookup reads it. Tests swap it.
var SpeechModels = []SpeechModel{
	{
		Name: "kokoro-en-v0_19", Kind: KindKokoro, Repo: "csukuangfj/kokoro-en-v0_19", Revision: Unverified,
		ModelFile: "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Files: []SpeechFile{
			{Path: "model.onnx", SHA256: Unverified},
			{Path: "voices.bin", SHA256: Unverified},
			{Path: "tokens.txt", SHA256: Unverified},
			{Path: "espeak-ng-data/phontab", SHA256: Unverified},
		},
		Voices: []SpeechVoice{
			{"af", 0, "en-US"}, {"af_bella", 1, "en-US"}, {"af_nicole", 2, "en-US"}, {"af_sarah", 3, "en-US"},
			{"af_sky", 4, "en-US"}, {"am_adam", 5, "en-US"}, {"am_michael", 6, "en-US"},
			{"bf_emma", 7, "en-GB"}, {"bf_isabella", 8, "en-GB"}, {"bm_george", 9, "en-GB"}, {"bm_lewis", 10, "en-GB"},
		},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	},
	{
		// Piper's en_GB "alba" (medium), the lighter choice: a smaller
		// model, one voice, less natural than Kokoro.
		Name: "piper-en_GB-alba-medium", Kind: KindVITS, Repo: "csukuangfj/vits-piper-en_GB-alba-medium", Revision: Unverified,
		ModelFile: "en_GB-alba-medium.onnx", TokensFile: "tokens.txt", DataDir: "espeak-ng-data",
		Files: []SpeechFile{
			{Path: "en_GB-alba-medium.onnx", SHA256: Unverified},
			{Path: "tokens.txt", SHA256: Unverified},
			{Path: "espeak-ng-data/phontab", SHA256: Unverified},
		},
		Voices:       []SpeechVoice{{"alba", 0, "en-GB"}},
		DefaultVoice: "alba", SampleRate: 22050,
	},
}

// SpeechNames lists the table's model names, in table order.
func SpeechNames() []string {
	out := make([]string, len(SpeechModels))
	for i, m := range SpeechModels {
		out[i] = m.Name
	}
	return out
}

// LookupSpeech finds a speech model by name.
func LookupSpeech(name string) (SpeechModel, bool) {
	i := slices.IndexFunc(SpeechModels, func(m SpeechModel) bool { return m.Name == name })
	if i < 0 {
		return SpeechModel{}, false
	}
	return SpeechModels[i], true
}

// Voice finds one of m's voices by name.
func (m SpeechModel) Voice(name string) (SpeechVoice, bool) {
	i := slices.IndexFunc(m.Voices, func(v SpeechVoice) bool { return v.Name == name })
	if i < 0 {
		return SpeechVoice{}, false
	}
	return m.Voices[i], true
}

// VoiceNames lists m's voice names, in table order.
func (m SpeechModel) VoiceNames() []string {
	out := make([]string, len(m.Voices))
	for i, v := range m.Voices {
		out[i] = v.Name
	}
	return out
}

// TotalSize is the sum of the pinned file sizes.
func (m SpeechModel) TotalSize() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
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

// Pinned reports why m cannot be used yet ("" = it can): a value is still
// Unverified or malformed, or the roles name no listed file.
func (m SpeechModel) Pinned() string {
	switch {
	case m.Revision == Unverified || len(m.Files) == 0:
		return fmt.Sprintf("lever's speech model table entry for %s is not verified yet (revision, and each file's size and sha256, must be pinned)", m.Name)
	case !isHex(m.Revision, 40):
		return fmt.Sprintf("speech model %s: the pinned revision is not a 40-hex commit", m.Name)
	case !cleanSpeechPath(m.Repo) || strings.Count(m.Repo, "/") != 1:
		return fmt.Sprintf("speech model %s: the repository %q is not owner/name", m.Name, m.Repo)
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
	seen := map[string]bool{}
	for _, f := range m.Files {
		switch {
		case f.SHA256 == Unverified || f.Size <= 0:
			return fmt.Sprintf("lever's speech model table entry for %s is not verified yet (%s: size and sha256 must be pinned)", m.Name, f.Path)
		case !isHex(f.SHA256, 64):
			return fmt.Sprintf("speech model %s: %s: the pinned sha256 is not 64 lowercase hex", m.Name, f.Path)
		case !cleanSpeechPath(f.Path):
			return fmt.Sprintf("speech model %s: the file path %q is not a plain relative path", m.Name, f.Path)
		case seen[f.Path]:
			return fmt.Sprintf("speech model %s lists %s twice", m.Name, f.Path)
		}
		seen[f.Path] = true
	}
	roles := []struct{ what, p string }{{"model", m.ModelFile}, {"tokens", m.TokensFile}}
	if m.Kind == KindKokoro {
		roles = append(roles, struct{ what, p string }{"voices", m.VoicesFile})
	}
	for _, r := range roles {
		if !seen[r.p] {
			return fmt.Sprintf("speech model %s: its %s file %q is not among its pinned files", m.Name, r.what, r.p)
		}
	}
	if !cleanSpeechPath(m.DataDir) || !slices.ContainsFunc(m.Files, func(f SpeechFile) bool { return strings.HasPrefix(f.Path, m.DataDir+"/") }) {
		return fmt.Sprintf("speech model %s: no pinned file under its data directory %q", m.Name, m.DataDir)
	}
	return ""
}

// FileURL is where file f of m is downloaded from: the repository at the
// pinned commit.
func (m SpeechModel) FileURL(f SpeechFile) string {
	parts := strings.Split(f.Path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return speechRepoHost + m.Repo + "/resolve/" + m.Revision + "/" + strings.Join(parts, "/")
}
