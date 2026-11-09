package voice

// Text to speech: lever's table of pinned speech models for lever-tool-tts.
//
// A speech model is a hundred files and more (an ONNX model, its token
// table, Kokoro's voice embeddings, pronunciation lexicons). sherpa-onnx
// publishes each as one archive in its "tts-models" GitHub release, so lever
// pins that archive's URL, size and sha256, unpacks it, and records every
// file's size and sha256 in a manifest (speechfiles.go). A file the engine
// needs that is published on its own (Extras: the English lexicon) is pinned
// the same way and placed beside the unpacked files. Every file the engine
// loads is checked against that manifest before a run, or refused.

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
// use, the speaker id (--sid), and its BCP 47 language tag, which picks the
// model's lexicons (SpeechModel.Lexicons).
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

// SpeechExtra is a file of a speech model that sherpa-onnx publishes on its
// own rather than in the model's archive: its release URL, pinned size and
// sha256, and the path it is placed at below the model's directory.
type SpeechExtra struct {
	Path   string
	URL    string
	Size   int64
	SHA256 string
}

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
	// The files sherpa-onnx is given, as paths below the model's directory.
	// VoicesFile is Kokoro's only.
	ModelFile, TokensFile, VoicesFile string
	// Extras are the files fetched on their own and placed beside the
	// archive's.
	Extras []SpeechExtra
	// Lexicons are, for each voice language, the lexicon files sherpa-onnx
	// is given, in order: where two hold the same word the later one wins,
	// so the accent's own lexicon comes last.
	Lexicons     map[string][]string
	Voices       []SpeechVoice
	DefaultVoice string
	// SampleRate is the model's output rate in Hz; the answer's WAV header
	// is checked against it.
	SampleRate int
}

const speechReleaseURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/"

// Pinned values come from GitHub's API for the assets of sherpa-onnx's
// tts-models release (repos/k2-fsa/sherpa-onnx/releases/tags/tts-models
// and /releases/assets/<id>): each asset's "size" and "digest" (the sha256
// GitHub computed at upload), and each was downloaded and checked against
// them. A re-uploaded asset gets a new digest, which `lever voice fetch`
// refuses until the table is updated.
//
//	kokoro-multi-lang-v1_0.tar.bz2        asset 677610079, uploaded 2026-10-26T09:08:34Z
//	vits-piper-en_GB-alba-medium.tar.bz2  asset 268506267, uploaded 2025-06-30T02:23:04Z
//	lexicon-en-us.txt                     asset 636425290, uploaded 2026-10-13T08:41:48Z
//
// Kokoro's voices and speaker ids are the model's own: the speaker2id
// metadata in kokoro-multi-lang-v1_0's model.onnx. lever offers its English
// voices. The engine does not refuse a wrong id: it speaks an id out of
// range with voice 0.
//
// sherpa-onnx 2.0 dropped espeak-ng: a word reaches the model only through
// a lexicon, and the engine skips a word in none of them. The Kokoro
// archive's own lexicons (lexicon-us-en.txt, lexicon-gb-en.txt) hold few
// inflected forms ("opened" and "agents" are missing), so the separate
// lexicon-en-us.txt goes first for every voice and the accent's own lexicon
// last. What is in none of them lever spells out first (speechtext.go).
//
// A value set to Unverified (or a size of 0) makes the model refused by
// fetch, by lever-tool-tts and by doctor.

// DefaultSpeechModel is lever-tool-tts's -model when unset.
const DefaultSpeechModel = "kokoro-multi-lang-v1_0"

// lexiconEnUS is sherpa-onnx's English lexicon, published on its own.
var lexiconEnUS = SpeechExtra{
	Path: "lexicon-en-us.txt", URL: speechReleaseURL + "lexicon-en-us.txt", Size: 6305773,
	SHA256: "8b67ae549a465eee78f92ecb74644ad90c3c58ce65f19c68baf85878a10ef73a",
}

// SpeechModels is lever's table; tests swap it.
var SpeechModels = []SpeechModel{
	{
		// Kokoro-82M v1.0 (Apache-2.0) as sherpa-onnx packages it for 2.x:
		// lever uses its American and British English voices.
		Name: "kokoro-multi-lang-v1_0", Kind: KindKokoro,
		ArchiveURL: speechReleaseURL + "kokoro-multi-lang-v1_0.tar.bz2", ArchiveSize: 349886476,
		ArchiveSHA256: "60703707bc05ccd48d60e1eb723f38b07d6e474795a0b6a588d7220400e0f7a4",
		ArchiveRoot:   "kokoro-multi-lang-v1_0",
		ModelFile:     "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin",
		Extras: []SpeechExtra{lexiconEnUS},
		Lexicons: map[string][]string{
			"en-US": {lexiconEnUS.Path, "lexicon-us-en.txt"},
			"en-GB": {lexiconEnUS.Path, "lexicon-gb-en.txt"},
		},
		Voices: []SpeechVoice{
			{"af_alloy", 0, "en-US"}, {"af_aoede", 1, "en-US"}, {"af_bella", 2, "en-US"}, {"af_heart", 3, "en-US"},
			{"af_jessica", 4, "en-US"}, {"af_kore", 5, "en-US"}, {"af_nicole", 6, "en-US"}, {"af_nova", 7, "en-US"},
			{"af_river", 8, "en-US"}, {"af_sarah", 9, "en-US"}, {"af_sky", 10, "en-US"},
			{"am_adam", 11, "en-US"}, {"am_echo", 12, "en-US"}, {"am_eric", 13, "en-US"}, {"am_fenrir", 14, "en-US"},
			{"am_liam", 15, "en-US"}, {"am_michael", 16, "en-US"}, {"am_onyx", 17, "en-US"}, {"am_puck", 18, "en-US"},
			{"am_santa", 19, "en-US"},
			{"bf_alice", 20, "en-GB"}, {"bf_emma", 21, "en-GB"}, {"bf_isabella", 22, "en-GB"}, {"bf_lily", 23, "en-GB"},
			{"bm_daniel", 24, "en-GB"}, {"bm_fable", 25, "en-GB"}, {"bm_george", 26, "en-GB"}, {"bm_lewis", 27, "en-GB"},
		},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	},
	{
		// Piper's en_GB "alba" (medium), the lighter choice: smaller, one
		// voice, less natural than Kokoro. Piper's config says 22050 Hz and
		// one speaker. sherpa-onnx's only English lexicon for VITS models is
		// American, so alba speaks with American pronunciations.
		Name: "piper-en_GB-alba-medium", Kind: KindVITS,
		ArchiveURL: speechReleaseURL + "vits-piper-en_GB-alba-medium.tar.bz2", ArchiveSize: 67212349,
		ArchiveSHA256: "fcd45962906933eec4431d3688f7d74aaac8713c87c6717f91fd3b23463aa1a1",
		ArchiveRoot:   "vits-piper-en_GB-alba-medium",
		ModelFile:     "en_GB-alba-medium.onnx", TokensFile: "tokens.txt",
		Extras:       []SpeechExtra{lexiconEnUS},
		Lexicons:     map[string][]string{"en-GB": {lexiconEnUS.Path}},
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

// LexiconFiles are the lexicons sherpa-onnx is given for voice v, in order.
func (m SpeechModel) LexiconFiles(v SpeechVoice) []string { return m.Lexicons[v.Lang] }

// isExtra reports whether p is one of m's Extras.
func (m SpeechModel) isExtra(p string) bool {
	return slices.ContainsFunc(m.Extras, func(e SpeechExtra) bool { return e.Path == p })
}

// RequiredFiles are the paths below the model's directory that sherpa-onnx
// is given from the archive: an archive without one of them is refused.
// Extras are not among them.
func (m SpeechModel) RequiredFiles() []string {
	out := []string{m.ModelFile, m.TokensFile}
	if m.Kind == KindKokoro {
		out = append(out, m.VoicesFile)
	}
	langs := make([]string, 0, len(m.Lexicons))
	for l := range m.Lexicons {
		langs = append(langs, l)
	}
	slices.Sort(langs)
	for _, l := range langs {
		for _, p := range m.Lexicons[l] {
			if !m.isExtra(p) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
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
	for _, v := range m.Voices {
		if len(m.LexiconFiles(v)) == 0 {
			return fmt.Sprintf("speech model %s: no lexicon for the voice %s (%s)", m.Name, v.Name, v.Lang)
		}
	}
	// A lexicon list is passed comma-separated, so no path may hold one.
	for _, p := range m.RequiredFiles() {
		if !cleanSpeechPath(p) || strings.Contains(p, ",") {
			return fmt.Sprintf("speech model %s: the file path %q is not a plain relative path", m.Name, p)
		}
	}
	for _, e := range m.Extras {
		switch {
		case !cleanSpeechPath(e.Path) || strings.Contains(e.Path, ","):
			return fmt.Sprintf("speech model %s: the extra file path %q is not a plain relative path", m.Name, e.Path)
		case slices.Contains([]string{m.ModelFile, m.TokensFile, m.VoicesFile}, e.Path):
			return fmt.Sprintf("speech model %s: the extra file %s has the name of one of the archive's files", m.Name, e.Path)
		case e.SHA256 == Unverified || e.Size <= 0:
			return fmt.Sprintf("lever's speech model table entry for %s is not verified yet (the size and sha256 of %s must be pinned)", m.Name, e.Path)
		case !isHex(e.SHA256, 64):
			return fmt.Sprintf("speech model %s: the pinned sha256 of %s is not 64 lowercase hex", m.Name, e.Path)
		case !speechURLOK(e.URL):
			return fmt.Sprintf("speech model %s: %s's URL %q is not on sherpa-onnx's GitHub releases", m.Name, e.Path, e.URL)
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
