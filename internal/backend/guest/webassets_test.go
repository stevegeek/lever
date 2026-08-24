package guest

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// isolateCacheDir points os.UserCacheDir at a temp dir for the duration of a
// test, so a staging test never reads or writes the developer's real cache.
func isolateCacheDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	root, err := webassets.CacheRoot()
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
// given source digest resolves to, so staging can be exercised without npm.
func stageCompletedBuild(t *testing.T, cacheRoot, digest string) {
	t.Helper()
	buildDir := filepath.Join(cacheRoot, digest)
	write(t, filepath.Join(buildDir, "dist", "client", "assets", "main.js"), "console.log(1)")
	write(t, filepath.Join(buildDir, webassets.BuildMarker), digest)
}

// A binary-mode spec must not touch the host or the guest at all: scion's own
// embedded assets (if any) stay in charge.
func TestEnsureScionWebAssetsSkipsBinaryMode(t *testing.T) {
	f := proc.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Binary: "/bin/scion", WebUI: true}); err != nil {
		t.Fatalf("EnsureScionWebAssets: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("binary mode ran %d command(s), want none: %+v", len(f.Calls), f.Calls)
	}
}

// The whole point of keying the build directory on the sources: a second apply
// on an unchanged pin must not run npm again.
func TestEnsureScionWebAssetsReusesCachedBuild(t *testing.T) {
	for _, shape := range prefixShapes("m") {
		t.Run(shape.name, func(t *testing.T) {
			cacheRoot := isolateCacheDir(t)
			src := fakeScionSource(t)
			digest, err := webassets.HashSource(filepath.Join(src, "web"))
			if err != nil {
				t.Fatal(err)
			}
			stageCompletedBuild(t, cacheRoot, digest)

			f := proc.NewFakeRunner()
			// The guest holds nothing yet, so staging must run.
			f.Script(strings.Join(shape.userPrefix, " ")+" /bin/bash -c", proc.Result{})
			f.Script(strings.Join(shape.rootPrefix, " ")+" bash -c", proc.Result{})
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
	digest, err := webassets.HashSource(filepath.Join(src, "web"))
	if err != nil {
		t.Fatal(err)
	}
	stageCompletedBuild(t, cacheRoot, digest)

	f := proc.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", proc.Result{Stdout: digest + "\n"})
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
	f := proc.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", proc.Result{Stdout: "abc123\n"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}
	if got := g.stagedWebDigest(context.Background()); got != "abc123" {
		t.Fatalf("stagedWebDigest=%q want abc123", got)
	}
	script := f.Calls[0].Args[len(f.Calls[0].Args)-1]
	if !strings.Contains(script, "test -f") || !strings.Contains(script, layout.WebAssetsSentinel) {
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
	empty := proc.NewFakeRunner()
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
	tmp := layout.WebAssetsDir + ".tmp"
	for _, want := range []string{
		"-C '" + tmp + "'",
		"mv '" + tmp + "' '" + layout.WebAssetsDir + "'",
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
