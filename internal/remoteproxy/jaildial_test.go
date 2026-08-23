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

// helperEnv switches this test binary into "guest-side nc" mode. A dial's
// prefix can then be the test binary itself, which makes the whole transport
// testable without orb, limactl, or a real nc on the host.
const helperEnv = "LEVER_TEST_JAILDIAL_HELPER"

// helperAddrEnv redirects the helper to a fixed address instead of the one
// JailDial appended. It lets a test point Target at an address nothing
// listens on, so the test can only pass if the request really travelled over
// the jail transport.
const helperAddrEnv = "LEVER_TEST_JAILDIAL_ADDR"

// TestJailDialHelperProcess is not a test: re-executed with helperEnv set, it
// impersonates the `nc <host> <port>` that JailDial runs inside the jail —
// connect to the named address, then shuttle stdin/stdout over it. It exits
// the process itself so the testing package never prints its own PASS line
// into the stream the parent is reading as HTTP.
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
	// JailDial appends `nc <host> <port>`, so the target is always the last
	// two arguments regardless of what the prefix itself carried.
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

// helperPrefix is a jail prefix that re-executes this test binary as the
// guest-side nc (see TestJailDialHelperProcess).
func helperPrefix(t *testing.T) []string {
	t.Helper()
	t.Setenv(helperEnv, "1")
	return []string{os.Args[0], "-test.run=^TestJailDialHelperProcess$", "--"}
}

// staticPrefix wraps a fixed argv so tests can hand JailDial a prefix without
// a resolver.
func staticPrefix(argv ...string) func() []string {
	return func() []string { return argv }
}

// shPrefix builds a prefix that runs script through sh, swallowing the
// `nc host port` arguments JailDial appends (they land in $1..$3).
func shPrefix(script string) []string {
	return []string{"sh", "-c", script, "jail"}
}

func dialTest(t *testing.T, prefix []string, addr string) (net.Conn, error) {
	t.Helper()
	return JailDial(staticPrefix(prefix...))(context.Background(), "tcp", addr)
}

func TestJailDialRoundTripsBytes(t *testing.T) {
	// `exec cat` echoes stdin to stdout: proof that Write reaches the child's
	// stdin and Read drains its stdout.
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
	// wait.done closes only after cmd.Wait returned, so the child was reaped
	// (no zombie per connection).
	select {
	case <-jc.wait.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close returned but the child was never reaped")
	}
	// Close is idempotent: the proxy can close a conn it has already closed.
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
	// Exit 127 with the shell's own wording is exactly what a guest without
	// netcat produces.
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
	// A hub that closes the connection is a normal end of stream: the child
	// exits 0 and the conn must report plain io.EOF, or every completed
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
	// `cat` with nothing written blocks forever; a conn whose deadlines were
	// a silent no-op would hang here instead of timing out.
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

// TestJailDialCarriesHTTPKeepAlive drives a real http.Transport over the jail
// transport, with the test binary standing in for the guest's nc. Two
// sequential requests on one connection prove the conn is well-behaved enough
// for keep-alive, which is what the proxy actually needs.
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

// TestJailDialCarriesWebSocketUpgrade drives a real 101 Switching Protocols
// handshake and a bidirectional exchange over a jailConn, through the full
// NewHandler stack. This is the hub's attach stream: without it, the jail
// transport's WebSocket claim rests on stdlib reasoning alone, and a later
// swap of http.Transport for a hand-rolled RoundTripper would break attach
// with every other test still green.
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
		// Echo one line back, proving bytes move in both directions after
		// the switch rather than the tunnel merely opening.
		line, err := buf.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer hub.Close()

	// The prefix is the test binary standing in for the guest's nc, so the
	// tunnel really does run over a jailConn's pipes. Target names an address
	// nothing listens on and the helper is redirected to the hub, so a
	// handler that quietly kept the default dialer fails outright here rather
	// than passing by reaching the hub the ordinary way.
	t.Setenv(helperAddrEnv, mustURL(t, hub.URL).Host)
	proxy := httptest.NewServer(NewHandler(Config{
		Target:      mustURL(t, "http://127.0.0.1:1"),
		PAT:         func() string { return "scion_pat_ws" },
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
	// The upgrade path must not become a hole in the credential rules.
	if gotAuth != "Bearer scion_pat_ws" {
		t.Errorf("Authorization = %q — PAT injection must apply to upgrades too", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("Cookie leaked through the upgrade path: %q", gotCookie)
	}
}

// TestEndChildRefusesAfterReap is the pid-reuse guard. Once the wait
// goroutine reaps the child, its pid is free for the OS to hand to an
// unrelated process — so a raw kill(-pid) at that point signals whatever
// process group later holds it. The proxy starts one child per connection, so
// this is reachable rather than theoretical. endChild must decline instead.
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

// TestEndChildEndsALiveChild: declining after the reap must not turn into
// declining always — a live child still has to die, or Close leaks one jail
// process per connection.
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

// TestJailDialCloseAfterChildExited: the ordinary path. The hub closes, the
// child exits, the wait goroutine reaps it, and only then does the proxy
// close the conn. That Close must be quiet and must not disturb the recorded
// exit.
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

// TestJailDialMissingMachineIsNotDiagnosedAsMissingNC: `orb -m <gone>` says
// "machine not found". Reading that as a missing netcat sends the operator to
// apt-get for something `lever up` fixes, so the hint must key on the
// shell's full "command not found", not a bare "not found".
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

// TestJailDialDroppedConnEndsItsChild: Close is the real path, but a caller
// that drops a conn without closing it must not leak a jail process (and its
// wait goroutine) for the life of the proxy.
func TestJailDialDroppedConnEndsItsChild(t *testing.T) {
	// Take only the wait channel, so the conn itself becomes unreachable here.
	// It closes only after cmd.Wait returned, so the child was waited on.
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
