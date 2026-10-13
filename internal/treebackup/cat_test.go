package treebackup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCat(t *testing.T) {
	b := memBlobs{}
	es := []Entry{
		dirE("d"), fileE(b, "d/f", "hello\n"),
		with(fileE(nil, ".lever/bootstrap.json", "ticket"), func(e *Entry) { e.State = StateSecret }),
		with(fileE(nil, "u", ""), func(e *Entry) { e.State, e.SHA256, e.Verified = StateUnreadable, "", 0 }),
		with(fileE(nil, "s", "x"), func(e *Entry) { e.State, e.Reason, e.SHA256, e.Verified = StateSkipped, ReasonRead, "", 0 }),
		linkE("l", "d/f"),
		fileE(nil, "lost", "gone"),
	}
	es = append(es, dirE(".lever"))
	m := memManifest(hdr("s"), es)

	var out bytes.Buffer
	if err := Cat(&out, m, b, []byte("d/f")); err != nil || out.String() != "hello\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	for p, why := range map[string]string{
		"d":                     "its type is dir",
		".lever/bootstrap.json": "broker ticket",
		"u":                     "not captured (unreadable)",
		"s":                     "not captured (skipped (read))",
		"l":                     "its type is link",
	} {
		out.Reset()
		err := Cat(&out, m, b, []byte(p))
		if !errors.Is(err, ErrNoContent) || !strings.Contains(err.Error(), why) || out.Len() != 0 {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := Cat(&out, m, b, []byte("nope")); !errors.Is(err, ErrNoEntry) {
		t.Errorf("missing: %v", err)
	}
	if err := Cat(&out, m, b, []byte("lost")); err == nil || out.Len() != 0 {
		t.Errorf("missing blob: %v", err)
	}
}

func TestCatHashMismatchSaysUntrusted(t *testing.T) {
	b := memBlobs{}
	e := fileE(b, "f", "good")
	b[e.SHA256] = []byte("evil")
	var out bytes.Buffer
	err := Cat(&out, memManifest(hdr("s"), []Entry{e}), b, []byte("f"))
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), "not trustworthy") {
		t.Fatalf("err %v", err)
	}
}

func TestCatInvalidManifestWritesNothing(t *testing.T) {
	b := memBlobs{}
	bad := badManifest(hdr("s"), []Entry{fileE(b, "f", "content")})
	var out bytes.Buffer
	if err := Cat(&out, bad, b, []byte("f")); !errors.Is(err, ErrInvalid) || out.Len() != 0 {
		t.Fatalf("%v %q", err, out.String())
	}
}
