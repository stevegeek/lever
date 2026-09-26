package loginfwd

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path"
	"testing"
	"testing/fstest"

	lexec "github.com/stevegeek/lever/internal/proc"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// prebuiltTree is a PrebuiltDir as genprebuilt leaves it, with digest as the
// recorded source digest and one binary per arch in bins.
func prebuiltTree(t *testing.T, digest string, bins map[string][]byte) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{path.Join(PrebuiltDir, ".gitkeep"): {}}
	if digest != "" {
		fsys[path.Join(PrebuiltDir, DigestFile)] = &fstest.MapFile{Data: []byte(digest + "\n")}
	}
	for arch, b := range bins {
		fsys[path.Join(PrebuiltDir, PrebuiltName(arch))] = &fstest.MapFile{Data: gz(t, b)}
	}
	return fsys
}

// withEmbedded swaps the tree Prebuilt reads for the duration of a test.
func withEmbedded(t *testing.T, fsys fstest.MapFS) {
	t.Helper()
	old := embedded
	embedded = fsys
	t.Cleanup(func() { embedded = old })
}

func TestPrebuiltSelection(t *testing.T) {
	both := map[string][]byte{"amd64": []byte("AMD"), "arm64": []byte("ARM")}
	cases := []struct {
		name     string
		fsys     fstest.MapFS
		arch     string
		want     string
		complete bool
	}{
		{"a plain checkout carries only the placeholder", prebuiltTree(t, "", nil), "arm64", "", false},
		{"current digest, both arches", prebuiltTree(t, SourceDigest(), both), "arm64", "ARM", true},
		{"current digest, amd64", prebuiltTree(t, SourceDigest(), both), "amd64", "AMD", true},
		// Built from other source (main.go changed since): stale, never used.
		{"stale digest", prebuiltTree(t, "0123abcd", both), "arm64", "", false},
		// An interrupted generation writes no digest.
		{"binaries without a digest", prebuiltTree(t, "", both), "arm64", "", false},
		{"one arch missing", prebuiltTree(t, SourceDigest(), map[string][]byte{"amd64": []byte("AMD")}), "arm64", "", false},
		{"unknown arch", prebuiltTree(t, SourceDigest(), both), "riscv64", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withEmbedded(t, c.fsys)
			got, ok := Prebuilt(c.arch)
			if ok != (c.want != "") || string(got) != c.want {
				t.Fatalf("Prebuilt(%s) = %q, %v; want %q", c.arch, got, ok, c.want)
			}
			if PrebuiltComplete() != c.complete {
				t.Fatalf("PrebuiltComplete() = %v, want %v", !c.complete, c.complete)
			}
		})
	}
}

// A corrupt archive is "not available", not a failure: the fallback build is
// always correct, a half-read binary never is.
func TestPrebuiltRejectsACorruptArchive(t *testing.T) {
	fsys := prebuiltTree(t, SourceDigest(), nil)
	fsys[path.Join(PrebuiltDir, PrebuiltName("arm64"))] = &fstest.MapFile{Data: []byte("not gzip")}
	withEmbedded(t, fsys)
	if _, ok := Prebuilt("arm64"); ok {
		t.Fatal("a corrupt archive must not be used")
	}
}

// The digest must change with each input it claims to cover, or a stale
// prebuilt would pass for a current one.
func TestSourceDigestCoversSourceModAndFlags(t *testing.T) {
	base := SourceDigest()
	for name, mutate := range map[string]func() func(){
		"source": func() func() { old := source; source += "\n// x"; return func() { source = old } },
		"flags": func() func() {
			old := buildFlags
			buildFlags = []string{"-trimpath"}
			return func() { buildFlags = old }
		},
	} {
		restore := mutate()
		if SourceDigest() == base {
			t.Errorf("changing the %s did not change the digest", name)
		}
		restore()
	}
	if SourceDigest() != base {
		t.Fatal("digest is not deterministic")
	}
}

// With a current prebuilt, Forwarder stages it and never asks for Go — the
// whole point for a build-free scion.binary host.
func TestForwarderUsesThePrebuiltWithoutGo(t *testing.T) {
	isolateCache(t)
	withEmbedded(t, prebuiltTree(t, SourceDigest(), map[string][]byte{"amd64": []byte("AMD"), "arm64": []byte("ARM")}))
	f := fakeRunner()
	out, src, err := Forwarder(context.Background(), f, "arm64", "m")
	if err != nil {
		t.Fatalf("Forwarder: %v", err)
	}
	if src != SourcePrebuilt {
		t.Fatalf("source = %s, want prebuilt", src)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("a prebuilt forwarder must not run anything on the host, ran %+v", f.Calls)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != "ARM" {
		t.Fatalf("staged %q, err=%v", b, err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("staged binary mode: %v, err=%v, want 0700", fi, err)
	}
}

// Without one, Forwarder falls back to the host build — the dev and
// `go install` path, unchanged from before.
func TestForwarderFallsBackToABuild(t *testing.T) {
	isolateCache(t)
	withEmbedded(t, prebuiltTree(t, "stale", map[string][]byte{"arm64": []byte("ARM")}))
	f := fakeRunner()
	f.Script("go env GOROOT", fakeResult("/opt/go\n"))
	f.Script("/opt/go/bin/go build", fakeResult(""))
	_, src, err := Forwarder(context.Background(), f, "arm64", "m")
	if err != nil {
		t.Fatalf("Forwarder: %v", err)
	}
	if src != SourceBuilt || !f.Called(lexec.Subcommand("/opt/go/bin/go", "build")) {
		t.Fatalf("source = %s; want a host build", src)
	}
}

// TestPrebuiltLoginForwarderEmbedded is the release gate: the goreleaser
// before-hook runs it with LEVER_REQUIRE_PREBUILT_LOGINFWD=1 right after
// genprebuilt, so a release can never ship without the embed (or with a stale
// one) and quietly need Go on the host again. Skipped everywhere else — a
// plain checkout legitimately carries only the placeholder.
func TestPrebuiltLoginForwarderEmbedded(t *testing.T) {
	if os.Getenv("LEVER_REQUIRE_PREBUILT_LOGINFWD") != "1" {
		t.Skip("set LEVER_REQUIRE_PREBUILT_LOGINFWD=1 after running genprebuilt")
	}
	for _, arch := range GuestArches {
		bin, ok := Prebuilt(arch)
		if !ok {
			t.Fatalf("no current prebuilt forwarder for linux/%s is embedded — run genprebuilt first", arch)
		}
		if !bytes.HasPrefix(bin, []byte("\x7fELF")) {
			t.Fatalf("the embedded linux/%s forwarder is not an ELF binary", arch)
		}
	}
}
