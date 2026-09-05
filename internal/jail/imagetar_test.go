package jail

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// tarImageSpec describes one image to write into a synthetic docker archive.
type tarImageSpec struct {
	repoTags []string
	labels   map[string]string
	legacy   bool // "<hex>.json" config path instead of "blobs/sha256/<hex>"
}

// writeDockerArchive writes a docker-save-shaped tar to dir: a (fake) layer
// blob first, the config blobs, then manifest.json LAST — the order docker
// 29 emits, which is what makes a stream-only reader pay for the whole
// archive. Returns the path and the config digest of each image, in order.
func writeDockerArchive(t *testing.T, dir string, specs ...tarImageSpec) (string, []string) {
	t.Helper()
	path := filepath.Join(dir, "images.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	add := func(name string, body []byte) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	layer := []byte(strings.Repeat("layer-bytes-", 4096))
	add("blobs/sha256/"+strings.Repeat("1", 64), layer)
	var manifest []map[string]any
	var digests []string
	for i, s := range specs {
		cfg, _ := json.Marshal(map[string]any{
			"architecture": "arm64",
			"config":       map[string]any{"Labels": s.labels},
			"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + strings.Repeat("1", 64)}},
			"_i":           i, // keeps two configs distinct
		})
		sum := sha256.Sum256(cfg)
		d := hex.EncodeToString(sum[:])
		digests = append(digests, d)
		cfgPath := "blobs/sha256/" + d
		if s.legacy {
			cfgPath = d + ".json"
		}
		add(cfgPath, cfg)
		manifest = append(manifest, map[string]any{
			"Config":   cfgPath,
			"RepoTags": s.repoTags,
			"Layers":   []string{"blobs/sha256/" + strings.Repeat("1", 64)},
		})
	}
	mb, _ := json.Marshal(manifest)
	add("manifest.json", mb)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path, digests
}

func TestReadImageTarParsesManifestAndConfig(t *testing.T) {
	path, digests := writeDockerArchive(t, t.TempDir(),
		tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}, labels: map[string]string{"claude_code_version": "2.1.207"}},
		tarImageSpec{repoTags: []string{"scionlocal/lever-claude:latest", "scionlocal/lever-claude:v3"}, legacy: true},
	)
	imgs, err := ReadImageTar(path)
	if err != nil {
		t.Fatalf("ReadImageTar: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("got %d images, want 2", len(imgs))
	}
	if imgs[0].ConfigDigest != digests[0] || imgs[1].ConfigDigest != digests[1] {
		t.Errorf("digests = %q/%q, want %q/%q", imgs[0].ConfigDigest, imgs[1].ConfigDigest, digests[0], digests[1])
	}
	if got := imgs[0].Labels["claude_code_version"]; got != "2.1.207" {
		t.Errorf("label = %q, want 2.1.207", got)
	}
	if len(imgs[1].RepoTags) != 2 || imgs[1].RepoTags[1] != "scionlocal/lever-claude:v3" {
		t.Errorf("repo tags = %v", imgs[1].RepoTags)
	}
}

func TestReadImageTarRejectsNonArchive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not.tar")
	os.WriteFile(p, []byte("hello"), 0o644)
	if _, err := ReadImageTar(p); err == nil {
		t.Fatal("want error for a non-tar file")
	}
	if _, err := ReadImageTar(filepath.Join(dir, "missing.tar")); err == nil {
		t.Fatal("want error for a missing file")
	}
}

func TestReadImageTarRejectsArchiveWithoutManifest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.tar")
	f, _ := os.Create(p)
	tw := tar.NewWriter(f)
	tw.WriteHeader(&tar.Header{Name: "blobs/sha256/abc", Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	f.Close()
	_, err := ReadImageTar(p)
	if err == nil || !strings.Contains(err.Error(), "manifest.json") {
		t.Fatalf("err = %v, want a manifest.json complaint", err)
	}
}

// TestFindTarImage pins reference normalisation: docker writes RepoTags
// unqualified (`scionlocal/x:latest`, `alpine:latest`), while a config may
// name the image qualified, tagless, or both.
func TestFindTarImage(t *testing.T) {
	imgs := []TarImage{
		{RepoTags: []string{"scionlocal/lever-claude:arm64"}, ConfigDigest: "a"},
		{RepoTags: []string{"alpine:latest"}, ConfigDigest: "b"},
	}
	cases := []struct {
		ref  string
		want string // digest, "" ⇒ not found
	}{
		{"scionlocal/lever-claude:arm64", "a"},
		{"docker.io/scionlocal/lever-claude:arm64", "a"},
		{"scionlocal/lever-claude:latest", ""},
		{"scionlocal/lever-claude", ""}, // tagless ⇒ :latest, which the tar lacks
		{"alpine", "b"},
		{"docker.io/library/alpine:latest", "b"},
		{"library/alpine", "b"},
		{"ghcr.io/scionlocal/lever-claude:arm64", ""},
	}
	for _, tc := range cases {
		got, err := FindTarImage(imgs, tc.ref)
		if tc.want == "" {
			if err == nil {
				t.Errorf("FindTarImage(%q) found %q, want not found", tc.ref, got.ConfigDigest)
			} else if !strings.Contains(err.Error(), tc.ref) {
				t.Errorf("FindTarImage(%q) error must name the ref: %v", tc.ref, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("FindTarImage(%q): %v", tc.ref, err)
		} else if got.ConfigDigest != tc.want {
			t.Errorf("FindTarImage(%q) = %q, want %q", tc.ref, got.ConfigDigest, tc.want)
		}
	}
}

func TestFindTarImageErrorListsAvailableTags(t *testing.T) {
	imgs := []TarImage{{RepoTags: []string{"scionlocal/lever-claude:arm64"}}}
	_, err := FindTarImage(imgs, "scionlocal/lever-claude:latest")
	if err == nil || !strings.Contains(err.Error(), "scionlocal/lever-claude:arm64") {
		t.Fatalf("err = %v, want the tar's tags listed", err)
	}
}

// TestLoadImageTarStreamsFileIntoPodmanLoad: the archive's bytes reach the
// jail's `podman load` on stdin, exactly as the docker-save path does, and
// host docker is never invoked.
func TestLoadImageTarStreamsFileIntoPodmanLoad(t *testing.T) {
	path, _ := writeDockerArchive(t, t.TempDir(), tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}})
	want, _ := os.ReadFile(path)
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	err := LoadImageTar(context.Background(), r, orbPrefix("lever-demo", "leveruser"), "501", "scionlocal/lever-claude:arm64", path)
	if err != nil {
		t.Fatalf("LoadImageTar: %v", err)
	}
	if len(r.Calls) == 0 || r.Calls[0].Name != "orb" {
		t.Fatalf("want an orb call (podman load) first, got %+v", r.Calls)
	}
	if r.Called(func(c proc.Call) bool { return c.Name == "docker" }) {
		t.Fatal("tar path must never call host docker")
	}
	got := append([]string{r.Calls[0].Name}, r.Calls[0].Args...)
	if !reflect.DeepEqual(got, loadImageArgs(orbPrefix("lever-demo", "leveruser"), "501")) {
		t.Fatalf("argv = %v", got)
	}
	if r.Calls[0].Stdin != string(want) {
		t.Fatalf("stdin (%d bytes) != archive (%d bytes)", len(r.Calls[0].Stdin), len(want))
	}
}

// A tar that does not carry the configured ref is a named config error, and
// nothing is streamed into the jail.
func TestLoadImageTarRejectsMissingTag(t *testing.T) {
	path, _ := writeDockerArchive(t, t.TempDir(), tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}})
	r := proc.NewFakeRunner()
	r.Script("orb", proc.Result{})
	err := LoadImageTar(context.Background(), r, orbPrefix("m", "u"), "501", "scionlocal/lever-claude:latest", path)
	if err == nil || !strings.Contains(err.Error(), "scionlocal/lever-claude:latest") || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want the ref and the tar path named", err)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("nothing must be loaded on a tag mismatch, got %+v", r.Calls)
	}
}

// TestImageLoadedTar: the skip compares the tar's config digest (what podman
// will assign as the ID) with the jail's ID for the ref — no host docker.
func TestImageLoadedTar(t *testing.T) {
	path, digests := writeDockerArchive(t, t.TempDir(), tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}})
	prefix := orbPrefix("lever-demo", "leveruser")
	cases := []struct {
		name    string
		ref     string
		tar     string
		jailOut string
		want    bool
	}{
		{"matching", "scionlocal/lever-claude:arm64", path, digests[0] + "\n", true},
		{"sha256-prefixed jail id", "scionlocal/lever-claude:arm64", path, "sha256:" + digests[0], true},
		{"jail-missing", "scionlocal/lever-claude:arm64", path, "", false},
		{"rebuilt", "scionlocal/lever-claude:arm64", path, strings.Repeat("a", 64), false},
		{"tag-not-in-tar", "scionlocal/lever-claude:latest", path, digests[0], false},
		{"tar-missing", "scionlocal/lever-claude:arm64", filepath.Join(t.TempDir(), "none.tar"), digests[0], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := proc.NewFakeRunner()
			if tc.jailOut != "" {
				r.Script("orb", proc.Result{Stdout: tc.jailOut})
			}
			if got := ImageLoadedTar(context.Background(), r, prefix, "501", tc.ref, tc.tar); got != tc.want {
				t.Fatalf("ImageLoadedTar = %v, want %v", got, tc.want)
			}
			if r.Called(func(c proc.Call) bool { return c.Name == "docker" }) {
				t.Fatal("tar path must never call host docker")
			}
		})
	}
}

// TestReadImageTarRealArchive checks the reader against an archive docker
// itself wrote. Opt-in: set LEVER_TEST_IMAGE_TAR to a `docker save` output
// and LEVER_TEST_IMAGE_ID to that image's docker ID (`docker image inspect
// --format '{{.Id}}'`), which must equal the parsed config digest.
func TestReadImageTarRealArchive(t *testing.T) {
	path, id := os.Getenv("LEVER_TEST_IMAGE_TAR"), os.Getenv("LEVER_TEST_IMAGE_ID")
	if path == "" || id == "" {
		t.Skip("set LEVER_TEST_IMAGE_TAR and LEVER_TEST_IMAGE_ID to run")
	}
	imgs, err := ReadImageTar(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) == 0 || imgs[0].ConfigDigest != normalizeImageID(id) {
		t.Fatalf("parsed %+v, want config digest %s", imgs, normalizeImageID(id))
	}
	t.Logf("tags %v labels %v", imgs[0].RepoTags, imgs[0].Labels)
}

// TestImageTarLabel: doctor reads an image's baked label from the archive
// when there is no host docker store to inspect.
func TestImageTarLabel(t *testing.T) {
	path, _ := writeDockerArchive(t, t.TempDir(),
		tarImageSpec{repoTags: []string{"scionlocal/lever-claude:arm64"}, labels: map[string]string{"claude_code_version": "2.1.240"}},
		tarImageSpec{repoTags: []string{"scionlocal/bare:latest"}},
	)
	if v, err := ImageTarLabel(path, "scionlocal/lever-claude:arm64", "claude_code_version"); err != nil || v != "2.1.240" {
		t.Fatalf("labelled: %q, %v", v, err)
	}
	if v, err := ImageTarLabel(path, "scionlocal/bare", "claude_code_version"); err != nil || v != "" {
		t.Fatalf("absent label must be \"\" with no error, got %q, %v", v, err)
	}
	if _, err := ImageTarLabel(path, "scionlocal/other:latest", "claude_code_version"); err == nil {
		t.Fatal("a ref the tar lacks must be an error")
	}
}
