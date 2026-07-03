package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// resolveAdminAddr normalizes adminAddr to a loopback bind address. An empty
// host defaults to 127.0.0.1. Any explicit non-loopback host is rejected so
// the unauthenticated admin /register endpoint can never bind to a routable
// interface.
func resolveAdminAddr(adminAddr string) (string, error) {
	host, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return "", fmt.Errorf("broker: admin address %q: %w", adminAddr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("broker: admin listener must bind to a loopback address, got %q", host)
	}
	return net.JoinHostPort(host, port), nil
}

// JailHandler builds an http.Handler that routes the jail (mTLS) listener.
// Routes: /provision, /grove/*, /msg/send, /msg/list, /enrol, /renew,
// /request, and one gated gateway per currently-registered tool under
// /mcp/<name>/. Gateways are bound at call time — tools must be registered
// before JailHandler() is called.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/provision", b.handleProvision)
	mux.HandleFunc("/grove/start", b.handleGroveStart)
	mux.HandleFunc("/grove/stop", b.handleGroveStop)
	mux.HandleFunc("/grove/suspend", b.handleGroveSuspend)
	mux.HandleFunc("/grove/resume", b.handleGroveResume)
	mux.HandleFunc("/grove/list", b.handleGroveList)
	mux.HandleFunc("/msg/send", b.handleMsgSend)
	mux.HandleFunc("/msg/list", b.handleMsgList)
	mux.HandleFunc("/enrol", b.handleEnrol)
	mux.HandleFunc("/renew", b.handleRenew)
	mux.HandleFunc("/request", b.handleRequest)
	mux.HandleFunc("/tools", b.handleTools)

	for _, name := range b.reg.Names() {
		if name == ReservedLLMTool {
			continue // served by /llm, not the MCP gateway
		}
		handler, err := b.gatewayHandler(name)
		if err != nil {
			b.audit("gateway", "", "error", err.Error())
			continue
		}
		// Strip the /mcp/<name> prefix so the gateway sees a clean path.
		prefix := "/mcp/" + name
		mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	}
	if b.apiKey != nil {
		mux.Handle("/llm/", http.StripPrefix("/llm", b.llmProxyHandler()))
	}
	return mux
}

// EpochResponse reports the broker's current minimum acceptable token epoch.
type EpochResponse struct {
	Epoch int `json:"epoch"`
}

// handleEpoch serves the current epoch for captool freshness checks (admin/loopback).
func (b *Broker) handleEpoch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(EpochResponse{Epoch: b.MinEpoch()})
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
// is cancelled. jailLn carries mTLS; adminLn is loopback plain HTTP.
func (b *Broker) ServeListeners(ctx context.Context, jailLn, adminLn net.Listener, serverCertPEM, serverKeyPEM []byte) error {
	// Fail closed if the caller bound adminLn on a non-loopback interface.
	// The unauthenticated admin routes (/bootstrap, /register, /revoke, …) must
	// never be reachable from a routable interface — enforce the invariant here
	// rather than relying on every caller to get it right.
	if ta, ok := adminLn.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return fmt.Errorf("broker: admin listener must be loopback, got %s", adminLn.Addr())
	}
	tlsCfg, err := b.ca.ServerTLSConfig(serverCertPEM, serverKeyPEM)
	if err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}
	jailSrv := &http.Server{
		Handler: b.JailHandler(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	adminSrv := &http.Server{
		Handler:           b.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16,
	}
	errc := make(chan error, 2)
	go func() { errc <- jailSrv.ServeTLS(jailLn, "", "") }()
	go func() { errc <- adminSrv.Serve(adminLn) }()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jailSrv.Shutdown(shutCtx)
		_ = adminSrv.Shutdown(shutCtx)
	}()
	// Return the first real error (ignore ErrServerClosed from clean shutdown).
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

// Serve starts the jail listener over mTLS and the admin listener over plain
// HTTP bound to loopback. It runs until ctx is cancelled, then shuts both
// servers down. Returns the first non-ErrServerClosed error from either server,
// or nil on clean shutdown.
func (b *Broker) Serve(ctx context.Context, jailAddr, adminAddr string, serverCertPEM, serverKeyPEM []byte) error {
	// Ensure admin listener is bound only to loopback — fail closed on
	// misconfiguration so /register is never reachable from a routable interface.
	boundAdminAddr, err := resolveAdminAddr(adminAddr)
	if err != nil {
		return err
	}
	jailLn, err := net.Listen("tcp", jailAddr)
	if err != nil {
		return err
	}
	adminLn, err := net.Listen("tcp", boundAdminAddr)
	if err != nil {
		_ = jailLn.Close()
		return err
	}
	return b.ServeListeners(ctx, jailLn, adminLn, serverCertPEM, serverKeyPEM)
}
