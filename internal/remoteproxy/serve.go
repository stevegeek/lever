package remoteproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/daemon"
)

// ServeConfig configures Serve.
type ServeConfig struct {
	Port int
	// Bind is the IP address the proxy binds. "" binds 127.0.0.1, the
	// default and the posture the rest of the package assumes. A loopback
	// address must yield a loopback listener (fail closed otherwise, see
	// listenProxy). Anything else is refused unless AllowNonLoopback is set.
	Bind string
	// AllowNonLoopback acknowledges a non-loopback Bind, so a caller cannot
	// widen the listener by passing an address alone. `lever remote serve`
	// sets it from a config that config.validateRemoteBind restricted to an
	// address the jail cannot reach, or to an acknowledged wildcard. The login
	// provider's listener is always loopback.
	AllowNonLoopback bool
	// Handler is the pre-built proxy handler (NewHandler's return value).
	// Its Audit callback must be wired BEFORE Serve is called: Serve cannot
	// reach into an opaque http.Handler to rewire it.
	Handler http.Handler
	// PIDPath, when non-empty, is where Serve records its own pid, written
	// once the listener is bound and removed on shutdown.
	PIDPath string
	// Stamp, when non-nil, records what THIS process is serving. Serve calls
	// it once, after the listeners are bound and PIDPath is written, and
	// before the first request can be handled.
	//
	// The running proxy must write that record itself: `lever apply` compares
	// a host-side stamp (state.State.WriteRemoteStamp) to decide whether to
	// reuse a running proxy, but apply is not the only thing that starts
	// proxies. Only the process itself knows what config it enforces.
	//
	// Ordering is part of the contract: after the bind, so a proxy that could
	// not take the port leaves the incumbent's record alone; after the pid
	// file, so an implementation may key its record on that pid.
	//
	// An error is a warning, not a failed serve. That is only safe while the
	// implementation leaves NO record behind when it fails (a stale stamp
	// would defeat apply's check); state.State.WriteRemoteStamp removes the
	// file on every failure path.
	Stamp func() error
	// Provider, when non-nil, is the local OIDC provider, served on its OWN
	// loopback listener (127.0.0.1:Provider.Port()) for the life of the serve.
	// The proxy answers the operator's browser through `tailscale serve`, the
	// provider answers the hub's back channel from inside the jail; one shared
	// listener would expose each to a caller that has no business with it.
	Provider *Provider
}

// Serve runs the proxy until ctx is cancelled: bind the proxy (127.0.0.1:<Port>
// by default; see listenProxy for a non-loopback Bind), bind the provider's
// loopback port when one is configured, write the pid file, record what this
// process is serving (Stamp), and serve. The caller opens the audit JSONL
// (OpenAudit) before building Handler. On ctx.Done it shuts both servers
// down gracefully, removes the pid file, and returns.
func Serve(ctx context.Context, cfg ServeConfig) error {
	ln, err := listenProxy(cfg.Bind, cfg.Port, cfg.AllowNonLoopback)
	if err != nil {
		return err
	}
	// Both listeners are bound before anything else, so a port collision
	// fails the serve outright instead of leaving a proxy whose login path
	// can never work.
	var provLn net.Listener
	if cfg.Provider != nil {
		provLn, err = listenLoopback(cfg.Provider.Port())
		if err != nil {
			_ = ln.Close()
			return err
		}
	}

	if err := daemon.WritePIDFile(cfg.PIDPath); err != nil {
		daemon.CloseListeners(ln, provLn)
		return err
	}
	defer daemon.RemovePIDFile(cfg.PIDPath)

	if cfg.Stamp != nil {
		if err := cfg.Stamp(); err != nil {
			slog.Warn("could not record what this proxy is serving; the next lever apply restarts the proxy rather than reuse it", "err", err)
		}
	}

	// Each goroutine closes over its OWN *http.Server, never over the slice:
	// appending the provider rewrites the slice header while the proxy's
	// goroutine runs (a data race). Only this goroutine touches the slice.
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
		for range servers {
			<-serveErr
		}
		return nil
	case err := <-serveErr:
		// One of them stopped on its own: take the other down too. Half-running
		// is never a useful state.
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
// The proxy's identity-header gate holds only while nothing but the
// authenticating front can reach this listener (see proxy.go's package doc):
// a peer that reaches it directly could set that header itself. Off
// loopback, the provider would be an unauthenticated identity endpoint.
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

// listenProxy binds the proxy's own listener: loopback by default (the same
// fail-closed listener as listenLoopback), or the configured address.
//
// A non-loopback address makes "only the front can reach this listener"
// the operator's firewall's job, so it needs the explicit acknowledgement
// and is announced on stderr (remote.log) every time the proxy starts.
func listenProxy(bind string, port int, allowNonLoopback bool) (net.Listener, error) {
	if bind == "" {
		return listenLoopback(port)
	}
	ip := net.ParseIP(bind)
	if ip == nil {
		return nil, fmt.Errorf("remoteproxy: bind %q is not an IP address", bind)
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return nil, fmt.Errorf("remoteproxy: bind %s is not loopback, and non-loopback binds were not acknowledged", bind)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("remoteproxy: bind: %w", err)
	}
	ta, ok := ln.Addr().(*net.TCPAddr)
	if ip.IsLoopback() && (!ok || !isLoopbackAddr(ta)) {
		_ = ln.Close()
		return nil, fmt.Errorf("remoteproxy: listener must be loopback, got %s", ln.Addr())
	}
	if !ip.IsLoopback() {
		slog.Warn("listening on an address that is NOT loopback: anything that can reach it can assert any identity "+
			"header, so only the authenticating front may reach it (host firewall)", "addr", ln.Addr().String())
	}
	return ln, nil
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
// Only two timeouts, chosen so they cannot break a legitimate session:
//
//   - ReadHeaderTimeout covers the header phase alone. Once the headers are
//     read, conn.readRequest resets the read deadline to ReadTimeout's
//     deadline, which is zero here, so nothing bounds the rest.
//   - IdleTimeout applies only between requests on a kept-alive connection.
//
// WriteTimeout and ReadTimeout must NOT be added. WriteTimeout bounds the
// whole response write, so the hub's streamed transcripts would die at the
// timeout. ReadTimeout would make the header deadline persist as a
// whole-request read deadline.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// isLoopbackAddr reports whether ta is bound to a loopback address.
func isLoopbackAddr(ta *net.TCPAddr) bool {
	return ta != nil && ta.IP.IsLoopback()
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
			slog.Warn("remote audit line not written", "err", err)
			return
		}
		b = append(b, '\n')
		mu.Lock()
		defer mu.Unlock()
		if _, err := f.Write(b); err != nil {
			slog.Warn("remote audit line not written", "err", err)
		}
	}
	return write, f, nil
}
