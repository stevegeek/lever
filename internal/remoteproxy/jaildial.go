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

// jailNC is the command run INSIDE the jail to bridge a pair of pipes to a TCP
// address. The guest carries OpenBSD netcat; socat and ncat are not installed.
// No -w: openbsd nc applies that timeout to an idle connection too, which
// would cut the hub's long-lived attach WebSockets.
const jailNC = "nc"

// ncNotFound is the shell's exit code for an absent command.
const ncNotFound = 127

// childGrace bounds two waits on the child: how long an I/O error waits for
// its exit status, and how long Close waits for the reap. Neither may hang
// the proxy's request path.
const childGrace = 2 * time.Second

// stderrLimit caps how much of the child's stderr is kept for the error
// message, so a guest cannot grow a per-connection buffer without bound.
const stderrLimit = 512

// JailDial returns a DialContext that reaches a TCP address INSIDE this
// instance's jail, by running `<prefix...> nc <host> <port>` on the host and
// adapting the child's stdin/stdout to a net.Conn.
//
// The hub binds the jail's loopback. A host-side 127.0.0.1:8080 that appears
// to reach it is OrbStack's port forwarding from whichever machine claimed
// the port first, not necessarily this instance's, so each proxy dials
// through its own jail.
//
// prefixFn returns the backend's jail argv prefix, e.g.
// ["orb","-m","lever-x","-u","devuser"] or ["limactl","shell","lever-x"]. It
// is resolved on every dial so a jail that was down, rebuilt or renamed when
// the proxy started is still found; an empty return fails that dial.
//
// The returned conn is live before the guest's TCP connect completes, so a
// refused connection surfaces on the first Read, carrying the child's exit
// status and stderr (see jailConn.translate) rather than a bare EOF.
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

// startJailConn runs the jail command and wires its pipes into a net.Conn.
//
// The child's lifetime follows a context of the conn's own, cancelled by
// Close (and by the GC cleanup below), NOT the dial context: a kept-alive
// conn outlives the request that dialled it, and net/http already detaches
// the dial context from the request. Context values are carried through.
func startJailConn(dialCtx context.Context, prefix []string, addr, host, port string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(dialCtx))
	// os.Pipe rather than cmd.StdinPipe/StdoutPipe: an *os.File over a pipe is
	// pollable, so its deadlines are real. A conn whose SetDeadline is a no-op
	// turns every timeout in http.Transport into a hang.
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
	// Own process group so endChild can reach a wrapper's helper, which would
	// otherwise hold the stdout pipe open and leave a conn that never sees EOF.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return endChild(cmd) }
	// Bound Wait even if something the child spawned inherited its stderr and
	// outlived it.
	cmd.WaitDelay = childGrace

	if err := cmd.Start(); err != nil {
		cancel()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("jail dial %s: cannot run %q on the host: %w", addr, prefix[0], err)
	}
	// The child holds the other ends now; ours must go, or the pipes never
	// reach EOF when the child exits.
	inR.Close()
	outW.Close()

	// The wait state is its own object so the goroutine below does not keep
	// the conn reachable while the child lives: the cleanup below needs the
	// conn collectable.
	w := &childWait{done: make(chan struct{})}
	c := &jailConn{in: inW, out: outR, stderr: stderr, prefix: prefix, addr: addr,
		cancel: cancel, wait: w}
	go func() {
		w.err = cmd.Wait() // written before done closes; read only after
		close(w.done)
	}()
	// Last resort for a conn dropped without Close; the cancel func holds no
	// reference back to the conn, so this cannot keep it alive.
	runtime.AddCleanup(c, func(stop context.CancelFunc) { stop() }, cancel)
	return c, nil
}

// endChild ends cmd's process. It signals the process GROUP only once
// os.Process.Kill has confirmed the direct child was still ours to signal:
// after the reap the pid may belong to an unrelated process, and with one
// child per connection pid reuse is reachable.
//
// Returning os.ErrProcessDone tells exec the process already finished, so
// it injects no error (see Cmd.watchCtx).
func endChild(cmd *exec.Cmd) error {
	p := cmd.Process
	if p == nil {
		return os.ErrProcessDone
	}
	if err := p.Kill(); err != nil {
		return err
	}
	// Residual race: the child could be reaped and its pid recycled between
	// Kill and this line, which needs a full pid-space wrap in microseconds.
	// os.Process has no group-signal API, and not signalling the group would
	// leave a wrapper's helper holding the pipes open.
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	return nil
}

// jailConn adapts a running `<prefix> nc host port` child to net.Conn.
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

// childWait carries the child's exit status from the wait goroutine to a
// failed read or write. Separate from jailConn so the goroutine does not pin
// the conn.
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

// translate replaces a pipe-level error with the reason the jail side failed,
// when there is one: at the pipe, a missing nc, a stopped machine and a hub
// refusing the connection all look like "EOF" or "broken pipe". A child that
// exited cleanly leaves the error alone.
//
// Deadline errors and errors after Close pass through: those are this side's
// doing, so the child's exit status would misattribute them.
func (c *jailConn) translate(err error) error {
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if c.closed.Load() {
		// net.Conn callers (http.Transport included) test for net.ErrClosed,
		// not os.ErrClosed, on a conn they closed themselves.
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

// ncMissing reports whether the child died because the guest has no netcat.
// Exit 127 is the shell's signal for it; the wording counts too, since a
// prefix binary may report a failed exec with a code of its own. It matches
// the full "command not found": `orb -m <gone>` says "machine not found",
// which `lever up` fixes, not a netcat install.
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
		// exec's watchCtx can still fire Cancel on a child Wait has already
		// reaped; the Kill guard inside endChild is what keeps that safe.
		c.cancel()
		// Wait for the reap, or each connection leaks a zombie. Bounded because
		// Close runs on the request path; the wait goroutine still reaps later.
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

// jailAddr names the jail-side endpoint a jailConn reaches: there is no
// host-side socket to describe.
type jailAddr struct{ addr string }

func (a jailAddr) Network() string { return "jail" }
func (a jailAddr) String() string  { return a.addr }

// capBuffer keeps at most stderrLimit bytes of the child's stderr. Guarded
// because exec's copier writes it from its own goroutine.
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
	// Report a full write: a short write would make exec tear the child's
	// stderr down.
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
