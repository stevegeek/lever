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
// Routes: /provision, /worker/*, /msg/send, /msg/list, /directive/consume,
// /directive/check, /enrol, /renew, /request, and one gated proxy per
// currently-registered tool under /mcp/<name>/. Tool routes are bound at
// call time — tools must be registered before JailHandler() is called.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	// Method patterns: every JSON route is POST-only; /tools is the lone GET.
	// A wrong method 405s at the mux, before any handler runs.
	mux.HandleFunc("POST "+wire.PathProvision, b.handleProvision)
	mux.HandleFunc("POST "+wire.PathWorkerStart, b.handleWorkerStart)
	mux.HandleFunc("POST "+wire.PathWorkerStop, b.handleWorkerStop)
	mux.HandleFunc("POST "+wire.PathWorkerSuspend, b.handleWorkerSuspend)
	mux.HandleFunc("POST "+wire.PathWorkerResume, b.handleWorkerResume)
	mux.HandleFunc("POST "+wire.PathWorkerList, b.handleWorkerList)
	mux.HandleFunc("POST "+wire.PathMsgSend, b.handleMsgSend)
	mux.HandleFunc("POST "+wire.PathMsgList, b.handleMsgList)
	mux.HandleFunc("POST "+wire.PathDirectiveConsume, b.handleDirectiveConsume)
	mux.HandleFunc("POST "+wire.PathDirectiveCheck, b.handleDirectiveCheck)
	mux.HandleFunc("POST "+wire.PathEnrol, b.handleEnrol)
	mux.HandleFunc("POST "+wire.PathRenew, b.handleRenew)
	mux.HandleFunc("POST "+wire.PathRequest, b.handleRequest)
	mux.HandleFunc("GET "+wire.PathTools, b.handleTools)

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
		mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	}
	if b.apiKey != nil {
		mux.Handle("/llm/", http.StripPrefix("/llm", b.llmProxyHandler()))
	}
	return mux
}

// EpochResponse aliases wire.EpochResponse for callers that still name it
// through this package (internal/cli/apply.go).
type EpochResponse = wire.EpochResponse

// handleEpoch serves the current epoch for captool freshness checks (admin/loopback).
func (b *Broker) handleEpoch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, EpochResponse{Epoch: b.MinEpoch(), Version: b.version, ConfigHash: b.configHash})
}

// AdminHandler builds an http.Handler for the admin (loopback) listener.
// Routes /register, /epoch, /bump-epoch, /revoke, /bootstrap — no capability-gated or agent-facing endpoints.
func (b *Broker) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathRegister, b.handleRegister)
	mux.HandleFunc("GET "+wire.PathEpoch, b.handleEpoch)
	mux.HandleFunc("POST "+wire.PathBumpEpoch, b.handleBumpEpoch)
	mux.HandleFunc("POST "+wire.PathRevoke, b.handleRevoke)
	mux.HandleFunc("POST "+wire.PathBootstrap, b.handleBootstrap)
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
