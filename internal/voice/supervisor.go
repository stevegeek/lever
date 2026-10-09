package voice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Supervisor runs whisper-server as a child of the remote proxy, bound to
// host loopback, restarts it with backoff when it exits, and stops it when
// its context ends. The jail never reaches the port: config validation
// refuses it in every allow_ports, so the egress rules drop it.
//
// The child's stdout and stderr are discarded, never logged: whisper.cpp
// prints what it transcribes (whisper.go, assumption 5). Log gets lever's
// own lines only (start, exit status, backoff).
type Supervisor struct {
	Program string
	Args    []string
	Port    int
	// Env is the child's environment; nil = ChildEnv(os.Environ()).
	Env []string
	// Log receives lever's own lines; nil drops them.
	Log func(format string, a ...any)
	// Check, when set, runs before every start: a failure skips the start
	// and is retried after the backoff (Service checks the program again,
	// so a restart never runs a file someone replaced).
	Check func() error
	// Zero values take the defaults below (tests shorten them).
	MinBackoff, MaxBackoff, StableAfter, StopGrace, ProbeEvery time.Duration

	ready   atomic.Bool
	blocked atomic.Bool // the last start found the port taken, or Check failed
}

const (
	defaultMinBackoff  = time.Second
	defaultMaxBackoff  = time.Minute
	defaultStableAfter = time.Minute // a run this long resets the backoff
	// defaultStopGrace is short: `lever stop` SIGKILLs a proxy that has not
	// exited 2 s after its SIGTERM, and the child must be gone by then
	// (off Linux nothing else kills it). whisper-server keeps no state worth
	// a graceful stop.
	defaultStopGrace  = time.Second
	defaultProbeEvery = 250 * time.Millisecond
)

// Addr is the child's address.
func (s *Supervisor) Addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)) }

// Ready reports whether the child is running and accepting connections.
func (s *Supervisor) Ready() bool { return s.ready.Load() }

// Blocked reports whether the last attempt could not start the child: the
// port was taken by another process, or Check failed.
func (s *Supervisor) Blocked() bool { return s.blocked.Load() }

func (s *Supervisor) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

func dur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// childEnvKeys are what the child keeps of the proxy's environment: what a
// GPU build needs to find its libraries and devices, and nothing that could
// carry a credential.
var childEnvKeys = []string{"PATH", "HOME", "TMPDIR", "LANG", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH"}

// childEnvPrefixes likewise, by prefix.
var childEnvPrefixes = []string{"LC_", "CUDA_", "GGML_", "NVIDIA_", "HIP_", "ROCR_", "VK_"}

// ChildEnv is environ reduced to childEnvKeys and childEnvPrefixes.
func ChildEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		keep := false
		for _, want := range childEnvKeys {
			keep = keep || k == want
		}
		for _, p := range childEnvPrefixes {
			keep = keep || strings.HasPrefix(k, p)
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// portTaken reports whether something already listens on the port: then the
// child is not started, and nothing is sent there (it is not lever's).
func (s *Supervisor) portTaken() bool {
	c, err := net.DialTimeout("tcp", s.Addr(), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Run supervises the child until ctx ends, then stops it (SIGTERM, then
// SIGKILL after StopGrace) and returns once it has exited.
func (s *Supervisor) Run(ctx context.Context) {
	minB, maxB := dur(s.MinBackoff, defaultMinBackoff), dur(s.MaxBackoff, defaultMaxBackoff)
	backoff := minB
	wait := func() bool {
		t := time.NewTimer(backoff)
		defer t.Stop()
		backoff = min(backoff*2, maxB)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		if s.Check != nil {
			if err := s.Check(); err != nil {
				s.blocked.Store(true)
				s.logf("voice: whisper-server not started: %v (retrying in %s)", err, backoff)
				if !wait() {
					return
				}
				continue
			}
		}
		if s.portTaken() {
			s.blocked.Store(true)
			s.logf("voice: 127.0.0.1:%d is already in use by another process; whisper-server not started, dictation unavailable (retrying in %s)", s.Port, backoff)
			if !wait() {
				return
			}
			continue
		}
		s.blocked.Store(false)
		ran, err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if ran >= dur(s.StableAfter, defaultStableAfter) {
			backoff = minB
		}
		s.logf("voice: whisper-server exited after %s (%v); restarting in %s", ran.Round(time.Millisecond), err, backoff)
		if !wait() {
			return
		}
	}
}

// command is the child's command: Stdin, Stdout and Stderr nil (the null
// device, never a log: whisper.cpp prints transcripts), a reduced
// environment, and Pdeathsig on Linux.
func (s *Supervisor) command() *exec.Cmd {
	cmd := exec.Command(s.Program, s.Args...)
	cmd.Env = s.Env
	if cmd.Env == nil {
		cmd.Env = ChildEnv(os.Environ())
	}
	cmd.SysProcAttr = childSysProcAttr()
	return cmd
}

// runOnce starts the child once and waits for it to exit or ctx to end.
func (s *Supervisor) runOnce(ctx context.Context) (time.Duration, error) {
	cmd := s.command()
	//
	// On Linux the child gets Pdeathsig, which the kernel ties to the thread
	// that started it: this goroutine keeps that thread until the child is
	// gone, so the signal fires only when the proxy dies.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	s.logf("voice: whisper-server started on %s (pid %d)", s.Addr(), cmd.Process.Pid)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	probeCtx, stopProbe := context.WithCancel(ctx)
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		s.probe(probeCtx, cmd.Process.Pid)
	}()
	defer func() {
		stopProbe()
		<-probed
		s.ready.Store(false)
	}()
	select {
	case err := <-exited:
		return time.Since(start), exitText(err)
	case <-ctx.Done():
		s.ready.Store(false)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		t := time.NewTimer(dur(s.StopGrace, defaultStopGrace))
		defer t.Stop()
		select {
		case <-exited:
		case <-t.C:
			_ = cmd.Process.Kill()
			<-exited
		}
		s.logf("voice: whisper-server stopped")
		return time.Since(start), nil
	}
}

// probe marks the child (pid) ready once its port accepts a connection
// and, where the platform can tell, the listener is the child's own.
func (s *Supervisor) probe(ctx context.Context, pid int) {
	tick := time.NewTicker(dur(s.ProbeEvery, defaultProbeEvery))
	defer tick.Stop()
	warned := false
	for {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		c, err := d.DialContext(dctx, "tcp", s.Addr())
		cancel()
		if err == nil {
			c.Close()
			owned, known := listenerOwnedBy(pid, s.Port)
			if known && !owned {
				// Another process took the port while the child loaded its
				// model: nothing is sent there. The child fails to bind and
				// exits, and the next start finds the port taken.
				if !warned {
					warned = true
					s.logf("voice: 127.0.0.1:%d answers, but not from whisper-server (pid %d): another process holds it; nothing is sent there", s.Port, pid)
				}
			} else if ctx.Err() == nil {
				s.ready.Store(true)
				s.logf("voice: whisper-server ready on %s", s.Addr())
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func exitText(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%s", ee.ProcessState.String())
	}
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}
