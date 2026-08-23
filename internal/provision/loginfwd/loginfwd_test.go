package loginfwd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lexec "github.com/stevegeek/lever/internal/exec"
)

func fakeRunner() *lexec.FakeRunner         { return lexec.NewFakeRunner() }
func fakeResult(stdout string) lexec.Result { return lexec.Result{Stdout: stdout} }

// buildForwarderForHost compiles the EMBEDDED forwarder source for the machine
// running the test, exactly the way Build compiles it for the
// guest (same source, same module file, same offline build). It is what proves
// the embedded program is real Go and does what it claims — a string constant
// that no longer compiles would otherwise only surface on a live apply.
func buildForwarderForHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	bin := filepath.Join(dir, "lever-login-forward")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the embedded login forwarder does not build: %v\n%s", err, out)
	}
	return bin
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

// TestLoginForwarderCarriesTrafficToTheTarget runs the real binary: this is
// the whole contract the guest half has to meet.
func TestLoginForwarderCarriesTrafficToTheTarget(t *testing.T) {
	bin := buildForwarderForHost(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "reached %s", r.URL.Path)
	}))
	defer target.Close()

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-listen", fmt.Sprintf("127.0.0.1:%d", port),
		"-target", strings.TrimPrefix(target.URL, "http://"))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	waitForListener(t, fmt.Sprintf("127.0.0.1:%d", port))

	// Several requests, because the hub makes three in a row (discovery,
	// /token, /userinfo) and a forwarder that serves one connection and stops
	// would pass a single-request test.
	for _, path := range []string{"/.well-known/openid-configuration", "/token", "/userinfo"} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatalf("GET %s through the forwarder: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "reached "+path {
			t.Fatalf("GET %s = %q", path, body)
		}
	}
}

// TestLoginForwarderRefusesANonLoopbackListen: off loopback it would publish
// the provider's endpoints to the guest's network rather than its loopback.
func TestLoginForwarderRefusesANonLoopbackListen(t *testing.T) {
	bin := buildForwarderForHost(t)
	for _, listen := range []string{"0.0.0.0:9500", "192.168.1.10:9500", "9500", ""} {
		args := []string{"-target", "host.orb.internal:9500"}
		if listen != "" {
			args = append(args, "-listen", listen)
		}
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err == nil {
			t.Fatalf("-listen %q was accepted; it must fail closed", listen)
		}
		if !strings.Contains(string(out), "lever-login-forward:") {
			t.Fatalf("-listen %q: %s", listen, out)
		}
	}
}

// TestBuildCrossCompilesWithTheResolvedGo pins the argv: the absolute go
// binary, an offline module build, and -trimpath for a reproducible hash.
func TestBuildCrossCompilesWithTheResolvedGo(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f := fakeRunner()
	f.Script("go env GOROOT", fakeResult("/opt/go\n"))
	f.Script("/opt/go/bin/go build", fakeResult(""))
	out, err := Build(context.Background(), f, "arm64", "m")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if filepath.Base(out) != "lever-login-forward" || !strings.Contains(out, "lever-loginfwd-m") {
		t.Fatalf("output path %q", out)
	}
	for _, name := range []string{"main.go", "go.mod"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(out), name)); err != nil {
			t.Fatalf("%s not staged: %v", name, err)
		}
	}
	build := f.Calls[len(f.Calls)-1]
	if build.Name != "/opt/go/bin/go" || strings.Join(build.Args, " ") != "build -trimpath -o "+out+" ." {
		t.Fatalf("build call = %+v", build)
	}
	if build.Dir != filepath.Dir(out) {
		t.Fatalf("build ran in %q, want %q", build.Dir, filepath.Dir(out))
	}
	for k, v := range map[string]string{"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0", "GOPROXY": "off", "GOFLAGS": "-mod=mod"} {
		if build.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, build.Env[k], v)
		}
	}
}

func TestBuildFailsWithoutGo(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if _, err := Build(context.Background(), fakeRunner(), "arm64", "m"); err == nil {
		t.Fatal("expected an error when go is not resolvable")
	}
}
