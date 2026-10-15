package scionbin_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// webTar is a gzip'd tar of the given entries.
func webTar(t *testing.T, hdrs ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range hdrs {
		body := strings.Repeat("x", int(h.Size))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reg(name string, size int64) tar.Header {
	return tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: size}
}

// withWeb returns a release pinning archive as its web UI, and the path the
// fake server serves it at.
func withWeb(t *testing.T, archive []byte) (*scionbin.Release, string) {
	t.Helper()
	rel := *newFakeScion(t, backendtest.EMX8664).rel
	rel.Web = &scionbin.WebAsset{Name: scionbin.WebAssetName(scionbin.ReleasePin), SHA256: sum(archive), Size: int64(len(archive))}
	return &rel, "/dl/v" + testLeverVersion + "/" + rel.Web.Name
}

func goodWeb(t *testing.T) []byte {
	return webTar(t, tar.Header{Typeflag: tar.TypeDir, Name: "assets/", Mode: 0o755},
		reg(layout.WebAssetsSentinel, 100), reg("index.html", 20))
}

func prebuilt(t *testing.T) (string, string, error) {
	t.Helper()
	return webassets.Prebuilt(context.Background(), scionbin.ReleasePin)
}

// No Go, no node: the runner is never involved, and the second apply reuses
// the verified tree without the network.
func TestPrebuiltWebUIDownloads(t *testing.T) {
	backendtest.IsolateCache(t)
	archive := goodWeb(t)
	rel, path := withWeb(t, archive)
	srv, n := serve(t, path, archive)
	scionbin.UseReleaseForTest(t, rel, srv)

	if !scionbin.DownloadsWebUI(scionbin.ReleasePin) {
		t.Fatal("DownloadsWebUI = false for the release pin")
	}
	dist, digest, err := prebuilt(t)
	if err != nil {
		t.Fatalf("Prebuilt: %v", err)
	}
	if digest != "release-"+rel.Web.SHA256 {
		t.Fatalf("digest = %s", digest)
	}
	if fi, err := os.Stat(filepath.Join(dist, filepath.FromSlash(layout.WebAssetsSentinel))); err != nil || fi.Size() != 100 {
		t.Fatalf("sentinel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		t.Fatal(err)
	}
	root, _ := webassets.CacheRoot()
	if left, _ := filepath.Glob(filepath.Join(root, ".release-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	if _, _, err := prebuilt(t); err != nil || n.Load() != 1 {
		t.Fatalf("second Prebuilt: err %v, requests %d", err, n.Load())
	}
}

func TestPrebuiltWebUIRefusesTampering(t *testing.T) {
	archive := goodWeb(t)
	flipped := append([]byte{}, archive...)
	flipped[len(flipped)/2] ^= 0xff
	for name, body := range map[string][]byte{
		"other bytes": flipped,
		"more bytes":  append(append([]byte{}, archive...), 'x'),
	} {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			rel, path := withWeb(t, archive)
			srv, _ := serve(t, path, body)
			scionbin.UseReleaseForTest(t, rel, srv)
			_, _, err := prebuilt(t)
			if !errors.Is(err, scionbin.ErrReleaseTampered) || scionbin.ReleaseUnavailable(err) {
				t.Fatalf("err = %v, want a tampering refusal (no fallback)", err)
			}
			if !strings.Contains(err.Error(), scionbin.DownloadEnv+"=off") {
				t.Fatalf("the refusal should name the way out: %v", err)
			}
			root, _ := webassets.CacheRoot()
			if left, _ := filepath.Glob(filepath.Join(root, "*")); len(left) != 0 {
				t.Fatalf("a refused download left %v in the cache", left)
			}
		})
	}
}

// A pinned archive that breaks the unpack rules is the release's own fault:
// it stops the apply rather than falling back.
func TestPrebuiltWebUIRefusesALinkInThePinnedArchive(t *testing.T) {
	backendtest.IsolateCache(t)
	archive := webTar(t, reg(layout.WebAssetsSentinel, 10),
		tar.Header{Typeflag: tar.TypeSymlink, Name: "index.html", Linkname: "/etc/shadow"})
	rel, path := withWeb(t, archive)
	srv, _ := serve(t, path, archive)
	scionbin.UseReleaseForTest(t, rel, srv)
	_, _, err := prebuilt(t)
	if !errors.Is(err, webassets.ErrBadReleaseArchive) || scionbin.ReleaseUnavailable(err) {
		t.Fatalf("err = %v, want ErrBadReleaseArchive", err)
	}
}

// Offline, a missing asset, a disallowed redirect, another version or
// downloads turned off: the caller builds from source.
func TestPrebuiltWebUIUnavailableFallsBack(t *testing.T) {
	archive := goodWeb(t)
	for name, setup := range map[string]func(t *testing.T) string{
		"offline": func(t *testing.T) string {
			rel, _ := withWeb(t, archive)
			srv := httptest.NewTLSServer(http.NotFoundHandler())
			srv.Close()
			scionbin.UseReleaseForTest(t, rel, srv)
			return scionbin.ReleasePin
		},
		"not found": func(t *testing.T) string {
			rel, _ := withWeb(t, archive)
			srv, _ := serve(t, "/elsewhere", archive)
			scionbin.UseReleaseForTest(t, rel, srv)
			return scionbin.ReleasePin
		},
		"truncated": func(t *testing.T) string {
			rel, path := withWeb(t, archive)
			srv, _ := serve(t, path, archive[:len(archive)/2])
			scionbin.UseReleaseForTest(t, rel, srv)
			return scionbin.ReleasePin
		},
		"redirect to another host": func(t *testing.T) string {
			rel, path := withWeb(t, archive)
			other, _ := serve(t, path, archive)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, rel, srv)
			return scionbin.ReleasePin
		},
		"no web asset in the release": func(t *testing.T) string {
			srv, _ := serve(t, "/", nil)
			scionbin.UseReleaseForTest(t, newFakeScion(t, backendtest.EMX8664).rel, srv)
			return scionbin.ReleasePin
		},
		"another version": func(t *testing.T) string {
			rel, path := withWeb(t, archive)
			srv, _ := serve(t, path, archive)
			scionbin.UseReleaseForTest(t, rel, srv)
			return "666333f9"
		},
		"downloads off": func(t *testing.T) string {
			rel, path := withWeb(t, archive)
			srv, _ := serve(t, path, archive)
			scionbin.UseReleaseForTest(t, rel, srv)
			t.Setenv(scionbin.DownloadEnv, "off")
			return scionbin.ReleasePin
		},
	} {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			version := setup(t)
			_, _, err := webassets.Prebuilt(context.Background(), version)
			if !scionbin.ReleaseUnavailable(err) {
				t.Fatalf("err = %v, want unavailable (build from source)", err)
			}
		})
	}
}

func TestLoadReleaseValidatesTheWebAsset(t *testing.T) {
	h := strings.Repeat("a", 64)
	base := `{"lever_version":"1.2.3","scion_version":"b41cd6fe","module_version":"v0","commit":"` + testCommit +
		`","assets":{"amd64":{"name":"scion_b41cd6fe_linux_amd64.gz","sha256":"` + h + `","size":10,"binary_sha256":"` + h + `","binary_size":20}}`
	good := base + `,"web":{"name":"scion-web_b41cd6fe.tar.gz","sha256":"` + h + `","size":30}}`
	if rel, err := scionbin.LoadRelease(writeManifest(t, good)); err != nil || rel.Web == nil {
		t.Fatalf("good manifest: %v", err)
	}
	for name, edit := range map[string][2]string{
		"path in name":  {`scion-web_b41cd6fe.tar.gz`, `../scion-web_b41cd6fe.tar.gz`},
		"other name":    {`scion-web_b41cd6fe.tar.gz`, `scion_b41cd6fe_linux_amd64.gz`},
		"short sha256":  {`"sha256":"` + h + `","size":30`, `"sha256":"` + h[:63] + `","size":30`},
		"zero size":     {`"size":30`, `"size":0`},
		"huge size":     {`"size":30`, `"size":999999999999`},
		"unknown field": {`"size":30`, `"size":30,"x":1`},
	} {
		body := strings.Replace(good, edit[0], edit[1], 1)
		if body == good {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := scionbin.LoadRelease(writeManifest(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The generator's web asset is what Prebuilt accepts. npm is not run: the
// build is already in the cache under its source digest.
func TestGenerateReleaseWebUIRoundTrip(t *testing.T) {
	backendtest.IsolateCache(t)
	mod := t.TempDir()
	writeFile(t, filepath.Join(mod, "web", "package.json"), `{"name":"@scion/web-frontend"}`)
	digest, err := webassets.HashSource(filepath.Join(mod, "web"))
	if err != nil {
		t.Fatal(err)
	}
	cache, err := webassets.CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(cache, digest)
	writeFile(t, filepath.Join(built, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel)), "console.log(1)")
	writeFile(t, filepath.Join(built, "dist", "client", "assets", "main.js.map"), "{}")
	writeFile(t, filepath.Join(built, "dist", "client", "index.html"), "<html>")
	writeFile(t, filepath.Join(built, webassets.BuildMarker), digest)

	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json "+scionbin.ModulePath+"@"+scionbin.ReleasePin, proc.Result{
		Stdout: `{"Version":"` + testModule + `","Dir":"` + mod + `","Origin":{"VCS":"git","Hash":"` + testCommit + `"}}`})
	f.Script("/opt/go/bin/go build", proc.Result{})
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	writeFile(t, filepath.Join(assets, scionbin.WebAssetName("old")), "stale")
	err = scionbin.GenerateRelease(context.Background(), buildingRunner{f, t}, scionbin.GenerateOptions{
		LeverVersion: testLeverVersion, Pin: scionbin.ReleasePin, AssetDir: assets, ManifestDir: filepath.Join(root, scionbin.ReleaseDir),
		WebUI: func(ctx context.Context, moduleDir, assetDir string) (*scionbin.WebAsset, error) {
			return webassets.GenerateRelease(ctx, f, moduleDir, assetDir, scionbin.ReleasePin)
		},
	})
	if err != nil {
		t.Fatalf("GenerateRelease: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "npm" {
			t.Fatalf("npm ran despite a cached build: %v", c.Args)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(assets, scionbin.WebAssetName("*"))); len(left) != 1 {
		t.Fatalf("web assets = %v, want only the new one", left)
	}
	rel, err := scionbin.LoadRelease(root)
	if err != nil || rel == nil || rel.Web == nil {
		t.Fatalf("LoadRelease: %+v, %v", rel, err)
	}
	archive, err := os.ReadFile(filepath.Join(assets, rel.Web.Name))
	if err != nil {
		t.Fatal(err)
	}

	backendtest.IsolateCache(t)
	srv, _ := serve(t, "/dl/v"+testLeverVersion+"/"+rel.Web.Name, archive)
	scionbin.UseReleaseForTest(t, rel, srv)
	dist, _, err := prebuilt(t)
	if err != nil {
		t.Fatalf("Prebuilt from the generated release: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dist, "index.html")); err != nil || string(b) != "<html>" {
		t.Fatalf("index.html: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dist, "assets", "main.js.map")); !os.IsNotExist(err) {
		t.Fatalf("sourcemap shipped: %v", err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
