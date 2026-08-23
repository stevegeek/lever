//go:build integration

package webassets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// Run with:
//
//	go test -tags integration -run TestRealScionWebBuild ./internal/provision/webassets/ -v
//
// Requires a real go toolchain (to fetch the scion module), a real node >= 20
// with npm, and network access for `npm ci`. Touches no guest: it exercises the
// HOST half only — fetch, copy out of the read-only module cache, npm ci, vite
// build, and the per-pin cache hit.
//
// This is the test that answers "does the pinned module's web/ actually build?",
// which no amount of FakeRunner argv-pinning can. Override the pin with
// LEVER_SCION_VERSION to check a candidate before moving examples onto it.
func TestRealScionWebBuild(t *testing.T) {
	version := os.Getenv("LEVER_SCION_VERSION")
	if version == "" {
		// The pin examples/ and the docs ship today.
		version = "e82a2a08"
	}
	r := proc.RealRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	srcWeb, err := SourceDir(ctx, r, scionbin.Spec{Version: version, WebUI: true})
	if err != nil {
		t.Fatalf("SourceDir: %v", err)
	}
	t.Logf("scion %s web sources: %s", version, srcWeb)

	// The build lands in the developer's real cache — the same path production
	// uses, so PATH and any walk-up version manager behave identically here.
	// Removed afterwards so a test run does not leave a few hundred MB behind.
	digest, err := HashSource(srcWeb)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	root, err := CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	buildDir := filepath.Join(root, digest)
	if webBuildComplete(buildDir) {
		t.Skipf("cache already holds a build for %s; remove %s to re-measure a cold build", version, buildDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(buildDir) })

	start := time.Now()
	dist, gotDigest, err := Build(ctx, r, srcWeb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("cold build took %s -> %s", time.Since(start).Round(time.Second), dist)
	if gotDigest != digest {
		t.Fatalf("digest = %q, want %q", gotDigest, digest)
	}

	// scion serves the app shell from Go and loads exactly this file; without
	// it the browser gets the "Web UI Not Available" page.
	if _, err := os.Stat(filepath.Join(dist, filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		t.Fatalf("no %s in the build output: %v", layout.WebAssetsSentinel, err)
	}
	// The Go server does not serve node_modules, so the icons scion's
	// components request must have been copied into the dist tree.
	if _, err := os.Stat(filepath.Join(dist, "shoelace", "assets", "icons", "gear.svg")); err != nil {
		t.Fatalf("shoelace icons missing from the build output: %v", err)
	}

	marker, err := os.Stat(filepath.Join(buildDir, BuildMarker))
	if err != nil {
		t.Fatalf("no completion marker: %v", err)
	}

	// Keep the sizes the guide quotes honest as scion's SPA grows, and keep the
	// sourcemap exclusion earning its place: it only pays if maps stay a large
	// fraction of the build.
	var total, maps int64
	err = filepath.WalkDir(dist, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		if filepath.Ext(p) == ".map" {
			maps += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const mb = 1 << 20
	t.Logf("dist/client %.1fMB, sourcemaps %.1fMB (%.0f%%), staged payload %.1fMB",
		float64(total)/mb, float64(maps)/mb, 100*float64(maps)/float64(total), float64(total-maps)/mb)
	if maps == 0 {
		t.Error("no sourcemaps in the build output — the --exclude='*.map' staging filter is now dead weight")
	}
	if staged := total - maps; staged > 6*mb {
		t.Errorf("staged payload grew to %.1fMB; the remote-access guide quotes ~3.3MB", float64(staged)/mb)
	}

	// The cache is the whole point: a re-apply on an unchanged pin must not
	// re-run npm.
	start = time.Now()
	dist2, digest2, err := Build(ctx, r, srcWeb)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	t.Logf("cached build took %s", time.Since(start).Round(time.Millisecond))
	if dist2 != dist || digest2 != digest {
		t.Fatalf("cache hit resolved elsewhere: %q/%q", dist2, digest2)
	}
	marker2, err := os.Stat(filepath.Join(buildDir, BuildMarker))
	if err != nil {
		t.Fatal(err)
	}
	if !marker2.ModTime().Equal(marker.ModTime()) {
		t.Fatal("the second build rewrote the marker; npm ran again on an unchanged pin")
	}

	// A cache directory that lost its assets — hand-edited, or left by an
	// older lever that built in place — must be rebuilt, not adopted. This is
	// the branch where the atomic publish finds its destination occupied by
	// something incomplete and has to clear it first.
	if err := os.Remove(filepath.Join(dist, filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		t.Fatal(err)
	}
	if webBuildComplete(buildDir) {
		t.Fatal("a cache missing the asset scion serves must not read as complete")
	}
	if _, _, err := Build(ctx, r, srcWeb); err != nil {
		t.Fatalf("rebuild over an incomplete cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dist, filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		t.Fatalf("the rebuild did not restore %s: %v", layout.WebAssetsSentinel, err)
	}

	// Nothing may be left behind under the cache root but the build directory
	// itself: a staging dir that outlived its build would leak a node_modules
	// per apply.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != digest {
			t.Fatalf("stray entry left in the build cache: %s", e.Name())
		}
	}
}
