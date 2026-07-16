package jail

import (
	"reflect"
	"testing"
)

func TestLoadImageArgs(t *testing.T) {
	got := LoadImageArgs(OrbPrefix("lever-demo", "leveruser"), "501")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "load",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadImageArgs:\n got  %v\n want %v", got, want)
	}
}

func TestImageInspectArgs(t *testing.T) {
	got := ImageInspectArgs(OrbPrefix("lever-demo", "leveruser"), "501", "scionlocal/lever-claude:latest")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "image", "inspect", "--format", "{{.Id}}", "scionlocal/lever-claude:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ImageInspectArgs:\n got  %v\n want %v", got, want)
	}
}

// TestNormalizeImageID pins the docker-vs-podman prefix reconciliation: docker
// prints the ID as "sha256:<hex>", (some) podman versions print bare "<hex>".
// Without stripping the prefix on both sides, the host-vs-jail comparison in
// ImageLoaded would never match and the guard would never skip a redundant load.
func TestNormalizeImageID(t *testing.T) {
	hex := "eb84fdc6f2a3a064445bb2a2fbc89c515666c428d6c96b6ab68a4cd218819688"
	for _, in := range []string{
		"sha256:" + hex,          // docker form
		hex,                      // bare podman form
		"  sha256:" + hex + "\n", // with surrounding whitespace (command output)
		hex + "\n",
	} {
		if got := normalizeImageID(in); got != hex {
			t.Errorf("normalizeImageID(%q) = %q, want %q", in, got, hex)
		}
	}
	if got := normalizeImageID(""); got != "" {
		t.Errorf("normalizeImageID(\"\") = %q, want empty", got)
	}
}

func TestPruneImagesArgs(t *testing.T) {
	got := PruneImagesArgs(OrbPrefix("lever-demo", "leveruser"), "501")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "image", "prune", "-f",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PruneImagesArgs:\n got  %v\n want %v", got, want)
	}
}
