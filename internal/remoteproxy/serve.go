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

type ServeConfig struct {
	Port int
	// Bind is the IP address bound; "" is 127.0.0.1. A loopback address must
	// yield a loopback listener (fail closed). Anything else needs
	// AllowNonLoopback.
	Bind string
	// AllowNonLoopback acknowledges a non-loopback Bind, so an address alone
	// cannot widen the listener. Callers pass only an address the jail cannot
	// reach (config.validateRemoteBind): a jail that reaches the listener can
	// forge the identity header. The provider's listener is always loopback.
	AllowNonLoopback bool
	// Handler is NewHandler's result; its Audit must be wired before Serve.
	Handler http.Handler
	// PIDPath, when non-empty, holds Serve's pid while the listener is bound.
	PIDPath string
	// Stamp, when non-nil, records what this process is serving: `lever
	// apply` decides whether to reuse a proxy from that record, and only the
	// process knows what config it enforces. Called after the bind (a proxy
	// that lost the port leaves the incumbent's record alone) and after the
	// pid file, before the first request. An error is only a warning, so the
	// implementation must leave no record behind on failure.
	Stamp func() error
	// Provider, when non-nil, is the OIDC provider, on its own loopback
	// listener: the proxy serves the operator's browser, the provider the hub's
	// back channel, and neither should be reachable by the other's callers.
	Provider *Provider
}

// Serve runs the proxy and the provider until ctx is cancelled, then shuts
// both down and removes the pid file. The caller opens the audit log
// (OpenAudit) before building Handler.
func Serve(ctx context.Context, cfg ServeConfig) error {
	ln, err := listenProxy(cfg.Bind, cfg.Port, cfg.AllowNonLoopback)
	if err != nil {
		return err
	}
	// Bind both first, so a port collision fails the serve outright.
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

	// Each goroutine closes over its own *http.Server, never the slice:
	// appending rewrites the slice header while the proxy goroutine runs.
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
		// Half-running is never useful: one stopped, so stop the other.
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

// listenLoopback fails closed on anything but a loopback address: the
// identity-header gate holds only while just the front can reach the
// listener, and off loopback the provider is an unauthenticated identity
// endpoint.
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

// listenProxy binds loopback by default. A non-loopback address makes
// front-only reachability the operator's firewall's job, so it needs the
// acknowledgement and is announced on stderr at every start.
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

const (
	serverReadHeaderTimeout = 20 * time.Second
	serverIdleTimeout       = 120 * time.Second
)

// newServer sets only ReadHeaderTimeout (the header phase alone; the read
// deadline is then reset to ReadTimeout's zero) and IdleTimeout. Never add
// WriteTimeout, which would kill the hub's streamed transcripts, or
// ReadTimeout, which would turn the header deadline into a whole-request one.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

func isLoopbackAddr(ta *net.TCPAddr) bool {
	return ta != nil && ta.IP.IsLoopback()
}

// OpenAudit opens path for append (0600) and returns a serialized appender
// of JSON AuditLines. Failures go to stderr: a broken audit sink must never
// take down the request it describes.
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
