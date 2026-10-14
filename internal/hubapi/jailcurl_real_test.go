package hubapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// sizedServer answers /cl/N with N bytes and a Content-Length, /chunked/N
// with N bytes and no Content-Length (chunked), and /api/v1/projects with 16
// KiB chunked.
func sizedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/v1/projects" {
			path = "/chunked/16384"
		}
		parts := strings.Split(path, "/")
		n, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if parts[1] == "cl" {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		body := []byte(strings.Repeat("x", n))
		for len(body) > 0 {
			k := min(len(body), 1024)
			if _, err := w.Write(body[:k]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			body = body[k:]
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// needCurl also clears the proxy variables, which would route the request to
// the test server elsewhere.
func needCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
}

// The real script and curl stop a reply over the call's cap whether or not
// the server sent a Content-Length, and pass one of exactly the cap.
func TestJailCurlScriptCapsARealReply(t *testing.T) {
	needCurl(t)
	srv := sizedServer(t)
	j := &JailCurl{Runner: proc.RealRunner{}, BaseURL: srv.URL, Token: func() string { return "t" }}
	ctx := WithReplyCap(context.Background(), 8<<10)
	for _, kind := range []string{"cl", "chunked"} {
		path := "/" + kind + "/" + strconv.Itoa(8<<10)
		status, body, err := j.Do(ctx, "GET", path)
		if err != nil || status != 200 || len(body) != 8<<10 {
			t.Fatalf("%s at the cap: status=%d len=%d err=%v", kind, status, len(body), err)
		}
		for _, n := range []int{8<<10 + 1, 4 << 20} {
			path := "/" + kind + "/" + strconv.Itoa(n)
			_, _, err := j.Do(ctx, "GET", path)
			assertTooLarge(t, err, "GET "+path, "8 KiB")
		}
	}

	a := &AssertCurl{Runner: proc.RealRunner{}, BaseURL: srv.URL, Sign: func() (string, error) { return "sig", nil }}
	_, _, err := a.Do(WithReplyCap(context.Background(), 8<<10), "GET", "/api/v1/projects")
	assertTooLarge(t, err, "GET /api/v1/projects", "8 KiB")
	if _, _, err := a.Do(WithReplyCap(context.Background(), 16<<10), "GET", "/api/v1/projects"); err != nil {
		t.Fatalf("assertion transport at the cap: %v", err)
	}
}

// pathRunner runs every command with dir first on PATH.
type pathRunner struct{ dir string }

func (p pathRunner) env(env map[string]string) map[string]string {
	out := map[string]string{"PATH": p.dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for k, v := range env {
		out[k] = v
	}
	return out
}

func (p pathRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return proc.RealRunner{}.Run(ctx, p.env(env), name, args...)
}

func (p pathRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return proc.RealRunner{}.RunIn(ctx, dir, p.env(env), name, args...)
}

func (p pathRunner) RunStdin(ctx context.Context, in io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return proc.RealRunner{}.RunStdin(ctx, in, p.env(env), name, args...)
}

// A curl that ignores --max-filesize (one that checks only a Content-Length)
// is stopped by the runner's output limit, and what it wrote never reads as
// a reply. The fake writes without end, so only the output limit can stop it
// before the test's deadline.
func TestJailCurlOutputLimitStopsACurlThatIgnoresTheCap(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := "#!/bin/sh\ntr '\\0' x </dev/zero\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	j := &JailCurl{Runner: pathRunner{dir: dir}, BaseURL: "http://127.0.0.1:9", Token: func() string { return "t" }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err := j.Do(ctx, "GET", "/api/v1/agents")
	assertTooLarge(t, err, "GET /api/v1/agents", "1 MiB")
}
