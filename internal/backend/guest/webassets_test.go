package guest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
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

func fakePrebuilt(t *testing.T, fn func(ctx context.Context, version string) (string, string, error)) {
	t.Helper()
	old := prebuiltWebUI
	t.Cleanup(func() { prebuiltWebUI = old })
	prebuiltWebUI = fn
}

// A release's web UI is staged as-is: no Go, no node.
func TestEnsureScionWebAssetsStagesThePrebuiltUI(t *testing.T) {
	dist := filepath.Join(t.TempDir(), "client")
	write(t, filepath.Join(dist, filepath.FromSlash(layout.WebAssetsSentinel)), "x")
	fakePrebuilt(t, func(_ context.Context, version string) (string, string, error) {
		if version != scionbin.ReleasePin {
			t.Fatalf("version = %s", version)
		}
		return dist, "release-abc", nil
	})
	f := proc.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", proc.Result{})
	f.Script("orb -u root -m m bash -c", proc.Result{})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Version: scionbin.ReleasePin, WebUI: true}); err != nil {
		t.Fatalf("EnsureScionWebAssets: %v", err)
	}
	var staged bool
	for _, c := range f.Calls {
		if c.Name == "go" || c.Name == "npm" || c.Name == "node" {
			t.Fatalf("a prebuilt UI ran %s %v", c.Name, c.Args)
		}
		if c.Stdin != "" {
			staged = strings.Contains(c.Args[len(c.Args)-1], "release-abc")
		}
	}
	if !staged {
		t.Fatal("the prebuilt UI was not staged under its digest")
	}
}

// A refused download stops the apply; an unavailable one builds instead.
func TestEnsureScionWebAssetsPrebuiltErrors(t *testing.T) {
	g := func(f *proc.FakeRunner) Guest {
		return Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
	}
	fakePrebuilt(t, func(context.Context, string) (string, string, error) {
		return "", "", fmt.Errorf("%w: bad bytes", scionbin.ErrReleaseTampered)
	})
	f := proc.NewFakeRunner()
	err := g(f).EnsureScionWebAssets(context.Background(), ScionSpec{Version: scionbin.ReleasePin, WebUI: true})
	if !errors.Is(err, scionbin.ErrReleaseTampered) || len(f.Calls) != 0 {
		t.Fatalf("err = %v, calls %+v; want the refusal and no fallback build", err, f.Calls)
	}

	// Unavailable: the source build runs, starting with the module download
	// (which this runner fails).
	fakePrebuilt(t, func(context.Context, string) (string, string, error) {
		_, _, err := webassets.Prebuilt(context.Background(), "666333f9")
		return "", "", err
	})
	f = proc.NewFakeRunner()
	err = g(f).EnsureScionWebAssets(context.Background(), ScionSpec{Version: scionbin.ReleasePin, WebUI: true})
	if err == nil || len(f.Calls) == 0 || f.Calls[0].Name != "go" {
		t.Fatalf("err = %v, calls %+v; want the source build attempted", err, f.Calls)
	}
}

// T0305: a pin built from source once is found by its record on the next
// command, with no `go` at all, so no `go mod download` asks the module proxy.
// The first command builds (here: finds the npm build cached by digest) and
// records the pin.
func TestEnsureScionWebAssetsReusesAPinnedSourceBuildOffline(t *testing.T) {
	cacheRoot := isolateCacheDir(t)
	src := fakeScionSource(t)
	digest, err := webassets.HashSource(filepath.Join(src, "web"))
	if err != nil {
		t.Fatal(err)
	}
	stageCompletedBuild(t, cacheRoot, digest)
	const pin = "65531d7f"
	fakePrebuilt(t, func(context.Context, string) (string, string, error) {
		_, _, err := webassets.Prebuilt(context.Background(), "666333f9")
		return "", "", err
	})
	newGuest := func() (*proc.FakeRunner, Guest) {
		f := proc.NewFakeRunner()
		f.Script("orb -m m /bin/bash -c", proc.Result{})
		f.Script("orb -u root -m m bash -c", proc.Result{})
		return f, Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
	}

	f, g := newGuest()
	f.Script("go env GOROOT", proc.Result{Stdout: "/goroot\n"})
	f.Script("/goroot/bin/go mod download -json", proc.Result{Stdout: fmt.Sprintf(`{"Dir":%q}`, src)})
	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Version: pin, WebUI: true}); err != nil {
		t.Fatalf("first EnsureScionWebAssets: %v", err)
	}
	var downloaded bool
	for _, c := range f.Calls {
		downloaded = downloaded || slices.Contains(c.Args, "download")
	}
	if !downloaded {
		t.Fatalf("the first command did not fetch the sources: %+v", f.Calls)
	}
	if _, got, ok := webassets.PinnedBuild(pin); !ok || got != digest {
		t.Fatalf("PinnedBuild after the first build = %q %v, want %s", got, ok, digest)
	}

	// Second command: no go, no npm; the build is staged under its digest.
	f, g = newGuest()
	if err := g.EnsureScionWebAssets(context.Background(), ScionSpec{Version: pin, WebUI: true}); err != nil {
		t.Fatalf("second EnsureScionWebAssets: %v", err)
	}
	var staged bool
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Name, "go") || c.Name == "npm" || c.Name == "node" {
			t.Fatalf("a recorded pin ran %s %v", c.Name, c.Args)
		}
		if c.Stdin != "" {
			staged = strings.Contains(c.Args[len(c.Args)-1], digest)
		}
	}
	if !staged {
		t.Fatal("the recorded build was not staged under its digest")
	}

	// The build gone from the cache: the record no longer counts, and the
	// sources are fetched again.
	if err := os.Remove(filepath.Join(cacheRoot, digest, webassets.BuildMarker)); err != nil {
		t.Fatal(err)
	}
	f, g = newGuest()
	err = g.EnsureScionWebAssets(context.Background(), ScionSpec{Version: pin, WebUI: true})
	if err == nil || len(f.Calls) == 0 || f.Calls[0].Name != "go" {
		t.Fatalf("err = %v, calls %+v; want the source fetch attempted", err, f.Calls)
	}
}
