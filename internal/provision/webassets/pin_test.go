package webassets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion/layout"
)

func isolateCache(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	root, err := CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func completeBuild(t *testing.T, root, digest string) {
	t.Helper()
	asset := filepath.Join(root, digest, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel))
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, digest, BuildMarker), []byte(digest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedBuild(t *testing.T) {
	root := isolateCache(t)
	digest := strings.Repeat("ab", 32)
	completeBuild(t, root, digest)

	if _, _, ok := PinnedBuild("65531d7f"); ok {
		t.Fatal("a pin with no record was found")
	}
	if err := RecordPinnedBuild("65531d7f", digest); err != nil {
		t.Fatal(err)
	}
	dist, got, ok := PinnedBuild("65531d7f")
	if !ok || got != digest || dist != filepath.Join(root, digest, "dist", "client") {
		t.Fatalf("PinnedBuild = %q %q %v", dist, got, ok)
	}
	// Another pin, and a pin that differs only by length, are not it.
	if _, _, ok := PinnedBuild("65531d7"); ok {
		t.Fatal("a different pin matched")
	}

	// A version that can move is never recorded nor found.
	for _, v := range []string{"main", "latest", "../x", ".pin-x", "v1.2"} {
		if err := RecordPinnedBuild(v, digest); err != nil {
			t.Fatalf("RecordPinnedBuild(%q): %v", v, err)
		}
		if _, _, ok := PinnedBuild(v); ok {
			t.Fatalf("a movable version %q was found", v)
		}
	}
	ents, err := os.ReadDir(filepath.Join(root, pinsDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "65531d7f" {
		t.Fatalf("pins dir holds %v, want only the one record", ents)
	}

	// A digest that is not one is refused, and a corrupt record reads as none.
	if err := RecordPinnedBuild("v1.2.3", "../../etc"); err == nil {
		t.Fatal("a non-digest was recorded")
	}
	if err := os.WriteFile(filepath.Join(root, pinsDir, "v1.2.3"), []byte("../"+digest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := PinnedBuild("v1.2.3"); ok {
		t.Fatal("a corrupt record was trusted")
	}

	// An incomplete build no longer counts.
	if err := os.Remove(filepath.Join(root, digest, BuildMarker)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := PinnedBuild("65531d7f"); ok {
		t.Fatal("an incomplete build was found")
	}
}
