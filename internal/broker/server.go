package broker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// JailHandler builds an http.Handler that routes the jail (mTLS) listener.
// Routes: /provision, /enrol, /renew, /request, and one gated gateway per
// currently-registered tool under /mcp/<name>/. Gateways are bound at call
// time — tools must be registered before JailHandler() is called.
func (b *Broker) JailHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/provision", b.handleProvision)
	mux.HandleFunc("/enrol", b.handleEnrol)
	mux.HandleFunc("/renew", b.handleRenew)
	mux.HandleFunc("/request", b.handleRequest)

	for _, name := range b.reg.Names() {
		handler, err := b.gatewayHandler(name)
		if err != nil {
			b.audit("gateway", "", "error", err.Error())
			continue
		}
		// Strip the /mcp/<name> prefix so the gateway sees a clean path.
		prefix := "/mcp/" + name
		mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	}
	return mux
}

// AdminHandler builds an http.Handler for the admin (loopback) listener.
// Routes only /register — no capability-gated or agent-facing endpoints.
func (b *Broker) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", b.handleRegister)
	return mux
}

// Serve starts the jail listener over mTLS and the admin listener over plain
// HTTP bound to loopback. It runs until ctx is cancelled, then shuts both
// servers down. Returns the first non-ErrServerClosed error from either server,
// or nil on clean shutdown.
func (b *Broker) Serve(ctx context.Context, jailAddr, adminAddr string, serverCertPEM, serverKeyPEM []byte) error {
	tlsCfg, err := b.ca.ServerTLSConfig(serverCertPEM, serverKeyPEM)
	if err != nil {
		return err
	}

	// Ensure admin listener is bound only to loopback.
	adminHost, adminPort, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return err
	}
	if adminHost == "" {
		adminHost = "127.0.0.1"
	}
	boundAdminAddr := net.JoinHostPort(adminHost, adminPort)

	jailSrv := &http.Server{
		Addr:              jailAddr,
		Handler:           b.JailHandler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64 KiB — slowloris/header-DoS mitigation
	}
	adminSrv := &http.Server{
		Addr:              boundAdminAddr,
		Handler:           b.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	errc := make(chan error, 2)

	go func() {
		// ListenAndServeTLS with empty cert/key strings uses TLSConfig.
		errc <- jailSrv.ListenAndServeTLS("", "")
	}()
	go func() {
		errc <- adminSrv.ListenAndServe()
	}()

	// Wait for context cancellation, then shut both down.
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
