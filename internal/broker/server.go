package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/wire"
)

// JailHandler builds an http.Handler that routes the jail (mTLS) listener.
// Routes: /worker/*, /msg/send, /msg/list, /directive/consume,
// /directive/check, /enrol, /renew, /request, and one gated proxy per
// currently-registered tool under /mcp/<name>/. Tool routes are bound at
// call time — tools must be registered before JailHandler() is called.
//
// Every route runs under the per-route deadlines of b.timeouts: the request
// body must arrive within Body (a connection read deadline; the listener has
// no ReadTimeout because /llm streams), and the small JSON control routes
// also run under an http.TimeoutHandler (Control), answering 503 when the
// handler overruns.
//
// /llm, /mcp/<tool>/ and /worker/* carry ONLY the body deadline. An
// http.TimeoutHandler buffers the whole response and offers no Flusher, so
// under one a streaming backend (an MCP streamable-HTTP text/event-stream
// answer, a streamed completion) delivers nothing until it finishes, and a
// legitimately long tool call or dispatch is cut at the bound with its
// buffered output discarded. A per-write deadline is not used either: re-armed
// on each write it would cut a quiet, long-lived MCP event stream, and cleared
// after each write it bounds only writes past the kernel send buffer. The
// handler side of these routes is therefore unbounded, as it was before the
// body deadline was added: the agent-side client and the runtime calls carry
// their own timeouts, and a client that goes away cancels the request context.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	control := func(h http.HandlerFunc) http.Handler { return b.bounded(h, b.timeouts.Control) }
	worker := func(h http.HandlerFunc) http.Handler { return withBodyDeadline(b.timeouts.Body, h) }
	// Method patterns: every JSON route is POST-only; /tools is the lone GET.
	// A wrong method 405s at the mux, before any handler runs.
	mux.Handle("POST "+wire.PathWorkerStart, worker(b.handleWorkerStart))
	mux.Handle("POST "+wire.PathWorkerStop, worker(b.handleWorkerStop))
	mux.Handle("POST "+wire.PathWorkerSuspend, worker(b.handleWorkerSuspend))
	mux.Handle("POST "+wire.PathWorkerResume, worker(b.handleWorkerResume))
	mux.Handle("POST "+wire.PathWorkerList, control(b.handleWorkerList))
	mux.Handle("POST "+wire.PathMsgSend, control(b.handleMsgSend))
	mux.Handle("POST "+wire.PathMsgList, control(b.handleMsgList))
	mux.Handle("POST "+wire.PathDirectiveConsume, control(b.handleDirectiveConsume))
	mux.Handle("POST "+wire.PathDirectiveCheck, control(b.handleDirectiveCheck))
	mux.Handle("POST "+wire.PathEnrol, control(b.handleEnrol))
	mux.Handle("POST "+wire.PathRenew, control(b.handleRenew))
	mux.Handle("POST "+wire.PathRequest, control(b.handleRequest))
	mux.Handle("GET "+wire.PathTools, control(b.handleTools))

	for _, name := range b.reg.Names() {
		if name == ReservedLLMTool {
			continue // served by /llm, not an /mcp/<name>/ tool route
		}
		handler, err := b.gatewayHandler(name)
		if err != nil {
			b.audit("gateway", "", "error", err.Error())
			continue
		}
		// Strip the /mcp/<name> prefix so the tool proxy sees a clean path.
		prefix := "/mcp/" + name
		mux.Handle(prefix+"/", http.StripPrefix(prefix, withBodyDeadline(b.timeouts.Body, handler)))
	}
	if b.apiKey != nil {
		mux.Handle("/llm/", http.StripPrefix("/llm", withBodyDeadline(b.timeouts.Body, b.llmProxyHandler())))
	}
	return mux
}

// bounded wraps a control route (a small JSON exchange, never streamed): the
// request body must arrive within b.timeouts.Body and h must finish within
// d, else the client gets 503 and h's eventual output is discarded
// (http.TimeoutHandler also cancels the request context, so a call in flight
// is abandoned). The body deadline is set on the OUTER writer: TimeoutHandler's
// writer has no connection to set it on. Not for a route that streams or
// legitimately runs long — see JailHandler.
func (b *Broker) bounded(h http.Handler, d time.Duration) http.Handler {
	return withBodyDeadline(b.timeouts.Body, http.TimeoutHandler(h, d, "request timed out"))
}

// withBodyDeadline puts a read deadline of d on the connection before h runs,
// bounding the request-body read (net/http clears the post-header deadline
// when the server has no ReadTimeout). Per request: the server re-arms the
// header and idle deadlines itself between requests. Best-effort — a writer
// with no connection (a test recorder) reports ErrNotSupported and is left
// alone.
func withBodyDeadline(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
		h.ServeHTTP(w, r)
	})
}

// handleEpoch serves the current epoch for captool freshness checks (admin/loopback).
func (b *Broker) handleEpoch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, wire.EpochResponse{Epoch: b.MinEpoch(), Version: b.version, ConfigHash: b.configHash})
}

// AdminHandler builds an http.Handler for the admin (loopback) listener.
// Routes /register, /epoch, /bump-epoch, /revoke, /bootstrap, /worker-ticket
// — no capability-gated or agent-facing endpoints. Nothing an agent can
// reach mints an enrolment ticket: the manager's comes from /bootstrap, a
// worker's from a dispatch or /worker-ticket, both host-side.
func (b *Broker) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathRegister, b.handleRegister)
	mux.HandleFunc("GET "+wire.PathEpoch, b.handleEpoch)
	mux.HandleFunc("POST "+wire.PathBumpEpoch, b.handleBumpEpoch)
	mux.HandleFunc("POST "+wire.PathRevoke, b.handleRevoke)
	mux.HandleFunc("POST "+wire.PathBootstrap, b.handleBootstrap)
	mux.HandleFunc("POST "+wire.PathWorkerTicket, b.handleWorkerTicket)
	return mux
}

// ServeListeners runs the broker on pre-bound listeners (the supervisor binds
// them so it can learn OS-assigned ports before starting tools). Runs until ctx
// is cancelled. jailLn carries mTLS with a self-rotating serving cert (certSrc
// re-mints before certTTL expires, so a long-running broker never serves an
// expired cert); adminLn is loopback plain HTTP. directiveLn is the
// operator-directive admin channel's UDS socket — nil when directives are
// disabled (or a caller has no socket to offer); when non-nil it MUST be a
// *net.UnixListener (fail closed otherwise), since the directive admin routes
// are gated by the socket's 0600 file permissions, not by network origin.
func (b *Broker) ServeListeners(ctx context.Context, jailLn, adminLn, directiveLn net.Listener, certSrc *ca.ServerCertSource) error {
	// Fail closed if the caller bound adminLn on a non-loopback interface.
	// The unauthenticated admin routes (/bootstrap, /register, /revoke, …) must
	// never be reachable from a routable interface — enforce the invariant here
	// rather than relying on every caller to get it right.
	if ta, ok := adminLn.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		_ = jailLn.Close()
		_ = adminLn.Close()
		if directiveLn != nil {
			_ = directiveLn.Close()
		}
		return fmt.Errorf("broker: admin listener must be loopback, got %s", adminLn.Addr())
	}
	if directiveLn != nil {
		if _, ok := directiveLn.(*net.UnixListener); !ok {
			_ = jailLn.Close()
			_ = adminLn.Close()
			_ = directiveLn.Close()
			return fmt.Errorf("broker: directive listener must be a unix socket, got %T", directiveLn)
		}
	}
	onLapse := b.lapseFunc()
	tlsCfg := b.ca.ServerTLSConfigSource(certSrc, onLapse)
	if onLapse != nil {
		// Auto-re-enrol healer (#22): drains natural-lapse events for the life
		// of the serve. Only started when the hook is installed at all.
		go b.runHealer(ctx)
	}
	// No ReadTimeout/WriteTimeout here: /llm streams. Body and handler
	// deadlines are per route (JailHandler, b.timeouts).
	jailSrv := &http.Server{
		Handler: b.JailHandler(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	adminSrv := &http.Server{
		Handler:           b.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	var directiveSrv *http.Server
	numServers := 2
	errc := make(chan error, 3)
	go func() { errc <- jailSrv.ServeTLS(jailLn, "", "") }()
	go func() { errc <- adminSrv.Serve(adminLn) }()
	if directiveLn != nil {
		directiveSrv = &http.Server{
			Handler:           b.DirectiveAdminHandler(),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
		}
		numServers = 3
		go func() { errc <- directiveSrv.Serve(directiveLn) }()
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jailSrv.Shutdown(shutCtx)
		_ = adminSrv.Shutdown(shutCtx)
		if directiveSrv != nil {
			_ = directiveSrv.Shutdown(shutCtx)
		}
	}()
	// Return the first real error (ignore ErrServerClosed from clean shutdown).
	for i := 0; i < numServers; i++ {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}
