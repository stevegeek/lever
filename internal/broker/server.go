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

// JailHandler builds the handler for the jail (mTLS) listener, with one gated
// proxy per registered tool under /mcp/<name>/. Tools must be registered
// before it is called.
//
// Every route has a body deadline; the JSON control routes also run under an
// http.TimeoutHandler (503 on overrun).
//
// /llm, /mcp/<tool>/ and /worker/* carry ONLY the body deadline: a
// TimeoutHandler buffers the whole response and has no Flusher, so it would
// stall a stream and cut a long tool call or dispatch. A per-write deadline
// would cut a quiet, long-lived MCP event stream. Their handlers are
// unbounded; the agent-side client and runtime calls carry their own
// timeouts, and a client that goes away cancels the request context.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	control := func(h http.HandlerFunc) http.Handler { return b.bounded(h, b.timeouts.Control) }
	worker := func(h http.HandlerFunc) http.Handler { return withBodyDeadline(b.timeouts.Body, h) }
	// A wrong method 405s at the mux, before any handler runs.
	mux.Handle("POST "+wire.PathWorkerStart, worker(b.handleWorkerStart))
	mux.Handle("POST "+wire.PathWorkerStop, worker(b.handleWorkerStop))
	mux.Handle("POST "+wire.PathWorkerSuspend, worker(b.handleWorkerSuspend))
	mux.Handle("POST "+wire.PathWorkerResume, worker(b.handleWorkerResume))
	mux.Handle("POST "+wire.PathWorkerRecycle, worker(b.handleWorkerRecycle))
	mux.Handle("POST "+wire.PathWorkerList, control(b.handleWorkerList))
	mux.Handle("POST "+wire.PathMsgSend, control(b.handleMsgSend))
	mux.Handle("POST "+wire.PathMsgList, control(b.handleMsgList))
	mux.Handle("POST "+wire.PathMsgRecipients, control(b.handleMsgRecipients))
	mux.Handle("POST "+wire.PathDirectiveConsume, control(b.handleDirectiveConsume))
	mux.Handle("POST "+wire.PathDirectiveCheck, control(b.handleDirectiveCheck))
	mux.Handle("POST "+wire.PathDirectivePreview, control(b.handleDirectivePreview))
	mux.Handle("POST "+wire.PathMessageVerify, control(b.handleMessageVerify))
	mux.Handle("POST "+wire.PathChatVerify, control(b.handleChatVerify))
	mux.Handle("POST "+wire.PathContacts, control(b.handleContacts))
	mux.Handle("POST "+wire.PathContactMessage, control(b.handleContactMessage))
	mux.Handle("POST "+wire.PathFilesList, control(b.handleFilesList))
	mux.Handle("POST "+wire.PathFilesShare, control(b.handleFilesShare))
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
		mux.Handle("/mcp/"+name+"/", withBodyDeadline(b.timeouts.Body, handler))
	}
	if b.apiKey != nil {
		mux.Handle("/llm/", http.StripPrefix("/llm", withBodyDeadline(b.timeouts.Body, b.llmProxyHandler())))
	}
	return mux
}

// bounded is for control routes only, never a streaming one (JailHandler).
// The body deadline is set on the OUTER writer: TimeoutHandler's writer has
// no connection to set it on.
func (b *Broker) bounded(h http.Handler, d time.Duration) http.Handler {
	return withBodyDeadline(b.timeouts.Body, http.TimeoutHandler(h, d, "request timed out"))
}

// withBodyDeadline bounds the request-body read: net/http clears the
// post-header deadline when the server has no ReadTimeout, and re-arms its own
// deadlines between requests. A writer with no connection is left alone.
func withBodyDeadline(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
		h.ServeHTTP(w, r)
	})
}

// EpochAnswer is the body of GET /epoch. InstanceID is read only host-side,
// so it lives here rather than in package wire, the in-jail ABI.
type EpochAnswer struct {
	wire.EpochResponse
	InstanceID string `json:"instance_id,omitempty"`
}

func (b *Broker) handleEpoch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, EpochAnswer{
		EpochResponse: wire.EpochResponse{Epoch: b.MinEpoch(), Version: b.version, ConfigHash: b.configHash},
		InstanceID:    b.epochInstanceID,
	})
}

// AdminHandler builds the handler for the loopback admin listener: no
// agent-facing routes. Nothing an agent can reach mints an enrolment ticket.
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

// OperatorHandler serves the 0600 operator UNIX socket. These routes are not
// on the admin TCP listener on purpose: that is unauthenticated loopback,
// reachable by any local process and the hub's network namespace, which could
// then pass text off as the operator's, wake workers, or bind ledger records.
// The socket's file permissions are the gate.
func (b *Broker) OperatorHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathOperatorNote, b.handleOperatorNote)
	mux.HandleFunc("POST "+wire.PathOperatorWake, b.handleOperatorWake)
	mux.HandleFunc("POST "+PathOperatorWorkerResume, b.handleOperatorWorkerResume)
	mux.HandleFunc("GET "+PathOperatorWorkerList, b.handleOperatorWorkerList)
	mux.HandleFunc("POST "+PathOperatorWorkerStart, b.handleOperatorWorkerStart)
	mux.HandleFunc("POST "+PathOperatorWorkerRecycle, b.handleOperatorWorkerRecycle)
	mux.HandleFunc("POST "+PathOperatorWorkerSuspend, b.handleOperatorWorkerSuspend)
	mux.HandleFunc("POST "+PathOperatorWorkerStop, b.handleOperatorWorkerStop)
	mux.HandleFunc("POST "+wire.PathOperatorAgentMessagesMatch, b.handleAgentMessagesMatch)
	return mux
}

// ServeListeners runs the broker on pre-bound listeners until ctx is
// cancelled. jailLn is mTLS with a self-rotating serving cert; adminLn must
// be loopback. directiveLn and operatorLn are nil or MUST be
// *net.UnixListener (fail closed otherwise): their routes are gated by the
// socket's 0600 permissions, not network origin.
func (b *Broker) ServeListeners(ctx context.Context, jailLn, adminLn, directiveLn, operatorLn net.Listener, certSrc *ca.ServerCertSource) error {
	// The unauthenticated admin routes must never be reachable from a
	// routable interface: enforce it here, not in every caller.
	closeAll := func() {
		for _, ln := range []net.Listener{jailLn, adminLn, directiveLn, operatorLn} {
			if ln != nil {
				_ = ln.Close()
			}
		}
	}
	if ta, ok := adminLn.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		closeAll()
		return fmt.Errorf("broker: admin listener must be loopback, got %s", adminLn.Addr())
	}
	for what, ln := range map[string]net.Listener{"directive": directiveLn, "operator": operatorLn} {
		if ln == nil {
			continue
		}
		if _, ok := ln.(*net.UnixListener); !ok {
			closeAll()
			return fmt.Errorf("broker: %s listener must be a unix socket, got %T", what, ln)
		}
	}
	onLapse := b.lapseFunc()
	tlsCfg := b.ca.ServerTLSConfigSource(certSrc, onLapse)
	if onLapse != nil {
		go b.runHealer(ctx)
	}
	if b.tokenWatchEnabled() {
		// scion's hub token refresh timer stalls while the host sleeps, and
		// nothing else notices.
		go b.runTokenWatch(ctx)
	}
	// No ReadTimeout/WriteTimeout: /llm streams. Deadlines are per route.
	jailSrv := &http.Server{
		Handler: b.JailHandler(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	adminSrv := &http.Server{
		Handler:           b.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	var directiveSrv, operatorSrv *http.Server
	numServers := 2
	errc := make(chan error, 4)
	go func() { errc <- jailSrv.ServeTLS(jailLn, "", "") }()
	go func() { errc <- adminSrv.Serve(adminLn) }()
	if directiveLn != nil {
		directiveSrv = &http.Server{
			Handler:           b.DirectiveAdminHandler(),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
		}
		numServers++
		go func() { errc <- directiveSrv.Serve(directiveLn) }()
	}
	if operatorLn != nil {
		operatorSrv = &http.Server{
			Handler:           b.OperatorHandler(),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
		}
		numServers++
		go func() { errc <- operatorSrv.Serve(operatorLn) }()
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
		if operatorSrv != nil {
			_ = operatorSrv.Shutdown(shutCtx)
		}
	}()
	for i := 0; i < numServers; i++ {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}
