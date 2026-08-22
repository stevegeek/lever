package remoteproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ServeConfig configures Serve.
type ServeConfig struct {
	// Port is the loopback port to bind: Serve always binds
	// "127.0.0.1:<Port>", never an operator-supplied address, so the
	// fail-closed loopback check below is a belt-and-braces invariant
	// rather than a live attack surface — see the check's comment.
	Port int
	// Handler is the pre-built proxy handler (NewHandler's return value).
	// Its Audit callback, if any, must already be wired by the caller
	// BEFORE Serve is called: Handler is an opaque http.Handler, so Serve
	// has no way to reach back into it and rewire its audit sink.
	Handler http.Handler
	// PIDPath, when non-empty, is where Serve records its own pid — written
	// once the listener is bound (so a pid file on disk means the proxy is,
	// or was, actually listening) and removed on shutdown.
	PIDPath string
	// Stamp, when non-nil, records what THIS process is serving. Serve calls
	// it once, after the listeners are bound and PIDPath is written, and
	// before the first request can be handled.
	//
	// The running proxy has to be what writes that record, which is why this
	// hook exists at all. `lever apply` decides whether to reuse a running
	// proxy or restart it by comparing a host-side stamp
	// (brokerctl.State.WriteRemoteStamp) — but apply is not the only thing
	// that starts proxies, while the pid file it reads is written by every
	// one of them. A proxy started by hand against a different config used to
	// inherit the stamp apply had left, so apply reported success against a
	// process enforcing a config it had never seen. Only the process itself
	// knows what it is running.
	//
	// Ordering is part of the contract in both directions: after the bind, so
	// a proxy that could not take the port leaves the incumbent's record
	// alone; after the pid file, so an implementation may key its record on
	// the pid it finds there (brokerctl's does).
	//
	// An error is a warning, not a failed serve — a proxy that is up and
	// working must not be taken down over a bookkeeping file. That is only
	// safe while the implementation leaves NO record behind when it fails:
	// no stamp costs the next apply a redundant restart, a stale one costs it
	// the whole check. brokerctl.State.WriteRemoteStamp removes the file on
	// every failure path for this reason.
	Stamp func() error
	// AuditPath, when non-empty, is opened by Serve via OpenAudit for the
	// life of the serve, guaranteeing the audit JSONL exists (0600) as soon
	// as the proxy starts and is closed cleanly on shutdown. This is
	// independent of Handler's own Audit wiring (see the Handler field
	// doc): the caller's separate OpenAudit call is what actually receives
	// per-request AuditLines, since Handler is already built by the time
	// Serve runs. Passing the same path here just means Serve co-owns the
	// file's lifecycle rather than leaving it entirely to the caller.
	AuditPath string
	// Provider, when non-nil, is the local OIDC provider, served on its OWN
	// loopback listener (127.0.0.1:Provider.Port()) for the life of the
	// serve.
	//
	// A second listener rather than extra routes on the proxy's own, because
	// the two have opposite audiences: the proxy answers the operator's
	// browser through `tailscale serve`, while the provider answers the hub's
	// back channel, which arrives from inside the jail through the guest
	// forwarder. One shared listener would publish the provider's endpoints
	// to the tailnet and the proxy's to the jail — each reachable by a caller
	// that has no business with it.
	Provider *Provider
}

// Serve runs the proxy until ctx is cancelled: bind 127.0.0.1:<Port> (fail
// closed on any non-loopback listen address), bind the provider's own
// loopback port when one is configured, open the audit JSONL (append, 0600)
// when AuditPath is set, write the pid file, record what this process is
// serving (Stamp), and serve. On ctx.Done it shuts
// both servers down gracefully, removes the pid file, and returns. Mirrors
// brokerctl.Serve's bind → pid → serve → remove-pid ordering
// (internal/brokerctl/serve.go).
func Serve(ctx context.Context, cfg ServeConfig) error {
	ln, err := listenLoopback(cfg.Port)
	if err != nil {
		return err
	}
	// Both listeners are bound before anything else, so a port collision
	// fails the serve outright instead of leaving a proxy up whose login path
	// can never work.
	var provLn net.Listener
	if cfg.Provider != nil {
		provLn, err = listenLoopback(cfg.Provider.Port())
		if err != nil {
			_ = ln.Close()
			return err
		}
	}

	if cfg.AuditPath != "" {
		_, auditCloser, err := OpenAudit(cfg.AuditPath)
		if err != nil {
			closeListeners(ln, provLn)
			return fmt.Errorf("remoteproxy: open audit: %w", err)
		}
		defer auditCloser.Close()
	}

	if err := writePIDFile(cfg.PIDPath); err != nil {
		closeListeners(ln, provLn)
		return err
	}
	defer removePIDFile(cfg.PIDPath)

	if cfg.Stamp != nil {
		if err := cfg.Stamp(); err != nil {
			fmt.Fprintf(os.Stderr, "lever: warning: could not record what this proxy is serving: %v\n"+
				"lever: the next `lever apply` will restart the proxy rather than reuse it\n", err)
		}
	}

	// Each goroutine closes over its OWN *http.Server, never over the slice:
	// appending the provider rewrites the slice header while the proxy's
	// goroutine is already running, which is a genuine data race on that
	// variable (caught by -race as soon as a test configured a provider). The
	// slice is the shutdown list, and only this goroutine touches it.
	proxySrv := newServer(cfg.Handler)
	servers := []*http.Server{proxySrv}
	serveErr := make(chan error, 2)
	go func() { serveErr <- proxySrv.Serve(ln) }()
	if provLn != nil {
		prov := newServer(cfg.Provider.Handler())
		servers = append(servers, prov)
		go func() { serveErr <- prov.Serve(provLn) }()
	}

	shutdownAll := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var first error
		for _, s := range servers {
			if err := s.Shutdown(shutdownCtx); err != nil && first == nil {
				first = fmt.Errorf("remoteproxy: shutdown: %w", err)
			}
		}
		return first
	}

	select {
	case <-ctx.Done():
		if err := shutdownAll(); err != nil {
			return err
		}
		// Wait for every Serve to actually return before the listeners are
		// considered released.
		for range servers {
			<-serveErr
		}
		return nil
	case err := <-serveErr:
		// One of them stopped on its own. Take the other down too: a proxy
		// with no provider cannot log in, and a provider with no proxy serves
		// nobody, so half-running is never the state to leave behind.
		_ = shutdownAll()
		for range servers[1:] {
			<-serveErr
		}
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("remoteproxy: serve: %w", err)
		}
		return nil
	}
}

// listenLoopback binds 127.0.0.1:<port> and fails closed on anything but a
// loopback address.
//
// The proxy's Tailscale-User-Login gate is only as strong as "nothing but
// `tailscale serve` can reach this listener" (see proxy.go's package doc): a
// non-loopback listener would let any LAN/tailnet peer reach it directly and
// set that header itself, bypassing the gate. The provider's listener carries
// the same requirement for a different reason — off loopback it would be an
// unauthenticated identity endpoint on the network.
func listenLoopback(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("remoteproxy: bind: %w", err)
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || !isLoopbackAddr(ta) {
		_ = ln.Close()
		return nil, fmt.Errorf("remoteproxy: listener must be loopback, got %s", ln.Addr())
	}
	return ln, nil
}

// closeListeners closes every non-nil listener, for the failure paths between
// binding and serving.
func closeListeners(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

// serverReadHeaderTimeout bounds how long a client may take to send its
// request headers, and serverIdleTimeout how long a kept-alive connection may
// sit unused between requests.
const (
	serverReadHeaderTimeout = 20 * time.Second
	serverIdleTimeout       = 120 * time.Second
)

// newServer builds the proxy's http.Server.
//
// Two timeouts, and deliberately not the other two. Defence in depth rather
// than a live need — the listener is loopback-only behind `tailscale serve` —
// so the bar is "cannot break a legitimate session", and only these two clear
// it:
//
//   - ReadHeaderTimeout covers the header phase alone. Once the headers are
//     read, conn.readRequest resets the read deadline to ReadTimeout's
//     deadline, which is zero here, so nothing bounds the rest.
//   - IdleTimeout applies only between requests on a kept-alive connection.
//
// WriteTimeout and ReadTimeout must NOT be added. WriteTimeout is armed on the
// raw connection before the handler runs and bounds the whole response write,
// so the hub's streamed transcripts would die mid-stream at the timeout. (An
// upgraded connection would survive it, but only because Hijack clears every
// deadline — an accident of the hijack path, not a property to rely on, and no
// help at all to the streaming path.) ReadTimeout is worse than it looks: it
// is what makes the header deadline persist as a whole-request read deadline,
// which is precisely why ReadHeaderTimeout is safe here without it.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// isLoopbackAddr reports whether ta is bound to a loopback address. Same
// check shape as the broker admin listener's guard (internal/broker/server.go).
func isLoopbackAddr(ta *net.TCPAddr) bool {
	return ta != nil && ta.IP.IsLoopback()
}

// writePIDFile records the running process's pid at path (0600), creating
// the parent directory if needed. A no-op when path is empty (tests that
// don't care about pid tracking).
func writePIDFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("remoteproxy: pid dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("remoteproxy: write pid: %w", err)
	}
	return nil
}

// removePIDFile deletes the pid file on shutdown. A removal failure is a
// warning, not fatal (the process is exiting anyway); an already-absent file
// is fine. A no-op when path is empty.
func removePIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "lever: warning: could not remove %s: %v\n", path, err)
	}
}

// OpenAudit opens path for append (creating it 0600 if absent) and returns a
// function that appends one newline-terminated JSON AuditLine per call, plus
// the underlying io.Closer. Writes are serialized so concurrent requests
// never interleave partial lines. A marshal or write failure is reported to
// stderr rather than returned: a broken audit sink must never take down the
// request it is describing.
func OpenAudit(path string) (func(AuditLine), io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("remoteproxy: open audit %s: %w", path, err)
	}
	var mu sync.Mutex
	write := func(line AuditLine) {
		b, err := json.Marshal(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lever: warning: audit marshal: %v\n", err)
			return
		}
		b = append(b, '\n')
		mu.Lock()
		defer mu.Unlock()
		if _, err := f.Write(b); err != nil {
			fmt.Fprintf(os.Stderr, "lever: warning: audit write: %v\n", err)
		}
	}
	return write, f, nil
}
