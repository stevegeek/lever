// Package voice is lever's speech to text on the host: lever's table of
// pinned Whisper models, their download (`lever voice fetch`) and check, the
// supervisor of the whisper.cpp server that lever-tool-whisper runs as its
// child on host loopback, the dictation socket between the remote proxy and
// that tool (dictate.go), and the tool's agent operation (agent.go).
//
// Every assumption about whisper.cpp itself (its flags, its HTTP form, its
// answer) lives in whisper.go, and nowhere else.
//
// Nothing here stores audio or a transcript: a dictation clip goes from the
// proxy's memory over the socket to the tool and on to the child over
// loopback, and the text comes back the same way.
package voice

import (
	"fmt"
	"slices"
	"strings"
)

// Unverified marks a table value nobody has checked against the published
// file yet. A model with any Unverified value is refused by `lever voice
// fetch`, by lever-tool-whisper and by `lever doctor`: nothing runs
// unpinned.
const Unverified = "UNVERIFIED"

// Model is one pinned model: a whisper.cpp ggml file on Hugging Face, at a
// fixed commit (never a branch), with the size and sha256 lever expects.
type Model struct {
	Name string
	// File is the file name in the repository and on disk.
	File string
	// Revision is the repository commit the URL is pinned to (40 hex).
	Revision string
	// Size is the file size in bytes (0 = unknown, which counts as
	// unverified).
	Size int64
	// SHA256 is the file's sha256, lowercase hex.
	SHA256 string
}

// modelRepo is the Hugging Face repository the models come from.
const modelRepo = "https://huggingface.co/ggerganov/whisper.cpp"

// ---------------------------------------------------------------------------
// PINNED VALUES, from the repository at one commit: the size and sha256 of
// each file as the hub's LFS metadata records them (the tree listing at that
// commit, and the resolve answer's X-Linked-Size and X-Linked-Etag, agree).
// A value set to Unverified (or a size of 0) makes the model refused.
//
// Approximate sizes, for orientation only: large-v3-turbo (f16) about
// 1.6 GB, large-v3-turbo-q5_0 about 0.55 GB.
// ---------------------------------------------------------------------------
const (
	pinnedRevision = "5359861c739e955e79d9a303bcbc70fb988958b1" // the commit of modelRepo both files are taken from

	largeV3TurboSize   int64 = 1624555275
	largeV3TurboSHA256       = "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69"

	largeV3TurboQ5Size   int64 = 574041195
	largeV3TurboQ5SHA256       = "394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2"
)

// DefaultModel is lever-tool-whisper's -model when unset.
const DefaultModel = "large-v3-turbo"

// Models is lever's table; every lookup reads it. Tests swap it for one
// with values of their own.
var Models = []Model{
	{Name: "large-v3-turbo", File: "ggml-large-v3-turbo.bin", Revision: pinnedRevision, Size: largeV3TurboSize, SHA256: largeV3TurboSHA256},
	{Name: "large-v3-turbo-q5_0", File: "ggml-large-v3-turbo-q5_0.bin", Revision: pinnedRevision, Size: largeV3TurboQ5Size, SHA256: largeV3TurboQ5SHA256},
}

// Names lists the table's model names, in table order.
func Names() []string {
	out := make([]string, len(Models))
	for i, m := range Models {
		out[i] = m.Name
	}
	return out
}

// Lookup finds a model by name.
func Lookup(name string) (Model, bool) {
	i := slices.IndexFunc(Models, func(m Model) bool { return m.Name == name })
	if i < 0 {
		return Model{}, false
	}
	return Models[i], true
}

// Pinned reports why m cannot be used yet ("" = it can): a value of its
// table entry is still Unverified, or malformed.
func (m Model) Pinned() string {
	switch {
	case m.Revision == Unverified || m.SHA256 == Unverified || m.Size <= 0:
		return fmt.Sprintf("lever's model table entry for %s is not verified yet (revision, size and sha256 must be pinned)", m.Name)
	case !isHex(m.Revision, 40):
		return fmt.Sprintf("model %s: the pinned revision is not a 40-hex commit", m.Name)
	case !isHex(m.SHA256, 64):
		return fmt.Sprintf("model %s: the pinned sha256 is not 64 lowercase hex", m.Name)
	case m.File == "" || strings.ContainsAny(m.File, `/\`) || strings.HasPrefix(m.File, "."):
		return fmt.Sprintf("model %s: the file name %q is not a plain name", m.Name, m.File)
	}
	return ""
}

// URL is where m is downloaded from: the repository at the pinned commit.
func (m Model) URL() string {
	return modelRepo + "/resolve/" + m.Revision + "/" + m.File
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
