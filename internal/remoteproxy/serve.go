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
	// AuditPath, when non-empty, is opened by Serve via OpenAudit for the
	// life of the serve, guaranteeing the audit JSONL exists (0600) as soon
	// as the proxy starts and is closed cleanly on shutdown. This is
	// independent of Handler's own Audit wiring (see the Handler field
	// doc): the caller's separate OpenAudit call is what actually receives
	// per-request AuditLines, since Handler is already built by the time
	// Serve runs. Passing the same path here just means Serve co-owns the
	// file's lifecycle rather than leaving it entirely to the caller.
	AuditPath string
}

// Serve runs the proxy until ctx is cancelled: bind 127.0.0.1:<Port> (fail
// closed on any non-loopback listen address), open the audit JSONL (append,
// 0600) when AuditPath is set, write the pid file, and serve cfg.Handler.
// On ctx.Done it shuts the server down gracefully, removes the pid file, and
// returns. Mirrors brokerctl.Serve's bind → pid → serve → remove-pid
// ordering (internal/brokerctl/serve.go).
func Serve(ctx context.Context, cfg ServeConfig) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("remoteproxy: bind: %w", err)
	}
	// Fail closed on anything but a loopback bind. The Handler's
	// Tailscale-User-Login gate is only as strong as "nothing but
	// `tailscale serve` can reach this listener" (see proxy.go's package
	// doc): a non-loopback listener would let any LAN/tailnet peer reach it
	// directly and set that header itself, bypassing the gate entirely.
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || !isLoopbackAddr(ta) {
		_ = ln.Close()
		return fmt.Errorf("remoteproxy: listener must be loopback, got %s", ln.Addr())
	}

	if cfg.AuditPath != "" {
		_, auditCloser, err := OpenAudit(cfg.AuditPath)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("remoteproxy: open audit: %w", err)
		}
		defer auditCloser.Close()
	}

	if err := writePIDFile(cfg.PIDPath); err != nil {
		_ = ln.Close()
		return err
	}
	defer removePIDFile(cfg.PIDPath)

	srv := newServer(cfg.Handler)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("remoteproxy: shutdown: %w", err)
		}
		<-serveErr // wait for srv.Serve to actually return before the listener is considered released
		return nil
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("remoteproxy: serve: %w", err)
		}
		return nil
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
