package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// JailHandler builds an http.Handler that routes the jail (mTLS) listener.
// Routes: /provision, /worker/*, /msg/send, /msg/list, /directive/consume,
// /directive/check, /enrol, /renew, /request, and one gated proxy per
// currently-registered tool under /mcp/<name>/. Tool routes are bound at
// call time — tools must be registered before JailHandler() is called.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/provision", b.handleProvision)
	mux.HandleFunc("/worker/start", b.handleWorkerStart)
	mux.HandleFunc("/worker/stop", b.handleWorkerStop)
	mux.HandleFunc("/worker/suspend", b.handleWorkerSuspend)
	mux.HandleFunc("/worker/resume", b.handleWorkerResume)
	mux.HandleFunc("/worker/list", b.handleWorkerList)
	mux.HandleFunc("/msg/send", b.handleMsgSend)
	mux.HandleFunc("/msg/list", b.handleMsgList)
	mux.HandleFunc("/directive/consume", b.handleDirectiveConsume)
	mux.HandleFunc("/directive/check", b.handleDirectiveCheck)
	mux.HandleFunc("/enrol", b.handleEnrol)
	mux.HandleFunc("/renew", b.handleRenew)
	mux.HandleFunc("/request", b.handleRequest)
	mux.HandleFunc("/tools", b.handleTools)

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

// EpochResponse reports the broker's current minimum acceptable token epoch,
// plus the serving process's identity: the binary version it runs and a
// digest of the broker-relevant configuration it was started with. apply's
// broker-reuse shortcut compares these against its own expectation and
// restarts the broker on mismatch (#19) — a broker predating these fields
// reports them empty, which callers treat as a mismatch.
type EpochResponse struct {
	Epoch      int    `json:"epoch"`
	Version    string `json:"version,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
}

// handleEpoch serves the current epoch for captool freshness checks (admin/loopback).
func (b *Broker) handleEpoch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, EpochResponse{Epoch: b.MinEpoch(), Version: b.version, ConfigHash: b.configHash})
}

// AdminHandler builds an http.Handler for the admin (loopback) listener.
// Routes /register, /epoch, /bump-epoch, /revoke, /bootstrap — no capability-gated or agent-facing endpoints.
func (b *Broker) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", b.handleRegister)
	mux.HandleFunc("/epoch", b.handleEpoch)
	mux.HandleFunc("/bump-epoch", b.handleBumpEpoch)
	mux.HandleFunc("/revoke", b.handleRevoke)
	mux.HandleFunc("/bootstrap", b.handleBootstrap)
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
