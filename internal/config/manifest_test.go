package config

import (
	"path/filepath"
	"testing"
)

// MountRoot (the in-jail bind-mount path, e.g. /lever) must survive a
// write/load round-trip: the in-jail manager reads it to translate a grove's
// tree-relative dir into the jail-absolute path scion needs for `-g` and
// `--workspace`. It is the jail mount point, not a host path, so it is safe in
// the sanitized manifest.
func TestManifestMountRootRoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		MountRoot: "/lever",
		Groves:    []ManifestGrove{{Name: "worker", Image: "img:1"}},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if got.MountRoot != "/lever" {
		t.Fatalf("MountRoot = %q, want /lever", got.MountRoot)
	}
	if img, ok := got.ImageFor("worker"); !ok || img != "img:1" {
		t.Fatalf("ImageFor(worker) = %q,%v", img, ok)
	}
}
