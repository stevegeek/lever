package treebackup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func lsManifest() OpenManifest {
	return memManifest(hdr("s"), []Entry{
		dirE("d"), fileE(nil, "d/f", "hello"), dirE("d/sub"), fileE(nil, "d/sub/g", "x"),
		linkE("d/l", "../x\xff"),
		with(fileE(nil, "d/big", ""), func(e *Entry) { e.State, e.SHA256, e.Verified, e.Size = StateTooBig, "", 0, 1<<40 }),
		fileE(nil, "top", "t"),
		{Path: []byte("x\xff"), Type: TypeUnknown, State: StateUnreadable},
	})
}

func lsLines(t *testing.T, path string, recursive bool) []string {
	t.Helper()
	var out bytes.Buffer
	var p []byte
	if path != "" {
		p = []byte(path)
	}
	if err := Ls(&out, lsManifest(), p, recursive); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		f := strings.Fields(l)
		if f[0] == "link" {
			paths = append(paths, f[len(f)-3])
		} else {
			paths = append(paths, f[len(f)-1])
		}
	}
	return paths
}

func TestLs(t *testing.T) {
	if got := strings.Join(lsLines(t, "", false), " "); got != `d top "x\xff"` {
		t.Errorf("root: %s", got)
	}
	if got := strings.Join(lsLines(t, "d", false), " "); got != "d/big d/f d/l d/sub" {
		t.Errorf("d: %s", got)
	}
	if got := strings.Join(lsLines(t, "d", true), " "); got != "d/big d/f d/l d/sub d/sub/g" {
		t.Errorf("d -r: %s", got)
	}
	if got := strings.Join(lsLines(t, "d/f", false), " "); got != "d/f" {
		t.Errorf("d/f: %s", got)
	}

	var out bytes.Buffer
	if err := Ls(&out, lsManifest(), []byte("d"), false); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"file    0644            5 " + shortHash(sumOf([]byte("hello"))) + " -            d/f",
		`link    0777            - -             -            d/l -> "../x\xff"`,
		"file    0644 1099511627776 -             too-big      d/big",
	} {
		if !strings.Contains(out.String(), w+"\n") {
			t.Errorf("missing %q in\n%s", w, out.String())
		}
	}
	out.Reset()
	if err := Ls(&out, lsManifest(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown ----            - -             unreadable   \"x\\xff\"\n") {
		t.Errorf("unknown entry:\n%s", out.String())
	}
}

func TestLsMissingAndInvalid(t *testing.T) {
	var out bytes.Buffer
	if err := Ls(&out, lsManifest(), []byte("nope"), false); !errors.Is(err, ErrNoEntry) {
		t.Fatalf("missing: %v", err)
	}
	bad := badManifest(hdr("s"), []Entry{dirE("d"), fileE(nil, "d/f", "x")})
	if err := Ls(&out, bad, nil, true); !errors.Is(err, ErrInvalid) || out.Len() != 0 {
		t.Fatalf("invalid: %v %q", err, out.String())
	}
}
