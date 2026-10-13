package remoteproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// jailNC bridges pipes to a TCP address inside the jail. The guest has
// OpenBSD netcat only. No -w: it would also cut idle attach WebSockets.
const jailNC = "nc"

const ncNotFound = 127

// childGrace bounds the waits on the child's exit status and reap, so
// neither hangs the request path.
const childGrace = 2 * time.Second

// stderrLimit caps the kept child stderr, so a guest cannot grow it unbounded.
const stderrLimit = 512

// JailDial returns a DialContext that reaches a TCP address inside this
// instance's jail via `<prefix...> nc <host> <port>`. A host-side
// 127.0.0.1:8080 may be OrbStack forwarding to another instance's jail, so
// each proxy dials through its own.
//
// prefixFn is resolved on every dial so a jail restarted or renamed since
// the proxy started is still found; an empty return fails the dial. The conn
// is live before the guest connects, so a refusal surfaces on the first Read
// with the child's status and stderr.
func JailDial(prefixFn func() []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return nil, fmt.Errorf("jail dial %s: network %q is not supported (the jail transport carries TCP only)", addr, network)
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("jail dial %s: %w", addr, err)
		}
		prefix := prefixFn()
		if len(prefix) == 0 {
			return nil, fmt.Errorf("jail dial %s: cannot resolve this instance's jail — is it up? run `lever up`", addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("jail dial %s: %w", addr, err)
		}
		return startJailConn(ctx, prefix, addr, host, port)
	}
}

// startJailConn ties the child to the conn's own context, cancelled by
// Close or GC cleanup, not to the dial context: a kept-alive conn outlives
// its request.
func startJailConn(dialCtx context.Context, prefix []string, addr, host, port string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(dialCtx))
	// os.Pipe, not StdinPipe: a pipe *os.File is pollable, so deadlines are
	// real. A no-op SetDeadline turns every Transport timeout into a hang.
	inR, inW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("jail dial %s: %w", addr, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		cancel()
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("jail dial %s: %w", addr, err)
	}

	argv := append(append([]string{}, prefix[1:]...), jailNC, host, port)
	cmd := exec.CommandContext(ctx, prefix[0], argv...)
	stderr := &capBuffer{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	// Own process group so endChild reaches a wrapper's helper, which would
	// otherwise hold stdout open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return endChild(cmd) }
	// Bounds Wait if a grandchild inherited stderr and outlived the child.
	cmd.WaitDelay = childGrace

	if err := cmd.Start(); err != nil {
		cancel()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("jail dial %s: cannot run %q on the host: %w", addr, prefix[0], err)
	}
	// Close our ends, or the pipes never reach EOF.
	inR.Close()
	outW.Close()

	// Separate wait state so the goroutine does not keep the conn reachable
	// for the cleanup.
	w := &childWait{done: make(chan struct{})}
	c := &jailConn{in: inW, out: outR, stderr: stderr, prefix: prefix, addr: addr,
		cancel: cancel, wait: w}
	go func() {
		w.err = cmd.Wait() // written before done closes; read only after
		close(w.done)
	}()
	// For a conn dropped without Close; the cancel func holds no conn reference.
	runtime.AddCleanup(c, func(stop context.CancelFunc) { stop() }, cancel)
	return c, nil
}

// endChild signals the process group only after Kill confirmed the child
// was not yet reaped: after the reap the pid may be reused. Returning
// os.ErrProcessDone stops exec injecting an error.
func endChild(cmd *exec.Cmd) error {
	p := cmd.Process
	if p == nil {
		return os.ErrProcessDone
	}
	if err := p.Kill(); err != nil {
		return err
	}
	// Residual race: reap and pid reuse between Kill and here needs a full
	// pid-space wrap in microseconds. Not signalling the group would leave a
	// helper holding the pipes.
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	return nil
}

type jailConn struct {
	in     *os.File // parent → child stdin
	out    *os.File // child stdout → parent
	stderr *capBuffer
	prefix []string
	addr   string

	cancel context.CancelFunc // ends the child, via exec's reap-guarded Cancel
	wait   *childWait

	closeOnce sync.Once
	closed    atomic.Bool
}

// childWait is separate from jailConn so the wait goroutine does not pin it.
type childWait struct {
	done chan struct{}
	err  error // written before done closes; read only after
}

var _ net.Conn = (*jailConn)(nil)

func (c *jailConn) Read(p []byte) (int, error) {
	n, err := c.out.Read(p)
	return n, c.translate(err)
}

func (c *jailConn) Write(p []byte) (int, error) {
	n, err := c.in.Write(p)
	return n, c.translate(err)
}

// translate replaces a pipe-level error ("EOF", "broken pipe") with why the
// jail side failed, if it did. Deadline errors and errors after Close are
// this side's doing and pass through.
func (c *jailConn) translate(err error) error {
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if c.closed.Load() {
		// net.Conn callers test for net.ErrClosed, not os.ErrClosed.
		if errors.Is(err, os.ErrClosed) {
			return net.ErrClosed
		}
		return err
	}
	select {
	case <-c.wait.done:
	case <-time.After(childGrace):
		return err // still running: the pipe error is the whole story
	}
	if c.wait.err == nil {
		return err
	}
	return c.diagnose()
}

func (c *jailConn) diagnose() error {
	via := strings.Join(c.prefix, " ")
	said := strings.TrimSpace(c.stderr.String())
	quoted := ""
	if said != "" {
		quoted = ": " + said
	}
	if c.ncMissing(said) {
		return fmt.Errorf("jail dial %s via %q: %s is missing from the jail — install it there "+
			"(apt-get install -y netcat-openbsd) and restart the proxy: %v%s",
			c.addr, via, jailNC, c.wait.err, quoted)
	}
	return fmt.Errorf("jail dial %s via %q: %w%s", c.addr, via, c.wait.err, quoted)
}

// ncMissing: exit 127, or "command not found" for a prefix that reports a
// failed exec its own way. Not "machine not found" (`orb -m <gone>`), which
// `lever up` fixes.
func (c *jailConn) ncMissing(stderr string) bool {
	var ee *exec.ExitError
	if errors.As(c.wait.err, &ee) && ee.ExitCode() == ncNotFound {
		return true
	}
	return strings.Contains(strings.ToLower(stderr), "command not found")
}

func (c *jailConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.in.Close()
		c.out.Close()
		// exec's watchCtx may still Cancel a reaped child; endChild's Kill guard
		// keeps that safe.
		c.cancel()
		// Reap, or each connection leaks a zombie; bounded because Close is on the
		// request path.
		select {
		case <-c.wait.done:
		case <-time.After(childGrace):
		}
	})
	return nil
}

func (c *jailConn) SetDeadline(t time.Time) error {
	return errors.Join(c.out.SetReadDeadline(t), c.in.SetWriteDeadline(t))
}

func (c *jailConn) SetReadDeadline(t time.Time) error  { return c.out.SetReadDeadline(t) }
func (c *jailConn) SetWriteDeadline(t time.Time) error { return c.in.SetWriteDeadline(t) }

func (c *jailConn) LocalAddr() net.Addr  { return jailAddr{"jail-dial"} }
func (c *jailConn) RemoteAddr() net.Addr { return jailAddr{c.addr} }

type jailAddr struct{ addr string }

func (a jailAddr) Network() string { return "jail" }
func (a jailAddr) String() string  { return a.addr }

// capBuffer is guarded: exec's copier writes from its own goroutine.
type capBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := stderrLimit - len(b.buf); n > 0 {
		b.buf = append(b.buf, p[:min(n, len(p))]...)
	}
	// A short write would make exec tear down the child's stderr.
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
