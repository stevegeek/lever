package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/loginfwd"
)

// TestGenerateWritesWhatLoginfwdEmbeds runs the real generator into a temp
// directory (offline: GOPROXY=off, stdlib-only program) and checks that the
// result is exactly the shape loginfwd reads: one archive per guest arch and
// the current source digest.
func TestGenerateWritesWhatLoginfwdEmbeds(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles twice")
	}
	root := t.TempDir()
	out := filepath.Join(root, loginfwd.PrebuiltDir)
	// A stale digest from an earlier run must not survive a failed or
	// completed generation as anything but the fresh value.
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, loginfwd.DigestFile), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(context.Background(), proc.RealRunner{}, out); err != nil {
		t.Fatalf("generate: %v", err)
	}
	digest, err := os.ReadFile(filepath.Join(out, loginfwd.DigestFile))
	if err != nil || string(digest) != loginfwd.SourceDigest()+"\n" {
		t.Fatalf("digest = %q, err=%v", digest, err)
	}
	for _, arch := range loginfwd.GuestArches {
		if fi, err := os.Stat(filepath.Join(out, loginfwd.PrebuiltName(arch))); err != nil || fi.Size() == 0 {
			t.Fatalf("linux/%s archive: %v, err=%v", arch, fi, err)
		}
	}
}
