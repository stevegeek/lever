package remoteproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes this test binary act as the guest-side nc.
const helperEnv = "LEVER_TEST_JAILDIAL_HELPER"

// helperAddrEnv redirects the helper, so Target can name an address nothing
// listens on and only the jail transport passes.
const helperAddrEnv = "LEVER_TEST_JAILDIAL_ADDR"

// TestJailDialHelperProcess is not a test: with helperEnv set it acts as
// the guest's nc, and exits itself so no PASS line enters the HTTP stream.
func TestJailDialHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	code := 0
	defer func() { os.Exit(code) }()

	args := os.Args
	if len(args) < 2 {
		code = 2
		return
	}
	addr := net.JoinHostPort(args[len(args)-2], args[len(args)-1])
	if override := os.Getenv(helperAddrEnv); override != "" {
		addr = override
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		_, _ = os.Stderr.WriteString("nc: " + err.Error() + "\n")
		code = 1
		return
	}
	defer c.Close()
	go func() {
		_, _ = io.Copy(c, os.Stdin)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	_, _ = io.Copy(os.Stdout, c)
}

func helperPrefix(t *testing.T) []string {
	t.Helper()
	t.Setenv(helperEnv, "1")
	return []string{os.Args[0], "-test.run=^TestJailDialHelperProcess$", "--"}
}

func staticPrefix(argv ...string) func() []string {
	return func() []string { return argv }
}

// shPrefix runs script through sh; JailDial's `nc host port` lands in $1..$3.
func shPrefix(script string) []string {
	return []string{"sh", "-c", script, "jail"}
}

func dialTest(t *testing.T, prefix []string, addr string) (net.Conn, error) {
	t.Helper()
	return JailDial(staticPrefix(prefix...))(context.Background(), "tcp", addr)
}

func TestJailDialRoundTripsBytes(t *testing.T) {
	c, err := dialTest(t, shPrefix("exec cat"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if line != "ping\n" {
		t.Fatalf("read %q, want %q", line, "ping\n")
	}
}

func TestJailDialCloseReapsChild(t *testing.T) {
	c, err := dialTest(t, shPrefix("exec cat"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	jc, ok := c.(*jailConn)
	if !ok {
		t.Fatalf("conn is %T, want *jailConn", c)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// wait.done closes only after cmd.Wait, so the child was reaped.
	select {
	case <-jc.wait.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close returned but the child was never reaped")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after close = %v, want net.ErrClosed", err)
	}
}

func TestJailDialMissingBinaryIsActionable(t *testing.T) {
	_, err := dialTest(t, []string{"lever-no-such-jail-binary"}, "127.0.0.1:8080")
	if err == nil {
		t.Fatal("dial with a nonexistent prefix binary should fail")
	}
	if !strings.Contains(err.Error(), "lever-no-such-jail-binary") {
		t.Errorf("error should name the missing binary, got: %v", err)
	}
}

func TestJailDialMissingNCIsActionable(t *testing.T) {
	c, err := dialTest(t, shPrefix("echo 'nc: command not found' >&2; exit 127"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_, err = io.ReadAll(c)
	if err == nil {
		t.Fatal("reading from a conn whose child died should fail, not report EOF")
	}
	msg := err.Error()
	for _, want := range []string{"nc", "netcat-openbsd", "127.0.0.1:8080"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestJailDialChildFailureNamesTheCause(t *testing.T) {
	c, err := dialTest(t, shPrefix("echo 'nc: connect to 127.0.0.1 port 8080 (tcp) failed: Connection refused' >&2; exit 1"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_, err = io.ReadAll(c)
	if err == nil {
		t.Fatal("a child that exits non-zero must surface as an error, not EOF")
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("error should quote the child's stderr, got: %v", err)
	}
}

func TestJailDialCleanExitIsEOF(t *testing.T) {
	// A hub closing is a normal end: plain io.EOF, or every completed
	// response would look like a transport failure.
	c, err := dialTest(t, shPrefix("exit 0"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("read after a clean child exit = %v, want EOF", err)
	}
}

func TestJailDialUnresolvedPrefixIsActionable(t *testing.T) {
	_, err := JailDial(func() []string { return nil })(context.Background(), "tcp", "127.0.0.1:8080")
	if err == nil {
		t.Fatal("an unresolvable jail should fail the dial")
	}
	if !strings.Contains(err.Error(), "lever up") {
		t.Errorf("error should tell the operator how to fix it, got: %v", err)
	}
}

func TestJailDialRejectsNonTCP(t *testing.T) {
	_, err := JailDial(staticPrefix("sh"))(context.Background(), "udp", "127.0.0.1:8080")
	if err == nil || !strings.Contains(err.Error(), "udp") {
		t.Fatalf("udp dial = %v, want a rejection naming the network", err)
	}
}

func TestJailDialHonorsReadDeadline(t *testing.T) {
	// A no-op deadline would hang here on `cat`.
	c, err := dialTest(t, shPrefix("exec cat"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	_, err = c.Read(make([]byte, 16))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read = %v, want a deadline-exceeded error", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read error must satisfy net.Error with Timeout() true, got %T %v", err, err)
	}
}

func TestJailDialRemoteAddrNamesTheJailTarget(t *testing.T) {
	c, err := dialTest(t, shPrefix("exec cat"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got := c.RemoteAddr().String(); got != "127.0.0.1:8080" {
		t.Errorf("RemoteAddr() = %q, want the jail-side target", got)
	}
	if c.LocalAddr() == nil {
		t.Error("LocalAddr() must not be nil")
	}
}

func TestJailDialCarriesHTTPKeepAlive(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok:" + r.URL.Path))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	tr := &http.Transport{DialContext: JailDial(staticPrefix(helperPrefix(t)...))}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	for _, path := range []string{"/one", "/two"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "ok:"+path {
			t.Fatalf("body = %q, want %q", body, "ok:"+path)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("server saw %d connections, want 1 — the conn is not keep-alive-clean", n)
	}
}

// TestJailDialCarriesWebSocketUpgrade: a real 101 and two-way exchange over
// a jailConn through NewHandler; fails if Transport is swapped for a
// RoundTripper that breaks upgrades.
func TestJailDialCarriesWebSocketUpgrade(t *testing.T) {
	var gotAuth, gotCookie string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade request", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if err := buf.Flush(); err != nil {
			return
		}
		line, err := buf.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer hub.Close()

	// Target names a dead address, so a handler keeping the default dialer
	// fails here.
	t.Setenv(helperAddrEnv, mustURL(t, hub.URL).Host)
	proxy := httptest.NewServer(NewHandler(Config{
		Target:      mustURL(t, "http://127.0.0.1:1"),
		Session:     testSession(),
		ServeHost:   "mac.ts.net",
		DialContext: JailDial(staticPrefix(helperPrefix(t)...)),
	}))
	defer proxy.Close()

	c, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	_, err = c.Write([]byte("GET /api/v1/agents/x/attach HTTP/1.1\r\nHost: mac.ts.net\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Bearer attacker\r\nCookie: scion_sess=evil\r\n\r\n"))
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if h == "\r\n" {
			break
		}
	}
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write after upgrade: %v", err)
	}
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}
	if echo != "echo:ping\n" {
		t.Fatalf("tunnelled reply = %q, want %q", echo, "echo:ping\n")
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q — the client's header must not survive the upgrade path", gotAuth)
	}
	if gotCookie != sessionCookieName+"="+testCookie {
		t.Errorf("Cookie = %q through the upgrade path, want only the injected session", gotCookie)
	}
}

// TestEndChildRefusesAfterReap: after the reap the pid may belong to an
// unrelated process group.
func TestEndChildRefusesAfterReap(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if err := endChild(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("endChild after reap = %v, want os.ErrProcessDone — signalling a reaped pid can hit an unrelated process", err)
	}
}

func TestEndChildEndsALiveChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := endChild(cmd); err != nil {
		t.Fatalf("endChild on a live child = %v, want nil", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the child exited cleanly — endChild did not end it")
	}
}

func TestJailDialCloseAfterChildExited(t *testing.T) {
	c, err := dialTest(t, shPrefix("exit 0"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	jc := c.(*jailConn)
	select {
	case <-jc.wait.done:
	case <-time.After(5 * time.Second):
		t.Fatal("child never exited")
	}
	if jc.wait.err != nil {
		t.Fatalf("waitErr = %v, want nil for a clean exit", jc.wait.err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if jc.wait.err != nil {
		t.Fatalf("waitErr = %v after Close — Close must not re-signal a reaped child", jc.wait.err)
	}
}

func TestJailDialMissingMachineIsNotDiagnosedAsMissingNC(t *testing.T) {
	c, err := dialTest(t, shPrefix("echo 'orb: machine not found' >&2; exit 1"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_, err = io.ReadAll(c)
	if err == nil {
		t.Fatal("a child that exits non-zero must surface as an error")
	}
	if strings.Contains(err.Error(), "netcat-openbsd") {
		t.Errorf("a missing machine must not be diagnosed as a missing nc, got: %v", err)
	}
	if !strings.Contains(err.Error(), "machine not found") {
		t.Errorf("error should quote what the jail transport actually said, got: %v", err)
	}
}

// TestJailDialDroppedConnEndsItsChild: a conn dropped without Close must
// not leak a jail process for the life of the proxy.
func TestJailDialDroppedConnEndsItsChild(t *testing.T) {
	// Keep only the wait channel, so the conn becomes unreachable.
	done := func() chan struct{} {
		c, err := dialTest(t, shPrefix("exec cat"), "127.0.0.1:8080")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c.(*jailConn).wait.done
	}()

	for range 50 {
		runtime.GC()
		select {
		case <-done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("a dropped conn left its child running")
}
