package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort releases the port it finds, so another process could take it
// first; production has the same race.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("freePort: close: %v", err)
	}
	return port
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestServeWritesAndRemovesPIDFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "remote.pid")
	port := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeConfig{Port: port, Handler: okHandler(), PIDPath: pidPath})
	}()

	waitFor(t, 2*time.Second, func() bool { return fileExists(pidPath) })

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if perm := statPerm(t, pidPath); perm != 0o600 {
		t.Errorf("pid file mode = %#o, want 0600", perm)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file content = %q, want this process's pid %d", data, os.Getpid())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}

	if fileExists(pidPath) {
		t.Error("pid file was not removed after ctx cancel")
	}
}

// TestServeStampsWhatThisProcessIsServing: Stamp runs after the bind and
// the pid file, since brokerctl keys its record on that pid.
func TestServeStampsWhatThisProcessIsServing(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "remote.pid")
	port := freePort(t)

	type observation struct {
		pid    string
		listen bool
	}
	seen := make(chan observation, 4)
	stamp := func() error {
		b, _ := os.ReadFile(pidPath)
		// Connect, not request: Serve may not be handling yet.
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()
		}
		seen <- observation{pid: strings.TrimSpace(string(b)), listen: err == nil}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeConfig{Port: port, Handler: okHandler(), PIDPath: pidPath, Stamp: stamp})
	}()

	select {
	case got := <-seen:
		if got.pid != strconv.Itoa(os.Getpid()) {
			t.Errorf("Stamp ran with pid file %q, want this process's pid %d — it must run AFTER the pid file is written",
				got.pid, os.Getpid())
		}
		if !got.listen {
			t.Error("Stamp ran before the port was bound — a proxy that never took the port must not leave a record")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve never called Stamp")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
	if len(seen) != 0 {
		t.Errorf("Stamp ran %d extra times, want exactly once per serve", len(seen))
	}
}

// TestServeDoesNotStampWhenTheBindFails: the losing proxy must leave the
// incumbent's record alone.
func TestServeDoesNotStampWhenTheBindFails(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	pidPath1 := filepath.Join(dir, "remote1.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, ServeConfig{Port: port, Handler: okHandler(), PIDPath: pidPath1}) }()
	waitFor(t, 2*time.Second, func() bool { return fileExists(pidPath1) })

	var stamped bool
	err := Serve(context.Background(), ServeConfig{
		Port: port, Handler: okHandler(), PIDPath: filepath.Join(dir, "remote2.pid"),
		Stamp: func() error { stamped = true; return nil },
	})
	if err == nil {
		t.Fatal("second Serve on an already-bound port should error")
	}
	if stamped {
		t.Error("a Serve that could not bind must not record anything about what is running")
	}
}

// TestServeKeepsServingWhenStampFails: the record is bookkeeping, not a
// precondition for serving.
func TestServeKeepsServingWhenStampFails(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeConfig{Port: port, Handler: okHandler(),
			Stamp: func() error { return errors.New("state dir is read-only") }})
	}()

	var resp *http.Response
	waitFor(t, 2*time.Second, func() bool {
		r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return false
		}
		resp = r
		return true
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — a failed stamp must not affect serving", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after a failed stamp, want a clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

func TestServeSecondOnSamePortErrors(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	pidPath1 := filepath.Join(dir, "remote1.pid")
	pidPath2 := filepath.Join(dir, "remote2.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeConfig{Port: port, Handler: okHandler(), PIDPath: pidPath1})
	}()
	waitFor(t, 2*time.Second, func() bool { return fileExists(pidPath1) })

	err := Serve(context.Background(), ServeConfig{Port: port, Handler: okHandler(), PIDPath: pidPath2})
	if err == nil {
		t.Fatal("second Serve on an already-bound port should error")
	}
	if fileExists(pidPath2) {
		t.Error("second Serve must not have written a pid file after a bind failure")
	}
}

func TestServeFailsClosedOnNonLoopbackAddr(t *testing.T) {
	// Unreachable through Serve's inputs, so test the loopback check directly.
	nonLoopback := &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 12345}
	if isLoopbackAddr(nonLoopback) {
		t.Fatal("0.0.0.0 must not be treated as loopback")
	}
	loopback := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	if !isLoopbackAddr(loopback) {
		t.Fatal("127.0.0.1 must be treated as loopback")
	}
}

func TestOpenAuditCreatesFileWithMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-audit.jsonl")

	fn, closer, err := OpenAudit(path)
	if err != nil {
		t.Fatalf("OpenAudit: %v", err)
	}
	defer closer.Close()

	if perm := statPerm(t, path); perm != 0o600 {
		t.Errorf("audit file mode = %#o, want 0600", perm)
	}
	_ = fn
}

func TestOpenAuditWritesMarshaledLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-audit.jsonl")

	fn, closer, err := OpenAudit(path)
	if err != nil {
		t.Fatalf("OpenAudit: %v", err)
	}

	when := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	fn(AuditLine{Time: when, TSLogin: "op@example.com", Method: "GET", Path: "/api/v1/agents", Decision: "allow", Status: 200})
	fn(AuditLine{Time: when, TSLogin: "op@example.com", Method: "GET", Path: "/api/v1/other", Decision: "deny-user", Status: 403})

	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), data)
	}
	var first AuditLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 not JSON: %v: %s", err, lines[0])
	}
	if first.Path != "/api/v1/agents" || first.Decision != "allow" || first.Status != 200 {
		t.Errorf("line 1 = %+v", first)
	}
	var second AuditLine
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 not JSON: %v: %s", err, lines[1])
	}
	if second.Path != "/api/v1/other" || second.Decision != "deny-user" {
		t.Errorf("line 2 = %+v", second)
	}
}

func TestOpenAuditAppendsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-audit.jsonl")

	fn1, closer1, err := OpenAudit(path)
	if err != nil {
		t.Fatalf("OpenAudit: %v", err)
	}
	fn1(AuditLine{Path: "/first", Decision: "allow"})
	if err := closer1.Close(); err != nil {
		t.Fatal(err)
	}

	fn2, closer2, err := OpenAudit(path)
	if err != nil {
		t.Fatalf("OpenAudit (reopen): %v", err)
	}
	fn2(AuditLine{Path: "/second", Decision: "allow"})
	if err := closer2.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/first") || !strings.Contains(string(data), "/second") {
		t.Fatalf("expected both lines preserved across reopen, got: %q", data)
	}
}

func TestOpenAuditRejectsUnwritableDir(t *testing.T) {
	_, _, err := OpenAudit(filepath.Join(t.TempDir(), "nosuchdir", "audit.jsonl"))
	if err == nil {
		t.Fatal("expected an error opening an audit path in a nonexistent directory")
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("expected the error to wrap an *os.PathError, got %v", err)
	}
}

func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestServerTimeouts: the two zeros are the point. WriteTimeout would cut
// streamed transcripts and ReadTimeout would make the header deadline
// cover the whole request.
func TestServerTimeouts(t *testing.T) {
	srv := newServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if srv.ReadHeaderTimeout != serverReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, serverReadHeaderTimeout)
	}
	if srv.IdleTimeout != serverIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, serverIdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — it would cut streamed responses", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 — it would make the header deadline bound the whole request", srv.ReadTimeout)
	}
	if srv.Handler == nil {
		t.Error("Handler must be wired")
	}
}

// TestHeaderAndIdleTimeoutsDoNotBoundTheBody checks the stdlib property
// newServer relies on: neither timeout bounds a response being written.
func TestHeaderAndIdleTimeoutsDoNotBoundTheBody(t *testing.T) {
	const tick = 80 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: tick, // both far shorter than the stream below
		IdleTimeout:       tick,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			rc := http.NewResponseController(w)
			for i := range 5 {
				fmt.Fprintf(w, "chunk%d\n", i)
				_ = rc.Flush()
				time.Sleep(tick)
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed body: %v", err)
	}
	if got := string(body); got != "chunk0\nchunk1\nchunk2\nchunk3\nchunk4\n" {
		t.Fatalf("streamed body = %q — a timeout cut the response mid-stream", got)
	}
}
