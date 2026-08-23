package webassets

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// isolateCacheDir points os.UserCacheDir at a temp dir for the duration of a
// test, so a build/cache test never reads or writes the developer's real
// ~/Library/Caches (darwin) or ~/.cache (linux).
func isolateCacheDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	root, err := CacheRoot()
	if err != nil {
		t.Fatalf("CacheRoot: %v", err)
	}
	return root
}

// fakeScionSource writes the minimum of scion's tree that the web build path
// looks at: a checkout root with a web/ holding package.json.
func fakeScionSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(filepath.Join(web, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "package.json"), `{"name":"@scion/web-frontend"}`)
	write(t, filepath.Join(web, "src", "main.ts"), "export const x = 1;")
	return root
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stageCompletedBuild fakes a finished npm build at the cache location the
// given source digest resolves to, so the cache-hit path can be exercised
// without running npm.
func stageCompletedBuild(t *testing.T, cacheRoot, digest string) string {
	t.Helper()
	buildDir := filepath.Join(cacheRoot, digest)
	write(t, filepath.Join(buildDir, "dist", "client", "assets", "main.js"), "console.log(1)")
	write(t, filepath.Join(buildDir, BuildMarker), digest)
	return buildDir
}

func TestNodeMajor(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"v25.9.0\n", 25, false},
		{"v20.0.0", 20, false},
		{"18.19.1", 18, false},
		{"", 0, true},
		{"No version is set for command node\n", 0, true},
	}
	for _, c := range cases {
		got, err := nodeMajor(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("nodeMajor(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("nodeMajor(%q) = %d, %v; want %d, nil", c.in, got, err, c.want)
		}
	}
}

func TestCheckNodeToolchain(t *testing.T) {
	t.Run("accepts a supported node", func(t *testing.T) {
		f := exec.NewFakeRunner()
		f.Script("node --version", exec.Result{Stdout: "v25.9.0\n"})
		f.Script("npm --version", exec.Result{Stdout: "11.12.1\n"})
		got, err := CheckNodeToolchain(context.Background(), f, "/probe")
		if err != nil {
			t.Fatalf("CheckNodeToolchain: %v", err)
		}
		if got != "v25.9.0" {
			t.Fatalf("version=%q", got)
		}
		// Both probes must run in the build's own directory — a walk-up
		// version manager answers per-directory.
		for _, c := range f.Calls {
			if c.Dir != "/probe" {
				t.Fatalf("probe %q ran in dir %q, want /probe", c.Name, c.Dir)
			}
		}
	})

	// The live shape of a dead asdf/mise shim: on PATH, resolves, exits 126.
	t.Run("rejects a broken shim", func(t *testing.T) {
		f := exec.NewFakeRunner()
		_, err := CheckNodeToolchain(context.Background(), f, "/probe")
		if err == nil {
			t.Fatal("expected an error when node is unusable")
		}
		if !strings.Contains(err.Error(), "node/npm toolchain not usable") {
			t.Fatalf("error should name the toolchain; got %v", err)
		}
	})

	t.Run("rejects node below the engines floor", func(t *testing.T) {
		f := exec.NewFakeRunner()
		f.Script("node --version", exec.Result{Stdout: "v18.19.1\n"})
		_, err := CheckNodeToolchain(context.Background(), f, "/probe")
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("want a too-old error naming the floor; got %v", err)
		}
	})

	t.Run("rejects a working node with no npm", func(t *testing.T) {
		f := exec.NewFakeRunner()
		f.Script("node --version", exec.Result{Stdout: "v25.9.0\n"})
		_, err := CheckNodeToolchain(context.Background(), f, "/probe")
		if err == nil || !strings.Contains(err.Error(), "npm --version") {
			t.Fatalf("want an npm error; got %v", err)
		}
	})
}

// The digest must key on build INPUTS only. Build output written back into the
// source tree (npm's postinstall and copy:client scripts do exactly that) would
// otherwise change the key on every build — a cache that never hits.
func TestHashWebSourceIgnoresBuildOutput(t *testing.T) {
	web := filepath.Join(fakeScionSource(t), "web")
	before, err := HashSource(web)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	write(t, filepath.Join(web, "node_modules", "lit", "index.js"), "x")
	write(t, filepath.Join(web, "dist", "client", "assets", "main.js"), "x")
	write(t, filepath.Join(web, "public", "assets", "main.js"), "x")
	write(t, filepath.Join(web, "public", "shoelace", "assets", "icons", "gear.svg"), "<svg/>")
	after, err := HashSource(web)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	if before != after {
		t.Fatal("build output changed the source digest; the build cache would never hit")
	}

	write(t, filepath.Join(web, "src", "main.ts"), "export const x = 2;")
	edited, err := HashSource(web)
	if err != nil {
		t.Fatalf("HashSource: %v", err)
	}
	if edited == before {
		t.Fatal("editing a source file did not change the digest; a source: checkout would serve a stale UI")
	}
}

// A rename is a change even when the bytes are identical, because the digest
// covers paths as well as contents.
func TestHashWebSourceCoversPaths(t *testing.T) {
	web := filepath.Join(fakeScionSource(t), "web")
	before, err := HashSource(web)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(web, "src", "main.ts"), filepath.Join(web, "src", "index.ts")); err != nil {
		t.Fatal(err)
	}
	after, err := HashSource(web)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a rename did not change the digest")
	}
}

// The Go module cache is read-only; npm must be able to write into its project
// directory, so the copy normalises modes rather than preserving them.
func TestCopyTreeNormalisesReadOnlySourceModes(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "package.json"), "{}")
	write(t, filepath.Join(src, "src", "main.ts"), "x")
	write(t, filepath.Join(src, "node_modules", "junk.js"), "x")
	for _, rel := range []string{"package.json", "src/main.ts"} {
		if err := os.Chmod(filepath.Join(src, filepath.FromSlash(rel)), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "build")
	if err := copyTree(src, dst, skipWebSourcePath); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	for _, rel := range []string{"package.json", "src/main.ts"} {
		fi, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if fi.Mode().Perm()&0o200 == 0 {
			t.Fatalf("%s copied read-only (%04o); npm could not write it", rel, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("node_modules was copied; it must be reinstalled by npm ci, not carried over")
	}
}

func TestWebBuildComplete(t *testing.T) {
	dir := t.TempDir()
	if webBuildComplete(dir) {
		t.Fatal("an empty directory is not a complete build")
	}
	write(t, filepath.Join(dir, BuildMarker), "d")
	if webBuildComplete(dir) {
		t.Fatal("a marker alone is not a complete build — dist/ may have been cleared")
	}
	write(t, filepath.Join(dir, "dist", "client", "assets", "main.js"), "x")
	if !webBuildComplete(dir) {
		t.Fatal("marker + main.js is a complete build")
	}
	if err := os.Remove(filepath.Join(dir, BuildMarker)); err != nil {
		t.Fatal(err)
	}
	if webBuildComplete(dir) {
		t.Fatal("assets without a marker are an interrupted build, not a complete one")
	}
}

// WriteTar produces the archive lever streams into the guest: every
// file under dist with its relative path, sourcemaps dropped, and no host
// artefacts (AppleDouble members, host uids).
func TestWriteWebAssetsTar(t *testing.T) {
	dist := t.TempDir()
	write(t, filepath.Join(dist, "index.html"), "<html>")
	write(t, filepath.Join(dist, "assets", "main.js"), "console.log(1)")
	write(t, filepath.Join(dist, "assets", "main.js.map"), "{}")
	write(t, filepath.Join(dist, "assets", "deep", "x.css"), "a{}")

	var buf bytes.Buffer
	if err := WriteTar(&buf, dist); err != nil {
		t.Fatalf("WriteTar: %v", err)
	}
	var got []string
	contents := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		got = append(got, h.Name)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s carries host ownership: uid=%d gid=%d %q/%q", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		if h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			contents[h.Name] = string(b)
		}
	}
	want := []string{"assets/", "assets/deep/", "assets/deep/x.css", "assets/main.js", "index.html"}
	if !slices.Equal(got, want) {
		t.Fatalf("archive entries = %v, want %v (sourcemaps excluded, paths relative)", got, want)
	}
	if contents["assets/main.js"] != "console.log(1)" {
		t.Fatalf("main.js content = %q", contents["assets/main.js"])
	}
}

// A build that exits 0 but produces nothing servable must fail at apply, not
// silently leave the browser on scion's "Web UI Not Available" page.
func TestBuildWebAssetsRejectsAnEmptyBuild(t *testing.T) {
	isolateCacheDir(t)
	src := fakeScionSource(t)
	f := exec.NewFakeRunner()
	f.Script("node --version", exec.Result{Stdout: "v25.9.0\n"})
	f.Script("npm --version", exec.Result{Stdout: "11.12.1\n"})
	f.Script("npm ci", exec.Result{})
	f.Script("npm run build", exec.Result{})

	_, _, err := Build(context.Background(), f, filepath.Join(src, "web"))
	if err == nil {
		t.Fatal("a build producing no main.js must be an error")
	}
	if !strings.Contains(err.Error(), layout.WebAssetsSentinel) {
		t.Fatalf("the error should name the missing asset; got %v", err)
	}
}

// npm ci, not npm install: it installs package-lock.json exactly and fails on a
// lock/manifest mismatch, which is what makes a pin build the same way twice.
func TestBuildWebAssetsUsesReproducibleInstall(t *testing.T) {
	isolateCacheDir(t)
	src := fakeScionSource(t)
	f := exec.NewFakeRunner()
	f.Script("node --version", exec.Result{Stdout: "v25.9.0\n"})
	f.Script("npm --version", exec.Result{Stdout: "11.12.1\n"})
	f.Script("npm ci", exec.Result{})
	f.Script("npm run build", exec.Result{})
	_, _, _ = Build(context.Background(), f, filepath.Join(src, "web"))

	var sawCI, sawBuild bool
	for _, c := range f.Calls {
		if c.Name != "npm" {
			continue
		}
		switch strings.Join(c.Args, " ") {
		case "ci --no-audit --no-fund":
			sawCI = true
		case "run build":
			sawBuild = true
		}
		if len(c.Args) > 0 && c.Args[0] == "install" {
			t.Fatal("npm install is not reproducible; npm ci is required")
		}
		if c.Dir == "" {
			t.Fatalf("npm %v ran in the process cwd, not the build dir", c.Args)
		}
	}
	if !sawCI || !sawBuild {
		t.Fatalf("npm ci=%v, npm run build=%v", sawCI, sawBuild)
	}
}

// A missing toolchain must be diagnosed BEFORE thousands of files are copied,
// and must carry the remediation rather than npm's bare exit code.
func TestBuildWebAssetsFailsEarlyWithoutNode(t *testing.T) {
	cacheRoot := isolateCacheDir(t)
	src := fakeScionSource(t)
	f := exec.NewFakeRunner()

	_, _, err := Build(context.Background(), f, filepath.Join(src, "web"))
	if err == nil {
		t.Fatal("expected a toolchain error")
	}
	if !strings.Contains(err.Error(), "node/npm toolchain not usable") || !strings.Contains(err.Error(), "asdf/mise shim") {
		t.Fatalf("error must diagnose and remediate; got %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "npm" {
			t.Fatal("npm ran despite an unusable toolchain")
		}
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("sources were copied before the toolchain was proven usable: %v", entries)
	}
}

// The whole point of keying the build directory on the sources: a second
// Build on an unchanged pin must not run npm again, and must resolve to the
// cached dist.
func TestBuildReusesCachedBuild(t *testing.T) {
	cacheRoot := isolateCacheDir(t)
	src := fakeScionSource(t)
	digest, err := HashSource(filepath.Join(src, "web"))
	if err != nil {
		t.Fatal(err)
	}
	buildDir := stageCompletedBuild(t, cacheRoot, digest)

	f := exec.NewFakeRunner()
	dist, got, err := Build(context.Background(), f, filepath.Join(src, "web"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got != digest || dist != filepath.Join(buildDir, "dist", "client") {
		t.Fatalf("Build = %q, %q", dist, got)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("a cached build ran %d command(s): %+v", len(f.Calls), f.Calls)
	}
}
