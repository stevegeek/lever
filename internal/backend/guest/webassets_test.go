package guest

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
)

// isolateCacheDir points os.UserCacheDir at a temp dir for the duration of a
// test, so a build/cache test never reads or writes the developer's real
// ~/Library/Caches (darwin) or ~/.cache (linux).
func isolateCacheDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	root, err := WebBuildCacheRoot()
	if err != nil {
		t.Fatalf("WebBuildCacheRoot: %v", err)
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
	write(t, filepath.Join(buildDir, webBuildMarker), digest)
	return buildDir
}

func TestBuildsWebAssets(t *testing.T) {
	cases := []struct {
		name string
		spec ScionSpec
		want bool
	}{
		{"version + web ui", ScionSpec{Version: "e82a2a08", WebUI: true}, true},
		{"source + web ui", ScionSpec{Source: "/src/scion", WebUI: true}, true},
		{"version without web ui", ScionSpec{Version: "e82a2a08"}, false},
		// A prebuilt binary carries no source to build from, and may already
		// embed its own assets — skip, never fail.
		{"binary + web ui", ScionSpec{Binary: "/bin/scion", WebUI: true}, false},
		{"nothing", ScionSpec{WebUI: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.BuildsWebAssets(); got != c.want {
				t.Fatalf("BuildsWebAssets()=%v want %v", got, c.want)
			}
		})
	}
}

// A binary-mode spec must not touch the host or the guest at all: scion's own
// embedded assets (if any) stay in charge.
func TestEnsureScionWebAssetsSkipsBinaryMode(t *testing.T) {
	f := exec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Binary: "/bin/scion", WebUI: true}); err != nil {
		t.Fatalf("EnsureScionWebAssets: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("binary mode ran %d command(s), want none: %+v", len(f.Calls), f.Calls)
	}
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
	before, err := hashWebSource(web)
	if err != nil {
		t.Fatalf("hashWebSource: %v", err)
	}
	write(t, filepath.Join(web, "node_modules", "lit", "index.js"), "x")
	write(t, filepath.Join(web, "dist", "client", "assets", "main.js"), "x")
	write(t, filepath.Join(web, "public", "assets", "main.js"), "x")
	write(t, filepath.Join(web, "public", "shoelace", "assets", "icons", "gear.svg"), "<svg/>")
	after, err := hashWebSource(web)
	if err != nil {
		t.Fatalf("hashWebSource: %v", err)
	}
	if before != after {
		t.Fatal("build output changed the source digest; the build cache would never hit")
	}

	write(t, filepath.Join(web, "src", "main.ts"), "export const x = 2;")
	edited, err := hashWebSource(web)
	if err != nil {
		t.Fatalf("hashWebSource: %v", err)
	}
	if edited == before {
		t.Fatal("editing a source file did not change the digest; a source: checkout would serve a stale UI")
	}
}

// A rename is a change even when the bytes are identical, because the digest
// covers paths as well as contents.
func TestHashWebSourceCoversPaths(t *testing.T) {
	web := filepath.Join(fakeScionSource(t), "web")
	before, err := hashWebSource(web)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(web, "src", "main.ts"), filepath.Join(web, "src", "index.ts")); err != nil {
		t.Fatal(err)
	}
	after, err := hashWebSource(web)
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
	write(t, filepath.Join(dir, webBuildMarker), "d")
	if webBuildComplete(dir) {
		t.Fatal("a marker alone is not a complete build — dist/ may have been cleared")
	}
	write(t, filepath.Join(dir, "dist", "client", "assets", "main.js"), "x")
	if !webBuildComplete(dir) {
		t.Fatal("marker + main.js is a complete build")
	}
	if err := os.Remove(filepath.Join(dir, webBuildMarker)); err != nil {
		t.Fatal(err)
	}
	if webBuildComplete(dir) {
		t.Fatal("assets without a marker are an interrupted build, not a complete one")
	}
}

// The whole point of keying the build directory on the sources: a second apply
// on an unchanged pin must not run npm again.
func TestEnsureScionWebAssetsReusesCachedBuild(t *testing.T) {
	for _, shape := range prefixShapes("m") {
		t.Run(shape.name, func(t *testing.T) {
			cacheRoot := isolateCacheDir(t)
			src := fakeScionSource(t)
			digest, err := hashWebSource(filepath.Join(src, "web"))
			if err != nil {
				t.Fatal(err)
			}
			stageCompletedBuild(t, cacheRoot, digest)

			f := exec.NewFakeRunner()
			// The guest holds nothing yet, so staging must run.
			f.Script(strings.Join(shape.userPrefix, " ")+" /bin/bash -c", exec.Result{})
			f.Script(strings.Join(shape.rootPrefix, " ")+" bash -c", exec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "m"}

			if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Source: src, WebUI: true}); err != nil {
				t.Fatalf("EnsureScionWebAssets: %v", err)
			}

			var staged bool
			for _, c := range f.Calls {
				if c.Name == "npm" {
					t.Fatalf("npm ran despite a complete cached build: %v", c.Args)
				}
				// The archive travels on stdin of `<rootPrefix> bash -c <script>`.
				if c.Stdin != "" {
					staged = true
					script := c.Args[len(c.Args)-1]
					if !strings.Contains(script, "tar -xf -") || !strings.Contains(script, digest) {
						t.Fatalf("the staging script does not extract stdin and record the digest it installed: %s", script)
					}
					if c.Name != shape.rootPrefix[0] {
						t.Fatalf("staging must run through the root prefix, got %s %v", c.Name, c.Args)
					}
					names := tarNames(t, c.Stdin)
					if !slices.Contains(names, "assets/main.js") {
						t.Fatalf("archive does not carry the sentinel asset: %v", names)
					}
				}
			}
			if !staged {
				t.Fatal("assets were never streamed into the guest")
			}
		})
	}
}

// The second skip level: the guest already holds this exact tree.
func TestEnsureScionWebAssetsSkipsStagingWhenGuestMatches(t *testing.T) {
	cacheRoot := isolateCacheDir(t)
	src := fakeScionSource(t)
	digest, err := hashWebSource(filepath.Join(src, "web"))
	if err != nil {
		t.Fatal(err)
	}
	stageCompletedBuild(t, cacheRoot, digest)

	f := exec.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", exec.Result{Stdout: digest + "\n"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}

	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Source: src, WebUI: true}); err != nil {
		t.Fatalf("EnsureScionWebAssets: %v", err)
	}
	for _, c := range f.Calls {
		if c.Stdin != "" {
			t.Fatalf("re-staged 12MB of assets the guest already holds: %v", c.Args)
		}
	}
}

// A guest whose digest matches but whose assets are gone must re-stage: this is
// the blind spot a bare marker would have.
func TestStagedWebDigestRequiresTheAssetItAttests(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", exec.Result{Stdout: "abc123\n"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}
	if got := g.stagedWebDigest(context.Background()); got != "abc123" {
		t.Fatalf("stagedWebDigest=%q want abc123", got)
	}
	script := f.Calls[0].Args[len(f.Calls[0].Args)-1]
	if !strings.Contains(script, "test -f") || !strings.Contains(script, webAssetsSentinel) {
		t.Fatalf("the digest probe must also prove the served asset exists; got %q", script)
	}
	// This one runs as the RUN USER, whose PATH has run-user-writable
	// directories ahead of /usr/bin: a bare `bash` there is a shim away from
	// pinning the staged UI stale forever, with no guest root needed.
	if !slices.Contains(f.Calls[0].Args, "/bin/bash") {
		t.Fatalf("the run-user probe must invoke an absolute shell path; got %v", f.Calls[0].Args)
	}

	// An unreadable guest reports "" so the caller re-stages, rather than
	// trusting a probe that did not answer.
	empty := exec.NewFakeRunner()
	ge := Guest{Host: empty, UserPrefix: []string{"orb", "-m", "m"}}
	if got := ge.stagedWebDigest(context.Background()); got != "" {
		t.Fatalf("a failed probe must read as %q, got %q", "", got)
	}
}

// tarNames lists the entry names of a tar archive held in memory.
func tarNames(t *testing.T, archive string) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(strings.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names = append(names, h.Name)
	}
}

func TestStageWebAssetsScript(t *testing.T) {
	got := stageWebAssetsScript("deadbeef")
	// The archive arrives on stdin; extraction runs as guest root and GNU tar
	// would otherwise restore the archive's ownership.
	if !strings.Contains(got, "tar -xf - --no-same-owner") {
		t.Errorf("missing stdin extract with --no-same-owner: %s", got)
	}
	// Extract-then-swap, so a failure leaves the destination absent rather
	// than half-written.
	tmp := ScionWebAssetsDir + ".tmp"
	for _, want := range []string{
		"-C '" + tmp + "'",
		"mv '" + tmp + "' '" + ScionWebAssetsDir + "'",
		"printf %s 'deadbeef' > '" + filepath.Join(tmp, webDigestFile) + "'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
}

// The digest is interpolated into the guest script, so it is quoted: a value
// with a single quote must not break out of the quoting.
func TestStageWebAssetsScriptQuotesTheDigest(t *testing.T) {
	got := stageWebAssetsScript("'; touch /tmp/pwned; '")
	if !strings.Contains(got, `'\''`) {
		t.Fatalf("digest was not shell-quoted: %s", got)
	}
}

// writeWebAssetsTar produces the archive lever streams into the guest: every
// file under dist with its relative path, sourcemaps dropped, and no host
// artefacts (AppleDouble members, host uids).
func TestWriteWebAssetsTar(t *testing.T) {
	dist := t.TempDir()
	write(t, filepath.Join(dist, "index.html"), "<html>")
	write(t, filepath.Join(dist, "assets", "main.js"), "console.log(1)")
	write(t, filepath.Join(dist, "assets", "main.js.map"), "{}")
	write(t, filepath.Join(dist, "assets", "deep", "x.css"), "a{}")

	var buf bytes.Buffer
	if err := writeWebAssetsTar(&buf, dist); err != nil {
		t.Fatalf("writeWebAssetsTar: %v", err)
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
	g := Guest{Host: f, Machine: "m"}

	_, _, err := g.buildWebAssets(context.Background(), filepath.Join(src, "web"))
	if err == nil {
		t.Fatal("a build producing no main.js must be an error")
	}
	if !strings.Contains(err.Error(), webAssetsSentinel) {
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
	g := Guest{Host: f, Machine: "m"}
	_, _, _ = g.buildWebAssets(context.Background(), filepath.Join(src, "web"))

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
	g := Guest{Host: f, Machine: "m"}

	_, _, err := g.buildWebAssets(context.Background(), filepath.Join(src, "web"))
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
