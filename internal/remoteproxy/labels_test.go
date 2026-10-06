package remoteproxy

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{
		"Via Roma 12":           "Via Roma 12",
		"  a \t\n b  ":          "a b",
		"x\u202ey":              "x y",                   // RLO: a format character
		"a\x00b\x07c":           "a b c",                 // controls
		"a\u2800b\u3164c":       "a b c",                 // glyphs that draw blank
		"\u200b":                "",                      // only a format character: dropped
		strings.Repeat("é", 70): strings.Repeat("é", 60), // by character
		"<b>bold</b>":           "<b>bold</b>",           // text, not markup: kept as is
	} {
		if got := CleanLabel(in); got != want {
			t.Errorf("CleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels([]byte(`{"deal-2":" Via Roma 12 ","x":"\u200b","n":"ok"}`))
	if err != nil || !maps.Equal(got, map[string]string{"deal-2": "Via Roma 12", "n": "ok"}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{`[]`, `{"a":1}`, `"s"`, `not json`, `{"a":"b"} trailing`, `{"a":"b"}{}`, `null`, ``} {
		if _, err := ParseLabels([]byte(bad)); err == nil {
			t.Errorf("ParseLabels(%s) accepted", bad)
		}
	}
}

func TestReadLabelsRefuses(t *testing.T) {
	tree := t.TempDir()
	write := func(rel, s string) { must(t, os.WriteFile(filepath.Join(tree, rel), []byte(s), 0o644)) }
	write("big.json", `{"a":"`+strings.Repeat("x", MaxLabelsFile)+`"}`)
	if _, err := ReadLabels(tree, "big.json"); !errors.Is(err, fsutil.ErrFileTooLarge) {
		t.Errorf("big: %v", err)
	}
	must(t, os.Symlink("/etc/hosts", filepath.Join(tree, "l.json")))
	if _, err := ReadLabels(tree, "l.json"); !errors.Is(err, fsutil.ErrSymlink) {
		t.Errorf("symlink: %v", err)
	}
	write("arr.json", `["a"]`)
	if _, err := ReadLabels(tree, "arr.json"); !errors.Is(err, ErrLabelsShape) {
		t.Errorf("array: %v", err)
	}
	write("ok.json", `{"w1":"a\u0000b","zz":"x"}`)
	if got, err := ReadLabels(tree, "ok.json"); err != nil || got["w1"] != "a b" {
		t.Errorf("good: %v %v", got, err)
	}
}

func TestLabelSourceCachesByMtimeAndSize(t *testing.T) {
	tree := t.TempDir()
	p := filepath.Join(tree, "labels.json")
	must(t, os.WriteFile(p, []byte(`{"a":"one"}`), 0o644))
	now := time.Unix(1000, 0)
	s := &LabelSource{Tree: tree, Rel: "labels.json", now: func() time.Time { return now }}
	if s.Labels()["a"] != "one" {
		t.Fatal("first read")
	}
	must(t, os.WriteFile(p, []byte(`{"a":"two!"}`), 0o644))
	now = now.Add(5 * time.Second)
	if s.Labels()["a"] != "one" {
		t.Fatal("inside 10 s the cache answers")
	}
	now = now.Add(6 * time.Second)
	if s.Labels()["a"] != "two!" {
		t.Fatal("after 10 s a changed size re-reads")
	}
	must(t, os.Remove(p))
	now = now.Add(11 * time.Second)
	if s.Labels() != nil {
		t.Fatal("a removed file is no labels")
	}
	var nilSrc *LabelSource
	if nilSrc.Labels() != nil {
		t.Fatal("nil source")
	}
}

// A caller that changes the map it got does not change the cache.
func TestLabelSourceHandsOutACopy(t *testing.T) {
	tree := t.TempDir()
	must(t, os.WriteFile(filepath.Join(tree, "labels.json"), []byte(`{"a":"one"}`), 0o644))
	s := &LabelSource{Tree: tree, Rel: "labels.json"}
	s.Labels()["a"] = "changed"
	if got := s.Labels()["a"]; got != "one" {
		t.Fatalf("the cache was changed through a returned map: %q", got)
	}
}
