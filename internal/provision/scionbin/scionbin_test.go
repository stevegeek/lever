package scionbin_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

func TestValidateNamesAllThreeKeys(t *testing.T) {
	err := scionbin.Spec{}.Validate()
	if err == nil {
		t.Fatal("expected an error when no scion mode is configured")
	}
	for _, want := range []string{"binary", "source", "version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the %q key", err, want)
		}
	}
}

func TestResolveSourceMissingNeverBuilds(t *testing.T) {
	f := proc.NewFakeRunner()
	_, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Source: "/does/not/exist"}, "arm64", "m")
	var srcErr *scionbin.SourceError
	if !errors.As(err, &srcErr) || srcErr.Path != "/does/not/exist" || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error should be a SourceError wrapping the stat failure; got: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("go must not run when the source is missing: %+v", f.Calls)
	}
}

func TestResolveSourceCrossCompiles(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	src := t.TempDir()
	out, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Source: src}, "arm64", "lever-jail")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want, err := scionbin.OutputPath("lever-jail")
	if err != nil {
		t.Fatal(err)
	}
	if out != want || !strings.Contains(out, "lever-scion-lever-jail") {
		t.Fatalf("output path %q, want %q", out, want)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %+v", f.Calls)
	}
	c := f.Calls[0]
	if c.Name != "go" || c.Dir != src || c.Env["GOOS"] != "linux" || c.Env["GOARCH"] != "arm64" {
		t.Fatalf("build call = %+v", c)
	}
	if strings.Join(c.Args, " ") != "build -trimpath -ldflags=-s -w -o "+out+" ./cmd/scion" {
		t.Fatalf("build args = %v", c.Args)
	}
}

// Version mode resolves the real go binary, downloads the pinned module, and
// cross-compiles from the module's source dir with that absolute binary.
func TestResolveVersionBuildsFromPinnedModule(t *testing.T) {
	backendtest.IsolateCache(t)
	const pin = "666333f9"
	const moduleDir = "/mod/github.com/!google!cloud!platform/scion@v0.0.0-x"
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json "+scionbin.ModulePath+"@"+pin,
		proc.Result{Stdout: `{"Version":"v0.0.0-x","Dir":"` + moduleDir + `"}`})
	f.Script("/opt/go/bin/go build -trimpath -ldflags=-s -w -o", proc.Result{})

	if _, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Version: pin}, "arm64", "m"); err != nil {
		t.Fatalf("Resolve(version): %v", err)
	}
	bi := f.CallIndex(proc.Subcommand("/opt/go/bin/go", "build"))
	if bi < 0 {
		t.Fatal("expected a cross-compile build with the resolved absolute go binary")
	}
	build := f.Calls[bi]
	if build.Dir != moduleDir {
		t.Fatalf("build ran in %q, want the pinned module dir %q", build.Dir, moduleDir)
	}
	if !slices.Contains(build.Args, "-trimpath") || !slices.Contains(build.Args, "-ldflags=-s -w") {
		t.Fatalf("the pinned build must be stripped and trimmed (lever#191): %v", build.Args)
	}
}

func TestFetchModuleErrors(t *testing.T) {
	cases := []struct {
		name, stdout, want string
	}{
		{"download error", `{"Error":"unknown revision deadbeef"}`, "unknown revision"},
		{"no dir", `{"Version":"v0"}`, "no source dir"},
		{"garbage", `not json`, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
			f.Script("/opt/go/bin/go mod download -json", proc.Result{Stdout: c.stdout})
			_, _, err := scionbin.FetchModule(context.Background(), f, "deadbeef")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// In binary mode nothing ever invokes `go`: the jail host needs no toolchain.
func TestResolveBinaryModeNeverInvokesGo(t *testing.T) {
	bin := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETExec)
	f := proc.NewFakeRunner()
	out, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Binary: bin}, "arm64", "m")
	if err != nil {
		t.Fatalf("Resolve(binary): %v", err)
	}
	if out != bin {
		t.Fatalf("binary mode must return the supplied path; got %q", out)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("binary mode must never run a command; got %+v", f.Calls)
	}
}

func TestResolveBinaryModeRejectsWrongArch(t *testing.T) {
	bin := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETExec)
	if _, err := scionbin.Resolve(context.Background(), proc.NewFakeRunner(), scionbin.Spec{Binary: bin}, "amd64", "m"); err == nil {
		t.Fatal("expected an arch mismatch error")
	}
}

func TestBuildsWebAssets(t *testing.T) {
	cases := []struct {
		name string
		spec scionbin.Spec
		want bool
	}{
		{"version + web ui", scionbin.Spec{Version: "e82a2a08", WebUI: true}, true},
		{"source + web ui", scionbin.Spec{Source: "/src/scion", WebUI: true}, true},
		{"version without web ui", scionbin.Spec{Version: "e82a2a08"}, false},
		// A prebuilt binary carries no source to build from, and may already
		// embed its own assets — skip, never fail.
		{"binary + web ui", scionbin.Spec{Binary: "/bin/scion", WebUI: true}, false},
		{"nothing", scionbin.Spec{WebUI: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.BuildsWebAssets(); got != c.want {
				t.Fatalf("BuildsWebAssets()=%v want %v", got, c.want)
			}
		})
	}
}

// TestResolveBuildsUnderThePerUserCacheDir: the output must not be a
// predictable name under the shared os.TempDir, where another local user can
// pre-create the path and swap the binary between the host-side hash and the
// root install into the jail. It goes in a 0700 directory under the
// operator's cache directory.
func TestResolveBuildsUnderThePerUserCacheDir(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	out, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Source: t.TempDir()}, "arm64", "lever-jail")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, cache+string(filepath.Separator)) {
		t.Fatalf("output %q is not under the user cache dir %q", out, cache)
	}
	if filepath.Dir(out) == os.TempDir() {
		t.Fatalf("output %q sits directly in the shared temp dir", out)
	}
	fi, err := os.Stat(filepath.Dir(out))
	if err != nil {
		t.Fatalf("the build directory must exist before go build runs: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("build directory mode = %v, want a 0700 directory", fi.Mode())
	}
	dir, err := scionbin.OutputDir()
	if err != nil || filepath.Dir(out) != dir {
		t.Fatalf("OutputDir() = %q, %v; output is in %q", dir, err, filepath.Dir(out))
	}
}

// Without a resolvable cache dir there is nowhere safe to build; scionbin.Resolve must
// say so rather than fall back to a shared location.
func TestResolveFailsWithoutAUserCacheDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	if _, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Source: t.TempDir()}, "arm64", "m"); err == nil {
		t.Fatal("expected an error when the user cache dir cannot be resolved")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("go must not run without a build directory: %+v", f.Calls)
	}
}

func gitRepo(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}

// SourceHasCommit against real git: the exit-code mapping is the gate, so
// it is not stubbed here.
func TestSourceHasCommit(t *testing.T) {
	dir := t.TempDir()
	git := gitRepo(t, dir)
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "one")
	first := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "two")
	second := git("rev-parse", "HEAD")
	git("checkout", "-q", first)
	ctx := context.Background()
	has := func(source, commit string) (bool, error) {
		return scionbin.SourceHasCommit(ctx, proc.RealRunner{}, source, commit)
	}
	if ok, err := has(dir, first); !ok || err != nil {
		t.Fatalf("HEAD itself: %v %v", ok, err)
	}
	git("checkout", "-q", second)
	if ok, err := has(dir, first); !ok || err != nil {
		t.Fatalf("an ancestor: %v %v", ok, err)
	}
	git("checkout", "-q", first)
	// git has the object, but HEAD does not hold it (merge-base exits 1).
	if ok, err := has(dir, second); ok || err != nil {
		t.Fatalf("a later commit: %v %v", ok, err)
	}
	// git has no such object (merge-base exits 128): "not held", never
	// "cannot tell".
	if ok, err := has(dir, config.AgentMessagesScionFloor); ok || err != nil {
		t.Fatalf("a commit git does not have: %v %v, want false, nil", ok, err)
	}
	// Not a repository: cannot tell.
	if ok, err := has(t.TempDir(), first); ok || err == nil || errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("not a checkout: %v %v, want a cannot-tell error", ok, err)
	}
	if ok, err := has(filepath.Join(t.TempDir(), "missing"), first); ok || err == nil || errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("no such directory: %v %v, want a cannot-tell error", ok, err)
	}
	// The deadline: cannot tell.
	done, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := scionbin.SourceHasCommit(done, proc.RealRunner{}, dir, config.AgentMessagesScionFloor); ok || err == nil || errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("expired context: %v %v, want a cannot-tell error", ok, err)
	}
}

// A shallow clone cuts history off, so a commit it does not show might be
// below the cut: SourceHasCommit says so (ErrShallowSource, which the gate
// refuses) rather than "not held" or "cannot tell". A commit it does show
// as an ancestor is still held.
func TestSourceHasCommitShallow(t *testing.T) {
	up := t.TempDir()
	git := gitRepo(t, up)
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "one")
	first := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "two")
	git("commit", "-q", "--allow-empty", "-m", "three")
	head := git("rev-parse", "HEAD")
	clone := filepath.Join(t.TempDir(), "c")
	git("clone", "-q", "--depth", "1", "file://"+up, clone)
	ctx := context.Background()
	has := func(commit string) (bool, error) {
		return scionbin.SourceHasCommit(ctx, proc.RealRunner{}, clone, commit)
	}
	if ok, err := has(head); !ok || err != nil {
		t.Fatalf("HEAD of a shallow clone: %v %v", ok, err)
	}
	if ok, err := has(first); ok || !errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("an ancestor below the cut: %v %v, want ErrShallowSource", ok, err)
	}
	if ok, err := has(config.AgentMessagesScionFloor); ok || !errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("an unknown commit: %v %v, want ErrShallowSource", ok, err)
	}
}

// git not runnable at all: cannot tell.
func TestSourceHasCommitWithoutGit(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir)("init", "-q")
	t.Setenv("PATH", t.TempDir())
	ok, err := scionbin.SourceHasCommit(context.Background(), proc.RealRunner{}, dir, config.AgentMessagesScionFloor)
	if ok || err == nil || errors.Is(err, scionbin.ErrShallowSource) {
		t.Fatalf("no git: %v %v, want a cannot-tell error", ok, err)
	}
}

// buildStampFixture is a Version-mode Resolve against a fake toolchain at
// a temp GOROOT whose VERSION file the stamp reads, with a fake build that
// writes the output.
type buildStampFixture struct {
	t    *testing.T
	root string
	out  string
}

func newBuildStampFixture(t *testing.T) *buildStampFixture {
	t.Helper()
	backendtest.IsolateCache(t)
	for _, k := range []string{"GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT", "GOAMD64", "GOARM64", "CGO_ENABLED", "GODEBUG"} {
		t.Setenv(k, "")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.4\ntime 2026-06-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := scionbin.OutputPath("m")
	if err != nil {
		t.Fatal(err)
	}
	return &buildStampFixture{t: t, root: root, out: out}
}

// resolve runs Resolve and reports whether it built (ran `go mod download`
// and `go build`).
func (x *buildStampFixture) resolve(pin string) bool {
	x.t.Helper()
	f := proc.NewFakeRunner()
	goBin := filepath.Join(x.root, "bin", "go")
	f.Script("go env GOROOT", proc.Result{Stdout: x.root + "\n"})
	f.Script(goBin+" mod download -json", proc.Result{Stdout: `{"Dir":"/mod/scion"}`})
	f.Script(goBin+" build -trimpath -ldflags=-s -w -o", proc.Result{})
	// The output a build would leave (the fake build writes nothing).
	if _, err := os.Stat(x.out); err != nil {
		if err := os.MkdirAll(filepath.Dir(x.out), 0o700); err != nil {
			x.t.Fatal(err)
		}
		if err := os.WriteFile(x.out, []byte("fake-scion"), 0o755); err != nil {
			x.t.Fatal(err)
		}
	}
	got, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Version: pin}, "arm64", "m")
	if err != nil || got != x.out {
		x.t.Fatalf("Resolve = %q, %v", got, err)
	}
	built := f.Called(proc.Subcommand(goBin, "build"))
	if downloaded := f.Called(proc.Subcommand(goBin, "mod")); downloaded != built {
		x.t.Fatalf("download=%v build=%v: both or neither", downloaded, built)
	}
	return built
}

// A Version-mode build leaves a stamp; the next Resolve of the same pin,
// arch and toolchain skips the download and build. Anything in the key, or
// the output itself, changing builds again.
func TestResolveVersionSkipsTheBuildOnAMatchingStamp(t *testing.T) {
	x := newBuildStampFixture(t)
	if !x.resolve("aaaaaaaa") {
		t.Fatal("the first resolve must build")
	}
	if x.resolve("aaaaaaaa") {
		t.Fatal("a matching stamp must skip the build")
	}
	if !x.resolve("bbbbbbbb") {
		t.Fatal("another pin must build")
	}
	if x.resolve("bbbbbbbb") {
		t.Fatal("the new pin's stamp must hold")
	}
	t.Setenv("GOFLAGS", "-tags=x")
	if !x.resolve("bbbbbbbb") {
		t.Fatal("a GOFLAGS change must build")
	}
	if err := os.WriteFile(filepath.Join(x.root, "VERSION"), []byte("go1.26.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !x.resolve("bbbbbbbb") {
		t.Fatal("a toolchain change must build")
	}
	// The output replaced since the build (another size).
	if err := os.WriteFile(x.out, []byte("something else entirely"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !x.resolve("bbbbbbbb") {
		t.Fatal("a changed output must build")
	}
	// Without a readable toolchain version, no stamp: build every time.
	if err := os.Remove(filepath.Join(x.root, "VERSION")); err != nil {
		t.Fatal(err)
	}
	if !x.resolve("bbbbbbbb") || !x.resolve("bbbbbbbb") {
		t.Fatal("no toolchain version must always build")
	}
}

// A failed build leaves no stamp that could vouch for the old output.
func TestResolveFailedBuildRemovesTheStamp(t *testing.T) {
	x := newBuildStampFixture(t)
	x.resolve("aaaaaaaa")
	if _, err := os.Stat(x.out + ".stamp"); err != nil {
		t.Fatalf("no stamp after a build: %v", err)
	}
	f := proc.NewFakeRunner()
	goBin := filepath.Join(x.root, "bin", "go")
	f.Script("go env GOROOT", proc.Result{Stdout: x.root + "\n"})
	f.Script(goBin+" mod download -json", proc.Result{Stdout: `{"Dir":"/mod/scion"}`})
	// No build script: the build fails.
	if _, err := scionbin.Resolve(context.Background(), f, scionbin.Spec{Version: "cccccccc"}, "arm64", "m"); err == nil {
		t.Fatal("want the build failure")
	}
	if _, err := os.Stat(x.out + ".stamp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stamp after a failed build: %v", err)
	}
}

// A version that can move (a branch, "latest") is fetched and built every
// time; only commits and semantic or pseudo-versions are stamped.
func TestResolveNeverStampsAMovingVersion(t *testing.T) {
	x := newBuildStampFixture(t)
	for _, v := range []string{"main", "latest", "feature/x", "v1.2"} {
		if !x.resolve(v) || !x.resolve(v) {
			t.Errorf("%q: a moving version must build every time", v)
		}
	}
	for _, v := range []string{"666333f9", "v1.2.3", "v0.0.0-20260101000000-abcdef123456"} {
		x.resolve(v)
		if x.resolve(v) {
			t.Errorf("%q: an immutable version must be stamped", v)
		}
	}
}
