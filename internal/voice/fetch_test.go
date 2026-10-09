package voice

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loopbackOnly lets a download reach only the test servers named.
func loopbackOnly(ok ...*httptest.Server) func(*url.URL) bool {
	return func(u *url.URL) bool {
		for _, s := range ok {
			if u.Scheme+"://"+u.Host == s.URL {
				return true
			}
		}
		return false
	}
}

func serve(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fetch-") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

func TestFetchVerifiesAndPlaces(t *testing.T) {
	const content = "model bytes"
	srv := serve(content)
	defer srv.Close()
	m := testModel(content)
	dir := filepath.Join(t.TempDir(), "models")
	var progress strings.Builder
	p, err := Fetcher{AllowHost: loopbackOnly(srv), Progress: &progress}.FetchFrom(context.Background(), m, srv.URL+"/x", dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 || p != ModelPath(dir, m) {
		t.Fatalf("%s %v %v", p, fi.Mode(), err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v", di.Mode())
	}
	if !strings.Contains(progress.String(), "100%") {
		t.Fatalf("progress %q", progress.String())
	}
	if err := Verify(p, m); err != nil {
		t.Fatal(err)
	}
	// A verified file is not downloaded again.
	srv.Close()
	if _, err := (Fetcher{AllowHost: loopbackOnly(srv)}).FetchFrom(context.Background(), m, srv.URL+"/x", dir); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	assertNoTemp(t, dir)
}

func TestFetchRefusesWrongBytes(t *testing.T) {
	for name, body := range map[string]string{"hash": "model bytez", "short": "model", "long": "model bytes and more"} {
		t.Run(name, func(t *testing.T) {
			srv := serve(body)
			defer srv.Close()
			dir := filepath.Join(t.TempDir(), "m")
			_, err := Fetcher{AllowHost: loopbackOnly(srv)}.FetchFrom(context.Background(), testModel("model bytes"), srv.URL, dir)
			if err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "ggml-tiny.bin")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("a wrong file was placed")
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestFetchReplacesAWrongFile(t *testing.T) {
	const content = "model bytes"
	srv := serve(content)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "m")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := testModel(content)
	if err := os.WriteFile(ModelPath(dir, m), []byte("model byteZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Fetcher{AllowHost: loopbackOnly(srv)}).FetchFrom(context.Background(), m, srv.URL, dir); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ModelPath(dir, m), m); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRefusesUnpinned(t *testing.T) {
	m := testModel("x")
	m.SHA256 = Unverified
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	_, err := Fetcher{AllowHost: loopbackOnly(srv)}.FetchFrom(context.Background(), m, srv.URL, filepath.Join(t.TempDir(), "m"))
	if !errors.Is(err, ErrUnpinned) || hit {
		t.Fatalf("err = %v, request made %v", err, hit)
	}
	// The real table: an entry that is still UNVERIFIED is refused before
	// any request.
	for _, real := range Models {
		if real.Pinned() == "" {
			continue
		}
		if _, err := (Fetcher{}).Fetch(context.Background(), real, t.TempDir()); !errors.Is(err, ErrUnpinned) {
			t.Fatalf("%s: err = %v", real.Name, err)
		}
	}
}

func TestFetchRedirects(t *testing.T) {
	const content = "model bytes"
	target := serve(content)
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/file", http.StatusFound)
	}))
	defer redirect.Close()
	m := testModel(content)
	// To a host that is not allowed: refused.
	_, err := Fetcher{AllowHost: loopbackOnly(redirect)}.FetchFrom(context.Background(), m, redirect.URL, filepath.Join(t.TempDir(), "m"))
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("err = %v", err)
	}
	// To an allowed one: followed.
	if _, err := (Fetcher{AllowHost: loopbackOnly(redirect, target)}).FetchFrom(context.Background(), m, redirect.URL, filepath.Join(t.TempDir(), "m")); err != nil {
		t.Fatal(err)
	}
	// The first URL is checked too.
	if _, err := (Fetcher{AllowHost: loopbackOnly(target)}).FetchFrom(context.Background(), m, redirect.URL, filepath.Join(t.TempDir(), "m")); err == nil {
		t.Fatal("a start URL on another host was fetched")
	}
}

func TestHuggingFaceHost(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://huggingface.co/ggerganov/whisper.cpp/resolve/x/y": true,
		"https://cdn-lfs.huggingface.co/repos/x":                   true,
		"https://cdn-lfs-us-1.hf.co/repos/x":                       true,
		"https://cas-bridge.xethub.hf.co/x":                        true,
		"https://HUGGINGFACE.CO/x":                                 true,
		"https://huggingface.co:443/x":                             true,
		"http://huggingface.co/x":                                  false,
		"https://huggingface.co:8443/x":                            false,
		"https://user@huggingface.co/x":                            false,
		"https://huggingface.co.evil.test/x":                       false,
		"https://evilhuggingface.co/x":                             false,
		"https://hf.co.evil.test/x":                                false,
		"https://evil.test/huggingface.co":                         false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := HuggingFaceHost(u); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	m := testModel("abc")
	p := ModelPath(dir, m)
	if err := Verify(p, m); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if err := os.WriteFile(p, []byte("abd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, m); !errors.Is(err, ErrModel) {
		t.Fatalf("wrong hash: %v", err)
	}
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, m); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, m); !errors.Is(err, ErrModel) {
		t.Fatalf("world-writable: %v", err)
	}
	os.Remove(p)
	other := filepath.Join(dir, "real")
	if err := os.WriteFile(other, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, m); !errors.Is(err, ErrModel) {
		t.Fatalf("symlink: %v", err)
	}
	unpinned := m
	unpinned.Revision = Unverified
	if err := Verify(other, unpinned); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("unpinned: %v", err)
	}
}

func TestPrivateDirRefusesOpenOrLinked(t *testing.T) {
	root := t.TempDir()
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(open); err == nil {
		t.Fatal("a 0755 dir was accepted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(link); err == nil {
		t.Fatal("a link was accepted")
	}
}

func TestCheckServer(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "whisper-server")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	if real, err := CheckServer(prog); err != nil || real != prog {
		t.Fatalf("%s %v", real, err)
	}
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(prog, link); err != nil {
		t.Fatal(err)
	}
	if real, err := CheckServer(link); err != nil || real != prog {
		t.Fatalf("through a link: %s %v", real, err)
	}
	if _, err := CheckServer("whisper-server"); err == nil {
		t.Fatal("a relative path was accepted")
	}
	for mode, what := range map[os.FileMode]string{0o775: "group-writable", 0o644: "not executable"} {
		if err := os.Chmod(prog, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckServer(prog); err == nil {
			t.Fatalf("a %s program was accepted", what)
		}
	}
	if err := os.Chmod(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if _, err := CheckServer(prog); err == nil {
		t.Fatal("a program in a world-writable directory was accepted")
	}
}
