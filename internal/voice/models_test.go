package voice

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// testModel is a pinned model whose file is content.
func testModel(content string) Model {
	sum := sha256.Sum256([]byte(content))
	return Model{Name: "tiny", File: "ggml-tiny.bin", Revision: strings.Repeat("a", 40), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

func TestTableHasTheSpecModels(t *testing.T) {
	for _, name := range []string{"large-v3-turbo", "large-v3-turbo-q5_0"} {
		m, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the table", name)
		}
		if !strings.HasPrefix(m.URL(), "https://huggingface.co/ggerganov/whisper.cpp/resolve/") || !strings.HasSuffix(m.URL(), "/"+m.File) {
			t.Errorf("%s: URL %s", name, m.URL())
		}
	}
	if _, ok := Lookup(DefaultModel); !ok {
		t.Fatal("the default model is not in the table")
	}
	if _, ok := Lookup("medium"); ok {
		t.Fatal("an unknown name was found")
	}
}

// TestUnverifiedEntriesAreRefused: while a table value is UNVERIFIED (or a
// size unknown), the model cannot be used. Once the values are filled in,
// this checks their form instead.
func TestUnverifiedEntriesAreRefused(t *testing.T) {
	for _, m := range Models {
		unverified := m.Revision == Unverified || m.SHA256 == Unverified || m.Size <= 0
		if unverified && m.Pinned() == "" {
			t.Errorf("%s: an unverified entry counts as pinned", m.Name)
		}
		if !unverified && m.Pinned() != "" {
			t.Errorf("%s: filled in but malformed: %s", m.Name, m.Pinned())
		}
		if strings.Contains(m.URL(), "/main/") {
			t.Errorf("%s: the URL names a branch, not a commit", m.Name)
		}
	}
}

func TestPinned(t *testing.T) {
	good := testModel("x")
	if why := good.Pinned(); why != "" {
		t.Fatal(why)
	}
	for name, mut := range map[string]func(*Model){
		"revision unverified": func(m *Model) { m.Revision = Unverified },
		"sha unverified":      func(m *Model) { m.SHA256 = Unverified },
		"size unknown":        func(m *Model) { m.Size = 0 },
		"revision branch":     func(m *Model) { m.Revision = "main" },
		"sha upper":           func(m *Model) { m.SHA256 = strings.ToUpper(m.SHA256) },
		"file path":           func(m *Model) { m.File = "../x.bin" },
		"file dot":            func(m *Model) { m.File = ".x" },
	} {
		m := good
		mut(&m)
		if m.Pinned() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
