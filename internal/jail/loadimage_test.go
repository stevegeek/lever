package jail

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestLoadImageArgs(t *testing.T) {
	got := LoadImageArgs(orbPrefix("lever-demo", "leveruser"), "501")
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
	got := ImageInspectArgs(orbPrefix("lever-demo", "leveruser"), "501", "scionlocal/lever-claude:latest")
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

const (
	testImageID = "eb84fdc6f2a3a064445bb2a2fbc89c515666c428d6c96b6ab68a4cd218819688"
	testImgRef  = "scionlocal/lever-claude:latest"
)

// imageLoadedRunner scripts the FakeRunner so hostImageID (a `docker …` call)
// and jailImageID (an `orb …` prefix call) resolve independently. A missing
// script for either binary makes that side error — the real-world "image not
// present / inspect exits non-zero" case — which the readers map to "".
func imageLoadedRunner(t *testing.T, hostOut, jailOut string) *exec.FakeRunner {
	t.Helper()
	r := exec.NewFakeRunner()
	if hostOut != "" {
		r.Script("docker", exec.Result{Stdout: hostOut})
	}
	if jailOut != "" {
		r.Script("orb", exec.Result{Stdout: jailOut})
	}
	return r
}

// TestImageLoaded exercises the fail-open host-vs-jail comparison offline
// through the exec.Runner seam — previously unreachable because the readers
// shelled out to os/exec directly.
func TestImageLoaded(t *testing.T) {
	prefix := orbPrefix("lever-demo", "leveruser")
	cases := []struct {
		name             string
		hostOut, jailOut string
		want             bool
	}{
		// docker inspect errors (unscripted) -> host ID "" -> fail-open false,
		// and the jail is never consulted.
		{"host-missing", "", testImageID, false},
		// host resolves but the jail inspect errors (image not loaded) -> false.
		{"jail-missing", testImageID, "", false},
		// identical IDs on both sides -> the redundant load can be skipped.
		{"matching", testImageID, testImageID, true},
		// docker prints "sha256:<hex>", podman prints bare "<hex>": normalizeImageID
		// must reconcile BOTH sides or this guard would never fire.
		{"sha256-prefix-reconciled", "sha256:" + testImageID + "\n", testImageID + "\n", true},
		// a rebuilt tag with a genuinely different jail ID -> fail-open false.
		{"genuine-mismatch", testImageID, strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := imageLoadedRunner(t, tc.hostOut, tc.jailOut)
			if got := ImageLoaded(context.Background(), r, prefix, "501", testImgRef); got != tc.want {
				t.Fatalf("ImageLoaded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestImageLoadedUsesHostSeam pins that the check runs on the host runner: the
// docker inspect goes out as a plain `docker …` call (NOT wrapped in the jail
// prefix), and the jail inspect goes through the prefix binary.
func TestImageLoadedUsesHostSeam(t *testing.T) {
	r := imageLoadedRunner(t, testImageID, testImageID)
	ImageLoaded(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501", testImgRef)
	if len(r.Calls) != 2 {
		t.Fatalf("want 2 host calls (docker inspect + orb inspect), got %d", len(r.Calls))
	}
	if r.Calls[0].Name != "docker" {
		t.Errorf("host inspect must invoke docker directly, got %q", r.Calls[0].Name)
	}
	if r.Calls[1].Name != "orb" {
		t.Errorf("jail inspect must go through the prefix binary, got %q", r.Calls[1].Name)
	}
}

// TestPruneImagesErrorPropagates: a failing prune surfaces a wrapped error (the
// only call site logs it non-fatally), rather than being swallowed.
func TestPruneImagesErrorPropagates(t *testing.T) {
	r := exec.NewFakeRunner() // no script for the prefix binary -> Run errors
	err := PruneImages(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501")
	if err == nil {
		t.Fatal("PruneImages must propagate the runner error")
	}
	if !strings.Contains(err.Error(), "prune images") {
		t.Errorf("error missing context prefix: %v", err)
	}
}

// TestPruneImagesSuccess: a clean prune returns nil and drives the prune argv
// through the host runner.
func TestPruneImagesSuccess(t *testing.T) {
	r := exec.NewFakeRunner()
	r.Script("orb", exec.Result{})
	if err := PruneImages(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501"); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "orb" {
		t.Fatalf("expected one orb-prefixed prune call, got %+v", r.Calls)
	}
	if got := strings.Join(r.Calls[0].Args, " "); !strings.Contains(got, "podman image prune -f") {
		t.Errorf("prune argv missing `podman image prune -f`: %q", got)
	}
}

func TestPruneImagesArgs(t *testing.T) {
	got := PruneImagesArgs(orbPrefix("lever-demo", "leveruser"), "501")
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
