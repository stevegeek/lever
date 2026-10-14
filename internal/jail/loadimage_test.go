package jail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestLoadImageArgs(t *testing.T) {
	got := loadImageArgs(orbPrefix("lever-demo", "leveruser"), "501")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "load",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadImageArgs:\n got  %v\n want %v", got, want)
	}
}

func TestImageInspectArgs(t *testing.T) {
	got := imageInspectArgs(orbPrefix("lever-demo", "leveruser"), "501", "scionlocal/lever-claude:latest")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "image", "inspect", "--format", "{{.Id}}", "scionlocal/lever-claude:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imageInspectArgs:\n got  %v\n want %v", got, want)
	}
}

// TestNormalizeImageID: docker prints the ID as "sha256:<hex>", some podman
// versions print bare "<hex>"; ImageLoaded must strip both sides to match.
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

// imageLoadedRunner scripts the host docker and the jail prefix call
// independently. A missing script makes that side error, as an absent image
// does, which the readers map to "".
func imageLoadedRunner(t *testing.T, hostOut, jailOut string) *proc.FakeRunner {
	t.Helper()
	r := proc.NewFakeRunner()
	if hostOut != "" {
		r.Script("docker", proc.Result{Stdout: hostOut})
	}
	if jailOut != "" {
		r.Script("orb", proc.Result{Stdout: jailOut})
	}
	return r
}

// TestImageLoaded: the host-vs-jail comparison fails open (reports not
// loaded) whenever either side cannot be read.
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
		// docker prints "sha256:<hex>", podman prints bare "<hex>".
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
	r := proc.NewFakeRunner() // no script for the prefix binary -> Run errors
	err := PruneImages(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501")
	if !errors.Is(err, proc.ErrUnscripted) {
		t.Fatalf("PruneImages must propagate the runner error, got %v", err)
	}
	if !strings.Contains(err.Error(), "prune images") {
		t.Errorf("error missing context prefix: %v", err)
	}
}

func TestPruneImagesSuccess(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
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
	got := pruneImagesArgs(orbPrefix("lever-demo", "leveruser"), "501")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "image", "prune", "-f",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruneImagesArgs:\n got  %v\n want %v", got, want)
	}
}

// TestWarmImageArgs pins the warm-up argv: scion's exact keep-id
// flag (a different mapping builds a different layer copy), `true` as the
// entrypoint so neither sciontool's hook nor claude runs, and no pull or
// network.
func TestWarmImageArgs(t *testing.T) {
	got := warmImageArgs(orbPrefix("lever-demo", "leveruser"), "501", "scionlocal/lever-claude:arm64")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "run", "--rm", "--pull=never", "--network=none",
		"--userns=keep-id:uid=1000,gid=1000", "--entrypoint", "true", "scionlocal/lever-claude:arm64",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("warmImageArgs:\n got  %v\n want %v", got, want)
	}
}

func TestWarmImage(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	if err := WarmImage(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501", "img"); err != nil {
		t.Fatalf("WarmImage: %v", err)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "orb" {
		t.Fatalf("expected one orb-prefixed call, got %+v", r.Calls)
	}

	r = proc.NewFakeRunner() // no script for the prefix binary -> Run errors
	err := WarmImage(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501", "img")
	if !errors.Is(err, proc.ErrUnscripted) || !strings.Contains(err.Error(), "warm image lever.local/img") {
		t.Fatalf("WarmImage error = %v, want the runner error wrapped with the name agents run", err)
	}
}

// TestLoadImageStreamsSaveIntoPodmanLoad: the producer's bytes reach the jail's
// `podman load` as stdin, through the prefix argv — no host shell, no pipeline
// string.
func TestLoadImageStreamsSaveIntoPodmanLoad(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	err := loadImage(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501", func(w io.Writer) error {
		_, err := io.WriteString(w, "tarball-bytes")
		return err
	})
	if err != nil {
		t.Fatalf("loadImage: %v", err)
	}
	if len(r.Calls) != 1 {
		t.Fatalf("want one host call, got %+v", r.Calls)
	}
	got := append([]string{r.Calls[0].Name}, r.Calls[0].Args...)
	if !reflect.DeepEqual(got, loadImageArgs(orbPrefix("lever-demo", "leveruser"), "501")) {
		t.Fatalf("argv = %v", got)
	}
	if r.Calls[0].Stdin != "tarball-bytes" {
		t.Fatalf("stdin = %q", r.Calls[0].Stdin)
	}
}

// A failing producer is reported as the cause, not masked by the consumer's
// short read.
func TestLoadImageReportsSaveFailure(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	saveErr := errors.New("docker save: no such image")
	err := loadImage(context.Background(), r, orbPrefix("m", "u"), "501", func(io.Writer) error {
		return saveErr
	})
	if !errors.Is(err, saveErr) {
		t.Fatalf("err = %v, want the producer's error as the cause", err)
	}
}

func TestLoadImageReportsLoadFailure(t *testing.T) {
	r := proc.NewFakeRunner() // unscripted orb -> load fails
	err := loadImage(context.Background(), r, orbPrefix("m", "u"), "501", func(w io.Writer) error {
		_, err := io.WriteString(w, "x")
		return err
	})
	if !errors.Is(err, proc.ErrUnscripted) || !strings.Contains(err.Error(), "podman load") {
		t.Fatalf("err = %v, want the load failure wrapped under podman load", err)
	}
}

// unreadRunner fails the load without ever reading stdin, the way a podman
// that rejects the command up front does. The producer then sees a closed
// pipe; that write error must not mask the load's own stderr.
type unreadRunner struct{ proc.Runner }

func (unreadRunner) RunStdin(context.Context, io.Reader, map[string]string, string, ...string) (proc.Result, error) {
	return proc.Result{Code: 125, Stderr: "Error: cannot connect to podman\n"}, errors.New("exit status 125")
}

func TestLoadImageLoadFailureBeforeDrainKeepsLoadStderr(t *testing.T) {
	err := loadImage(context.Background(), unreadRunner{}, orbPrefix("m", "u"), "501", func(w io.Writer) error {
		if _, err := io.WriteString(w, "tarball-bytes"); err != nil {
			return fmt.Errorf("docker save: %w", err)
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"podman load", "exit status 125", "cannot connect to podman", "docker save"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q lacks %q", err, want)
		}
	}
	if !strings.HasPrefix(err.Error(), "loadimage: podman load:") {
		t.Errorf("load failure must be primary, got %q", err)
	}
}

// TestAliasArgs pins the post-load re-tag. A docker archive's unqualified
// RepoTag lands in podman as docker.io/<ref>. Agents run it as
// lever.local/<ref>, a qualified name scion never rewrites to the hub's image
// registry, and an agent record created on the unqualified <ref> resolves it
// to localhost/<ref>, which after a rebuild would still point at the old
// image; every name is tagged onto the fresh image.
func TestAliasArgs(t *testing.T) {
	pre := []string{"orb", "-m", "lever-demo", "-u", "leveruser", "env", "XDG_RUNTIME_DIR=/run/user/501", "podman", "tag"}
	for _, c := range []struct {
		ref  string
		tags []string
	}{
		{"scionlocal/lever-claude:arm64", []string{"docker.io/scionlocal/lever-claude:arm64", "localhost/scionlocal/lever-claude:arm64", "lever.local/lever-claude:arm64"}},
		{"scionlocal/x", []string{"docker.io/scionlocal/x:latest", "localhost/scionlocal/x:latest", "lever.local/x:latest"}},
		{"alpine:3", []string{"docker.io/library/alpine:3", "localhost/alpine:3", "lever.local/alpine:3"}},
		{"localhost/scionlocal/x:1", []string{"localhost/scionlocal/x:1", "lever.local/localhost/scionlocal/x:1"}},
	} {
		got := aliasArgs(orbPrefix("lever-demo", "leveruser"), "501", c.ref)
		if want := append(append([]string{}, pre...), c.tags...); !reflect.DeepEqual(got, want) {
			t.Errorf("aliasArgs(%q):\n got  %v\n want %v", c.ref, got, want)
		}
	}
}

// A ref that names its registry is loaded under that exact name and needs no
// alias; neither does a digest-pinned one.
func TestAliasSkipsQualifiedRefs(t *testing.T) {
	// A digest-pinned ref (security.require_image_digest) is self-naming
	// too: docker save writes it with no RepoTag, and a digest is not a
	// legal tag target, so an alias attempt could only fail the load.
	for _, ref := range []string{"ghcr.io/org/img:1", "lever.local/lever-claude:arm64", "reg:5000/img:1", "docker.io/scionlocal/x:latest",
		"scionlocal/x@sha256:" + strings.Repeat("a", 64), "alpine@sha256:" + strings.Repeat("a", 64)} {
		r := proc.NewFakeRunner()
		r.Script("orb", proc.Result{})
		if err := aliasLoaded(context.Background(), r, orbPrefix("m", "u"), "501", ref); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if len(r.Calls) != 0 {
			t.Errorf("%s: qualified ref must not be re-tagged, got %+v", ref, r.Calls)
		}
	}
}

// TestImageLoadedChecksTheAgentName: an install whose jail holds the image
// only under its old unqualified name is not loaded, so the next apply loads
// it again and tags the name agents run.
func TestImageLoadedChecksTheAgentName(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("docker", proc.Result{Stdout: testImageID})
	r.Script("orb", proc.Result{Stdout: testImageID})
	if !ImageLoaded(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/lever-claude:arm64") {
		t.Fatal("want loaded")
	}
	if got := r.Calls[1].Args[len(r.Calls[1].Args)-1]; got != "lever.local/lever-claude:arm64" {
		t.Fatalf("the jail was asked for %q, want the agent name", got)
	}
}

// Both load paths alias after a successful load; a failed load does not.
func TestLoadImagePathsAliasLocalhostAfterLoad(t *testing.T) {
	path, _ := writeDockerArchive(t, t.TempDir(), tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}})
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	if err := LoadImageTar(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/lever-claude:arm64", path, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 2 || !strings.Contains(strings.Join(r.Calls[1].Args, " "), "podman tag docker.io/scionlocal/lever-claude:arm64 localhost/scionlocal/lever-claude:arm64 lever.local/lever-claude:arm64") {
		t.Fatalf("want load then alias, got %+v", r.Calls)
	}
	// The docker-save path shares the same tail.
	r = proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	if err := loadImageAndAlias(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/x", func(w io.Writer) error {
		_, err := io.WriteString(w, "bytes")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 2 || !strings.Contains(strings.Join(r.Calls[1].Args, " "), "podman tag docker.io/scionlocal/x:latest localhost/scionlocal/x:latest lever.local/x:latest") {
		t.Fatalf("want load then alias with :latest filled in, got %+v", r.Calls)
	}
	// A failed load never re-tags.
	r = proc.NewFakeRunner()
	if err := loadImageAndAlias(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/x", func(w io.Writer) error { return nil }); err == nil {
		t.Fatal("want load error")
	}
	if len(r.Calls) != 1 {
		t.Fatalf("a failed load must not be followed by a tag, got %+v", r.Calls)
	}
}

// An alias failure is fatal: the container would otherwise run the stale
// localhost/ image while apply reports success.
func TestLocalhostAliasFailureIsFatal(t *testing.T) {
	r := &loadThenFail{FakeRunner: proc.NewFakeRunner()}
	err := loadImageAndAlias(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/x:latest", func(w io.Writer) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "localhost/scionlocal/x:latest and lever.local/x:latest") {
		t.Fatalf("err = %v, want the alias failure naming the target tag", err)
	}
}

// loadThenFail accepts the stdin load and fails every plain Run (the tag).
type loadThenFail struct{ *proc.FakeRunner }

func (l *loadThenFail) RunStdin(ctx context.Context, in io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	io.Copy(io.Discard, in)
	return proc.Result{}, nil
}
func (l *loadThenFail) Run(context.Context, map[string]string, string, ...string) (proc.Result, error) {
	return proc.Result{Stderr: "Error: tag: no such image"}, errors.New("exit status 125")
}
