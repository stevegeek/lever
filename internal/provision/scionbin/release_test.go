package scionbin_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

const (
	testLeverVersion = "1.2.3"
	testCommit       = "b41cd6fe2bf17c88aca86b54398bf14e4bd04e40"
	testModule       = "v0.3.0-preview.3.0.20261013021256-b41cd6fe2bf1"
)

// fakeScion is an amd64 ELF header padded to a realistic shape, its gzip
// asset, and the manifest pinning both.
type fakeScion struct {
	bin, gz []byte
	rel     *scionbin.Release
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func newFakeScion(t *testing.T, machine uint16) fakeScion {
	t.Helper()
	hdr, err := os.ReadFile(backendtest.WriteELF64(t, t.TempDir(), machine, backendtest.ETExec))
	if err != nil {
		t.Fatal(err)
	}
	bin := append(hdr, bytes.Repeat([]byte("scion"), 4096)...)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	gz := buf.Bytes()
	return fakeScion{bin: bin, gz: gz, rel: &scionbin.Release{
		LeverVersion:  testLeverVersion,
		ScionVersion:  scionbin.ReleasePin,
		ModuleVersion: testModule,
		Commit:        testCommit,
		Assets: map[string]scionbin.Asset{"amd64": {
			Name: "scion_" + scionbin.ReleasePin + "_linux_amd64.gz", SHA256: sum(gz), Size: int64(len(gz)),
			BinarySHA256: sum(bin), BinarySize: int64(len(bin)),
		}},
	}}
}

// assetPath is where the fake release server serves the amd64 asset.
func (f fakeScion) assetPath() string {
	return "/dl/v" + testLeverVersion + "/" + f.rel.Assets["amd64"].Name
}

// serve starts a TLS server that answers the asset path with body and
// counts the requests.
func serve(t *testing.T, path string, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// noGo is a runner on which any `go` command fails, as on a host without Go.
func noGo() *proc.FakeRunner { return proc.NewFakeRunner() }

// buildScripted scripts the source build Resolve falls back to.
func buildScripted() *proc.FakeRunner {
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json", proc.Result{Stdout: `{"Version":"v0","Dir":"/mod/scion"}`})
	f.Script("/opt/go/bin/go build", proc.Result{})
	return f
}

func resolve(t *testing.T, f *proc.FakeRunner, version string) (string, error) {
	t.Helper()
	return scionbin.Resolve(context.Background(), f, scionbin.Spec{Version: version}, "amd64", "m")
}

func TestResolveDownloadsTheReleaseScionWithoutGo(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	srv, n := serve(t, fs.assetPath(), fs.gz)
	scionbin.UseReleaseForTest(t, fs.rel, srv)

	f := noGo()
	out, err := resolve(t, f, scionbin.ReleasePin)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("a release download must not touch Go: %+v", f.Calls)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, fs.bin) {
		t.Fatalf("installed binary differs from the pinned one (err %v)", err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755", fi.Mode())
	}
	if n.Load() != 1 {
		t.Fatalf("requests = %d", n.Load())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".scion-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}

	// The verified binary is reused without the network.
	if _, err := resolve(t, f, scionbin.ReleasePin); err != nil || n.Load() != 1 {
		t.Fatalf("second Resolve: err %v, requests %d", err, n.Load())
	}

	// A cached file that is not the pinned binary is replaced, even at the
	// pinned size.
	bad := bytes.Clone(fs.bin)
	bad[len(bad)-1] ^= 1
	if err := os.WriteFile(out, bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t, f, scionbin.ReleasePin); err != nil || n.Load() != 2 {
		t.Fatalf("Resolve over a changed cache: err %v, requests %d", err, n.Load())
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, fs.bin) {
		t.Fatal("the changed cache was not replaced by the pinned binary")
	}
}

func TestResolveRefusesATamperedDownload(t *testing.T) {
	fs := newFakeScion(t, backendtest.EMX8664)
	same := bytes.Clone(fs.gz)
	same[len(same)/2] ^= 0xff
	cases := map[string][]byte{
		"same size, other bytes": same,
		"one byte longer":        append(bytes.Clone(fs.gz), 0),
		"much larger":            bytes.Repeat([]byte{0}, 10*len(fs.gz)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			srv, _ := serve(t, fs.assetPath(), body)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
			f := buildScripted()
			_, err := resolve(t, f, scionbin.ReleasePin)
			if !errors.Is(err, scionbin.ErrReleaseTampered) || !strings.Contains(err.Error(), scionbin.DownloadEnv+"=off") {
				t.Fatalf("err = %v, want ErrReleaseTampered naming the way out", err)
			}
			if len(f.Calls) != 0 {
				t.Fatalf("a refused download must not fall back to a build: %+v", f.Calls)
			}
			out, _ := scionbin.OutputPath("m")
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("a refused download left %s (err %v)", out, err)
			}
		})
	}
}

// Pinned digests that agree with each other but name a binary for another
// arch are refused too: the ELF check runs on the download.
func TestResolveRefusesAWrongArchDownload(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMAArch64)
	srv, _ := serve(t, fs.assetPath(), fs.gz)
	scionbin.UseReleaseForTest(t, fs.rel, srv)
	if _, err := resolve(t, noGo(), scionbin.ReleasePin); !errors.Is(err, scionbin.ErrReleaseTampered) {
		t.Fatalf("err = %v, want ErrReleaseTampered", err)
	}
}

func TestResolveBuildsWhenTheReleaseIsUnavailable(t *testing.T) {
	fs := newFakeScion(t, backendtest.EMX8664)
	cases := map[string]func(t *testing.T){
		"not found": func(t *testing.T) {
			srv, _ := serve(t, "/elsewhere", fs.gz)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
		},
		"plain http": func(t *testing.T) {
			srv, _ := serve(t, fs.assetPath(), fs.gz)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
			scionbin.UseReleaseBaseForTest(t, strings.Replace(srv.URL, "https://", "http://", 1)+"/dl/")
		},
		"redirect to another host": func(t *testing.T) {
			other, _ := serve(t, fs.assetPath(), fs.gz)
			srv := httptest.NewTLSServer(http.RedirectHandler(other.URL+fs.assetPath(), http.StatusFound))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
		},
		"redirect to http": func(t *testing.T) {
			plain := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(plain.Close)
			srv := httptest.NewTLSServer(http.RedirectHandler(plain.URL+fs.assetPath(), http.StatusFound))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
		},
		"stalled body": func(t *testing.T) {
			scionbin.SetStallTimeoutForTest(t, 100*time.Millisecond)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(fs.gz)))
				_, _ = w.Write(fs.gz[:len(fs.gz)/2])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
		},
		"truncated": func(t *testing.T) {
			// Close-delimited, so the short body ends without a transport error.
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "close")
				w.(http.Flusher).Flush()
				_, _ = w.Write(fs.gz[:len(fs.gz)-1])
			}))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
		},
		"redirect with userinfo": func(t *testing.T) {
			assets, _ := serve(t, fs.assetPath(), fs.gz)
			target := strings.Replace(assets.URL, "https://", "https://user:pw@", 1) + fs.assetPath()
			srv := httptest.NewTLSServer(http.RedirectHandler(target, http.StatusFound))
			t.Cleanup(srv.Close)
			scionbin.UseReleaseForTest(t, fs.rel, srv, assets)
		},
		"server down": func(t *testing.T) {
			srv, _ := serve(t, fs.assetPath(), fs.gz)
			scionbin.UseReleaseForTest(t, fs.rel, srv)
			srv.Close()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			setup(t)
			f := buildScripted()
			if _, err := resolve(t, f, scionbin.ReleasePin); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !f.Called(proc.Subcommand("/opt/go/bin/go", "build")) {
				t.Fatalf("expected the source build as the fallback: %+v", f.Calls)
			}
		})
	}
}

func TestResolveFollowsARedirectToAnAllowedHost(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	assets, n := serve(t, fs.assetPath(), fs.gz)
	front := httptest.NewTLSServer(http.RedirectHandler(assets.URL+fs.assetPath(), http.StatusFound))
	t.Cleanup(front.Close)
	scionbin.UseReleaseForTest(t, fs.rel, front, assets)
	f := noGo()
	if _, err := resolve(t, f, scionbin.ReleasePin); err != nil || n.Load() != 1 || len(f.Calls) != 0 {
		t.Fatalf("err %v, asset requests %d, calls %+v", err, n.Load(), f.Calls)
	}
}

func TestResolveUnavailableWithoutGoNamesBoth(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	srv, _ := serve(t, "/elsewhere", fs.gz)
	scionbin.UseReleaseForTest(t, fs.rel, srv)
	_, err := resolve(t, noGo(), scionbin.ReleasePin)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "Go toolchain") {
		t.Fatalf("err = %v, want the download failure and the missing toolchain", err)
	}
}

func TestResolveDoesNotDownloadOtherVersionsOrWhenTurnedOff(t *testing.T) {
	fs := newFakeScion(t, backendtest.EMX8664)
	for name, c := range map[string]struct{ version, env string }{
		"another version":   {"666333f9", ""},
		"downloads off":     {scionbin.ReleasePin, "off"},
		"downloads 0":       {scionbin.ReleasePin, "0"},
		"no asset for arch": {scionbin.ReleasePin, ""},
	} {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			t.Setenv(scionbin.DownloadEnv, c.env)
			srv, n := serve(t, fs.assetPath(), fs.gz)
			rel := *fs.rel
			if name == "no asset for arch" {
				rel.Assets = map[string]scionbin.Asset{"arm64": {Name: "scion_x_linux_arm64.gz"}}
			}
			scionbin.UseReleaseForTest(t, &rel, srv)
			f := buildScripted()
			if _, err := resolve(t, f, c.version); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if n.Load() != 0 || !f.Called(proc.Subcommand("/opt/go/bin/go", "build")) {
				t.Fatalf("requests %d, calls %+v: want a source build and no download", n.Load(), f.Calls)
			}
			if scionbin.DownloadsRelease(c.version, "amd64") {
				t.Fatal("DownloadsRelease disagrees with Resolve")
			}
		})
	}
}

func TestDownloadsRelease(t *testing.T) {
	fs := newFakeScion(t, backendtest.EMX8664)
	scionbin.UseReleaseForTest(t, fs.rel)
	if !scionbin.DownloadsRelease(scionbin.ReleasePin, "amd64") || scionbin.DownloadsRelease(scionbin.ReleasePin, "arm64") {
		t.Fatal("DownloadsRelease must follow the manifest's assets")
	}
	scionbin.UseReleaseForTest(t, nil)
	if scionbin.DownloadsRelease(scionbin.ReleasePin, "amd64") {
		t.Fatal("a lever without a manifest downloads nothing")
	}
}

func TestReleaseMatches(t *testing.T) {
	rel := newFakeScion(t, backendtest.EMX8664).rel
	for v, want := range map[string]bool{
		scionbin.ReleasePin:             true,
		testModule:                      true,
		testCommit:                      true,
		testCommit[:7]:                  true,
		testCommit[:12]:                 true,
		testCommit[:6]:                  false,
		strings.ToUpper(testCommit[:8]): false,
		"666333f9":                      false,
		"main":                          false,
		"":                              false,
	} {
		if got := rel.Matches(v); got != want {
			t.Errorf("Matches(%q) = %v, want %v", v, got, want)
		}
	}
	var none *scionbin.Release
	if none.Matches(scionbin.ReleasePin) {
		t.Error("a nil release matches nothing")
	}
}

// The production download is https from GitHub's release hosts only.
func TestProductionReleaseSite(t *testing.T) {
	base, hosts := scionbin.ProductionReleaseSite()
	if base != "https://github.com/stevegeek/lever/releases/download/" {
		t.Fatalf("base = %q", base)
	}
	want := []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v", hosts)
	}
	for _, h := range want {
		if !hosts[h] {
			t.Fatalf("hosts = %v, missing %s", hosts, h)
		}
	}
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, scionbin.ReleaseDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scionbin.ReleaseDir, scionbin.ReleaseManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadReleaseValidates(t *testing.T) {
	if rel, err := scionbin.LoadRelease(t.TempDir()); rel != nil || err != nil {
		t.Fatalf("no manifest: %v, %v; want nil, nil", rel, err)
	}
	h := strings.Repeat("a", 64)
	good := `{"lever_version":"1.2.3","scion_version":"b41cd6fe","module_version":"v0","commit":"` + testCommit +
		`","assets":{"amd64":{"name":"scion_b41cd6fe_linux_amd64.gz","sha256":"` + h + `","size":10,"binary_sha256":"` + h + `","binary_size":20}}}`
	if rel, err := scionbin.LoadRelease(writeManifest(t, good)); err != nil || rel == nil {
		t.Fatalf("good manifest: %v", err)
	}
	for _, v := range []string{"1.2.3-rc.1", "1.2.3-rc-1", "1.2.3-SNAPSHOT-abc1234"} {
		if _, err := scionbin.LoadRelease(writeManifest(t, strings.Replace(good, `"1.2.3"`, `"`+v+`"`, 1))); err != nil {
			t.Errorf("version %s refused: %v", v, err)
		}
	}
	if _, err := scionbin.LoadRelease(writeManifest(t, good+"{}")); err == nil {
		t.Error("data after the manifest accepted")
	}
	for name, edit := range map[string][2]string{
		"version with a path":  {`"1.2.3"`, `"1.2.3/../x"`},
		"version with a query": {`"1.2.3"`, `"1.2.3-a?b"`},
		"asset name path":      {`scion_b41cd6fe_linux_amd64.gz`, `../scion_b41cd6fe_linux_amd64.gz`},
		"asset for other arch": {`"amd64":{`, `"arm64":{`},
		"short sha256":         {`"sha256":"` + h, `"sha256":"` + h[:63]},
		"zero size":            {`"size":10`, `"size":0`},
		"huge size":            {`"binary_size":20`, `"binary_size":99999999999`},
		"short commit":         {testCommit, testCommit[:12]},
		"unknown field":        {`"commit"`, `"extra":1,"commit"`},
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

// buildingRunner fakes `go build` by writing an ELF for the GOARCH it is
// given at the -o path.
type buildingRunner struct {
	*proc.FakeRunner
	t *testing.T
}

func (b buildingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := b.FakeRunner.RunIn(ctx, dir, env, name, args...)
	if err == nil && len(args) > 0 && args[0] == "build" {
		for i, a := range args {
			if a == "-o" {
				machine := backendtest.EMX8664
				if env["GOARCH"] == "arm64" {
					machine = backendtest.EMAArch64
				}
				src := backendtest.WriteELF64(b.t, filepath.Dir(args[i+1]), machine, backendtest.ETExec)
				if rerr := os.Rename(src, args[i+1]); rerr != nil {
					return res, rerr
				}
			}
		}
	}
	return res, err
}

// The generator's output is what the download path accepts: build the
// assets, serve them, and resolve from the generated manifest.
func TestGenerateReleaseRoundTrip(t *testing.T) {
	backendtest.IsolateCache(t)
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json "+scionbin.ModulePath+"@"+scionbin.ReleasePin, proc.Result{
		Stdout: `{"Version":"` + testModule + `","Dir":"/mod/scion","Origin":{"VCS":"git","Hash":"` + testCommit + `"}}`})
	f.Script("/opt/go/bin/go build", proc.Result{})
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	manifestDir := filepath.Join(root, scionbin.ReleaseDir)
	err := scionbin.GenerateRelease(context.Background(), buildingRunner{f, t}, scionbin.GenerateOptions{
		LeverVersion: testLeverVersion, Pin: scionbin.ReleasePin, AssetDir: assets, ManifestDir: manifestDir,
	})
	if err != nil {
		t.Fatalf("GenerateRelease: %v", err)
	}
	for _, c := range f.Calls {
		if c.Subcommand("/opt/go/bin/go", "build") {
			if c.Dir != "/mod/scion" || c.Env["GOOS"] != "linux" || c.Env["CGO_ENABLED"] != "0" ||
				!strings.Contains(c.Argv(), "-trimpath -ldflags=-s -w") {
				t.Fatalf("build call = %+v", c)
			}
		}
	}
	rel, err := scionbin.LoadRelease(root)
	if err != nil || rel == nil {
		t.Fatalf("LoadRelease: %v, %v", rel, err)
	}
	if rel.LeverVersion != testLeverVersion || rel.Commit != testCommit || rel.ModuleVersion != testModule || len(rel.Assets) != 2 {
		t.Fatalf("manifest = %+v", rel)
	}
	a := rel.Assets["amd64"]
	gz, err := os.ReadFile(filepath.Join(assets, a.Name))
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, "/dl/v"+testLeverVersion+"/"+a.Name, gz)
	scionbin.UseReleaseForTest(t, rel, srv)
	if _, err := resolve(t, noGo(), scionbin.ReleasePin); err != nil {
		t.Fatalf("Resolve from the generated release: %v", err)
	}
}

func TestGenerateReleaseRemovesAStaleManifestOnFailure(t *testing.T) {
	root := t.TempDir()
	manifestDir := filepath.Join(root, scionbin.ReleaseDir)
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(manifestDir, scionbin.ReleaseManifestFile)
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scionbin.GenerateRelease(context.Background(), proc.NewFakeRunner(), scionbin.GenerateOptions{
		LeverVersion: testLeverVersion, Pin: scionbin.ReleasePin, AssetDir: filepath.Join(root, "a"), ManifestDir: manifestDir,
	})
	if err == nil {
		t.Fatal("GenerateRelease without Go must fail")
	}
	if _, serr := os.Stat(p); !os.IsNotExist(serr) {
		t.Fatalf("stale manifest survived a failed generation: %v", serr)
	}
}

// The binary digests are checked after decompression too: a manifest whose
// gz digest matches but whose binary digest or size does not is refused.
func TestResolveRefusesAnUnpackMismatch(t *testing.T) {
	fs := newFakeScion(t, backendtest.EMX8664)
	for name, edit := range map[string]func(a *scionbin.Asset){
		"other binary digest": func(a *scionbin.Asset) { a.BinarySHA256 = strings.Repeat("0", 64) },
		"larger than pinned":  func(a *scionbin.Asset) { a.BinarySize-- },
		"smaller than pinned": func(a *scionbin.Asset) { a.BinarySize++ },
	} {
		t.Run(name, func(t *testing.T) {
			backendtest.IsolateCache(t)
			rel := *fs.rel
			a := rel.Assets["amd64"]
			edit(&a)
			rel.Assets = map[string]scionbin.Asset{"amd64": a}
			srv, _ := serve(t, fs.assetPath(), fs.gz)
			scionbin.UseReleaseForTest(t, &rel, srv)
			if _, err := resolve(t, noGo(), scionbin.ReleasePin); !errors.Is(err, scionbin.ErrReleaseTampered) {
				t.Fatalf("err = %v, want ErrReleaseTampered", err)
			}
			out, _ := scionbin.OutputPath("m")
			if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatalf("a refused download left %s (err %v)", out, err)
			}
		})
	}
}

// A symlink at the output path is never taken as the cached binary, even
// when it points at the pinned bytes, and its target is left alone.
func TestResolveReplacesASymlinkedCache(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	srv, n := serve(t, fs.assetPath(), fs.gz)
	scionbin.UseReleaseForTest(t, fs.rel, srv)
	out, _ := scionbin.OutputPath("m")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, fs.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, out); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t, noGo(), scionbin.ReleasePin); err != nil || n.Load() != 1 {
		t.Fatalf("err %v, requests %d: want a fresh download", err, n.Load())
	}
	if fi, err := os.Lstat(out); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("output is not a regular file: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, fs.bin) {
		t.Fatal("the symlink's target was changed")
	}
}

// A server that labels the .gz with Content-Encoding: gzip does not get it
// decoded before the digest check.
func TestResolveKeepsTheAssetEncoded(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(fs.gz)
	}))
	t.Cleanup(srv.Close)
	scionbin.UseReleaseForTest(t, fs.rel, srv)
	if _, err := resolve(t, noGo(), scionbin.ReleasePin); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// Cancelling the apply mid-download stops it: no fallback build.
func TestResolveCancelledDownloadDoesNotBuild(t *testing.T) {
	backendtest.IsolateCache(t)
	fs := newFakeScion(t, backendtest.EMX8664)
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(fs.gz)))
		_, _ = w.Write(fs.gz[:10])
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	scionbin.UseReleaseForTest(t, fs.rel, srv)
	f := buildScripted()
	_, err := scionbin.Resolve(ctx, f, scionbin.Spec{Version: scionbin.ReleasePin}, "amd64", "m")
	if !errors.Is(err, context.Canceled) || len(f.Calls) != 0 {
		t.Fatalf("err %v, calls %+v: want context.Canceled and no build", err, f.Calls)
	}
}
