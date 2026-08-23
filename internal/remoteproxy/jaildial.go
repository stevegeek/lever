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
// Deliberately no -w: openbsd nc applies that timeout to an IDLE connection as
// well as to connect, which would cut the hub's long-lived attach WebSockets
// mid-session.
const jailNC = "nc"

// ncNotFound is the shell's exit code for an absent command — the same signal
// internal/hubapi/jailcurl.go reads for a missing curl.
const ncNotFound = 127

// childGrace bounds two waits on the child: how long an I/O error waits for
// its exit status before reporting the raw pipe error, and how long Close
// waits for the reap. Only failure paths pay it — by the time either runs the
// child has been killed or has already closed the pipe — and neither may hang
// the proxy's request path.
const childGrace = 2 * time.Second

// stderrLimit caps how much of the child's stderr is kept for the error
// message. A per-connection buffer that grows with whatever the guest writes
// is a memory leak a remote client can trigger, and an error should quote a
// hint rather than a transcript.
const stderrLimit = 512

// JailDial returns a DialContext that reaches a TCP address INSIDE this
// instance's jail, by running `<prefix...> nc <host> <port>` on the host and
// adapting the child's stdin/stdout to a net.Conn.
//
// The host has no direct route to the hub: the hub binds the JAIL's loopback,
// and a host-side 127.0.0.1:8080 that appears to reach it is an artifact of
// OrbStack's port forwarding from whichever machine claimed the port first —
// not necessarily this instance's (internal/scion/client.go states the rule;
// internal/hubapi/jailcurl.go is the same rule applied to lever's other hub
// calls). Dialing through the jail is what lets two remote-enabled instances
// share a host: each proxy reaches its OWN hub instead of contending for one
// forwarded port.
//
// prefixFn returns the backend's jail argv prefix, e.g.
// ["orb","-m","lever-x","-u","stephen"] or ["limactl","shell","lever-x"]. It
// is a func rather than a value so a jail that was down, rebuilt, or renamed
// when the proxy started is resolved on a later dial instead of pinned at
// construction; an empty return means "cannot resolve the jail right now" and
// fails that dial with an actionable message.
//
// The returned conn is live before the guest's TCP connect completes: nothing
// is written to stdout until the hub answers, so a refused connection cannot
// surface as a dial error. It surfaces on the first Read instead, carrying the
// child's exit status and stderr (see jailConn.translate) rather than a bare
// EOF.
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
// The child's lifetime is governed by a context of the conn's own, cancelled
// by Close (and by the GC cleanup below), NOT by the dial context. A conn
// outlives the request that dialled it — that is what keep-alive is — and
// net/http hands DialContext a context it has already detached from the
// request for exactly that reason (Transport.getConn: "the dial should
// proceed even if the request is canceled"). Context values are carried
// through; only cancellation is ours to decide.
func startJailConn(dialCtx context.Context, prefix []string, addr, host, port string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(dialCtx))
	// os.Pipe rather than cmd.StdinPipe/StdoutPipe: those hand back plain
	// io.ReadCloser/io.WriteCloser, and a conn whose SetDeadline is a no-op
	// turns every timeout in http.Transport into a hang. An *os.File over a
	// pipe is pollable, so its deadlines are real.
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
	// Own process group so endChild can reach a wrapper's helper. Killing only
	// the direct child leaves such a helper holding the stdout pipe open, and a
	// conn that never sees EOF is a stuck proxy goroutine.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return endChild(cmd) }
	// Bound Wait even if something the child spawned inherited its stderr and
	// outlived it: past this, exec closes the pipes and stops waiting, so the
	// wait goroutine cannot be pinned open by a process lever does not know
	// about.
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

	// The wait state is its own object rather than fields on the conn, so the
	// goroutine below captures IT and not the conn. A goroutine holding the
	// conn would keep it reachable for as long as the child lives, which is
	// exactly when the cleanup registered further down needs it collectable.
	w := &childWait{done: make(chan struct{})}
	c := &jailConn{in: inW, out: outR, stderr: stderr, prefix: prefix, addr: addr,
		cancel: cancel, wait: w}
	go func() {
		w.err = cmd.Wait() // written before done closes; read only after
		close(w.done)
	}()
	// Last resort for a conn dropped without Close: cancelling is the entire
	// cleanup, and the cancel func holds no reference back to the conn, so
	// this cannot keep the conn itself alive. Close remains the real path —
	// this only stops a caller's mistake from leaking a jail process forever.
	runtime.AddCleanup(c, func(stop context.CancelFunc) { stop() }, cancel)
	return c, nil
}

// endChild ends cmd's process. It signals the process GROUP only once
// os.Process.Kill has confirmed the direct child was still ours to signal:
// after the wait goroutine reaps it, the pid is free for the OS to hand to an
// unrelated process, and a raw kill(-pid) would then hit whatever group later
// holds it. os.Process guards against that; a bare syscall does not. The proxy
// starts one child per connection against a pid space of a few tens of
// thousands, so pid reuse is reachable, not theoretical.
//
// Returning os.ErrProcessDone is meaningful to exec: it treats it as "the
// process already finished" and injects no error (see Cmd.watchCtx).
func endChild(cmd *exec.Cmd) error {
	p := cmd.Process
	if p == nil {
		return os.ErrProcessDone
	}
	if err := p.Kill(); err != nil {
		return err
	}
	// Honest residual: Kill has returned by now, so the wait goroutine could
	// reap and the OS recycle the pid before this line runs. That needs a full
	// pid-space wrap inside a few microseconds. Go offers nothing tighter —
	// os.Process has no group-signal API to inherit its pidfd guarantees — and
	// the alternative, not signalling the group, leaves a wrapper's helper
	// holding the pipes open.
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

// childWait carries the child's exit status from the wait goroutine to
// whoever needs to explain a failed read or write. Separate from jailConn so
// the goroutine does not pin the conn — see startJailConn.
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

// translate replaces a pipe-level error with the reason the JAIL side failed,
// when there is one. At the pipe, a missing nc, a stopped machine, and a hub
// refusing the connection all look identical — "EOF" or "broken pipe" — and a
// 502 with no cause is exactly what this transport must not produce. A child
// that exited cleanly leaves the error alone: that is an ordinary end of
// stream, not a failure.
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

// diagnose renders the child's failure as something an operator can act on.
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
// Exit 127 is the shell's own signal for it; the wording counts too, since a
// prefix binary may report a failed exec with a code of its own.
//
// The wording test is deliberately the full "command not found" rather than a
// bare "not found": `orb -m <gone>` says "machine not found", and diagnosing a
// destroyed or renamed machine as a missing netcat sends the operator to
// apt-get for a problem `lever up` fixes.
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
		// Ending the child goes through exec's context machinery rather than a
		// direct signal. Note what that does NOT buy: exec's watchCtx selects
		// between handing its result to Wait and the context being done, so
		// Cancel can still fire on a child Wait has already reaped. What keeps
		// that safe is the Kill guard inside endChild — do not remove it on
		// the belief that exec has already ruled the case out.
		c.cancel()
		// Wait for the reap: the proxy opens one child per connection, so a
		// Close that returns before the child is waited on leaks a zombie per
		// request. Bounded because Close runs on the request path — past the
		// grace the wait goroutine still reaps, just not before Close returns.
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

// jailAddr names the jail-side endpoint a jailConn reaches. net.Conn's
// contract wants a net.Addr, but there is no host-side socket to describe —
// the bytes travel over pipes to a process — so this reports where the
// connection actually went rather than inventing a plausible host address.
type jailAddr struct{ addr string }

func (a jailAddr) Network() string { return "jail" }
func (a jailAddr) String() string  { return a.addr }

// capBuffer keeps at most stderrLimit bytes of the child's stderr. Guarded
// because exec's copier writes it from its own goroutine while a Read on this
// side may be building an error message.
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
	// Report a full write regardless: dropping the tail is this buffer's
	// choice, and a short write would make exec tear the child's stderr down.
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
