package treebackup

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func logSnaps() []LogSnapshot {
	h1, h2, h3 := hdr("s1"), hdr("s2"), hdr("s3")
	h1.Trigger, h1.Quiescent = TriggerStop, QuiescentVMOff
	h3.Complete = false
	h3.Partial = []Partial{{State: StateTruncated, Reason: ReasonEntries, At: []byte("docs"), HasAt: true, Count: 2}}
	h3.Flood = []Flood{{At: []byte("docs"), HasAt: true, Count: 100000}}
	h3.Counts.UnreadableNew = 1
	s1 := []Entry{dirE("docs"), fileE(nil, "docs/a.md", "v1"), fileE(nil, "bin", "b")}
	s2 := []Entry{dirE("docs"), fileE(nil, "docs/a.md", "v2"),
		with(fileE(nil, "bin", "b"), func(e *Entry) { e.Mode = 0o4755 }),
		with(fileE(nil, "hard", "h"), func(e *Entry) { e.Nlink = 2 })}
	s3 := []Entry{with(dirE("docs"), func(e *Entry) { e.State, e.Reason = StateTruncated, ReasonEntries }),
		with(fileE(nil, "bin", "b"), func(e *Entry) { e.Mode = 0o4755 }),
		with(fileE(nil, "hard", "h"), func(e *Entry) { e.Nlink = 2 })}
	return []LogSnapshot{
		{ID: "s1", Open: memManifest(h1, s1)},
		{ID: "s2", Open: memManifest(h2, s2)},
		{ID: "s3", Open: memManifest(h3, s3)},
	}
}

func TestLogWithoutPath(t *testing.T) {
	var out bytes.Buffer
	if err := Log(&out, logSnaps(), LogOptions{}); err != nil {
		t.Fatal(err)
	}
	want := `s1  stop vm-off  complete  oldest retained: A 3, D 0, M 0, T 0, L 0, P 0, ? 0
    A bin  (file, size 1, sha256 ` + shortHash(sumOf([]byte("b"))) + `)
    A docs  (dir, 1 entries below)
s2  manual none  complete  vs s1: A 1, D 0, M 1, T 0, L 0, P 1, ? 0
    note: setuid/setgid appeared on 1: bin (4755)
    note: nlink > 1 appeared on 1: hard
    P bin  (mode 0644 → 4755)
    M docs/a.md  (size 2 → 2, sha256 ` + shortHash(sumOf([]byte("v1"))) + ` → ` + shortHash(sumOf([]byte("v2"))) + `)
    A hard  (file, size 1, sha256 ` + shortHash(sumOf([]byte("h"))) + `)
s3  manual none  INCOMPLETE (truncated entries at docs (2); flood docs (100000))  vs s2: A 0, D 0, M 0, T 0, L 0, P 0, ? 2  unreadable new 1
    ? docs  (listing not captured: state ok → truncated (entries))
    ? docs/a.md  (not seen in new: docs truncated (entries))
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	if err := Log(&out, logSnaps(), LogOptions{N: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "s3 ") || strings.Contains(out.String(), "s1 ") || !strings.Contains(out.String(), "vs s2") {
		t.Fatalf("N=1:\n%s", out.String())
	}
}

func TestLogChangedPathsCap(t *testing.T) {
	var es []Entry
	for i := range logChangedPaths + 5 {
		es = append(es, fileE(nil, fmt.Sprintf("f%02d", i), "x"))
	}
	snaps := []LogSnapshot{{ID: "s1", Open: memManifest(hdr("s1"), es)}}
	var out bytes.Buffer
	if err := Log(&out, snaps, LogOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "    … 5 more changed paths (--all lists them)\n") {
		t.Fatalf("no cap:\n%s", out.String())
	}
	out.Reset()
	if err := Log(&out, snaps, LogOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n    A f") != logChangedPaths+5 {
		t.Fatalf("All:\n%s", out.String())
	}
}

func TestLogWithPath(t *testing.T) {
	var out bytes.Buffer
	if err := Log(&out, logSnaps(), LogOptions{Path: []byte("docs/a.md"), HasPath: true}); err != nil {
		t.Fatal(err)
	}
	want := `s1  A  file 2 ` + shortHash(sumOf([]byte("v1"))) + ` ok  (file, size 2, sha256 ` + shortHash(sumOf([]byte("v1"))) + `, under docs (A))
s2  M  file 2 ` + shortHash(sumOf([]byte("v2"))) + ` ok  (size 2 → 2, sha256 ` + shortHash(sumOf([]byte("v1"))) + ` → ` + shortHash(sumOf([]byte("v2"))) + `)
s3  ?  absent  (not seen in new: docs truncated (entries))
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	if err := Log(&out, logSnaps(), LogOptions{Path: []byte("docs/a.md"), HasPath: true, N: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "s3 ") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("N=1:\n%s", out.String())
	}
}

func TestLogSkipsInvalidManifest(t *testing.T) {
	snaps := logSnaps()
	snaps[1].Open = badManifest(hdr("s2"), []Entry{fileE(nil, "evil\x1b[2J", "x")})
	var out bytes.Buffer
	err := Log(&out, snaps, LogOptions{})
	if err == nil || !strings.Contains(err.Error(), "1 snapshot manifests are invalid") {
		t.Fatalf("err %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "s2  INVALID manifest, skipped: invalid tree backup manifest: duplicate key\n") ||
		!strings.Contains(s, "s3  manual none  INCOMPLETE") || !strings.Contains(s, "vs s1:") || strings.Contains(s, "evil") {
		t.Fatalf("output:\n%s", s)
	}
}

func TestLogEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := Log(&out, nil, LogOptions{}); err != nil || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
}
