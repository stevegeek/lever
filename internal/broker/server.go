package broker

import (
	"context"
	"fmt"
	"net/http"
)

// Handler builds the broker's HTTP routing: /provision, one gated route per
// policy Route, and (in api-key mode) the LLM proxy.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/provision", b.handleProvision)
	for _, rt := range b.policy.Routes {
		h, err := b.routeHandler(rt)
		if err != nil {
			// A bad backend URL is a config error; register a fail-closed handler.
			msg := err.Error()
			mux.Handle(rt.PathPrefix, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, msg, http.StatusInternalServerError)
			}))
			continue
		}
		mux.Handle(rt.PathPrefix, h)
	}
	if b.policy.LLM.Mode == LLMAPIKey {
		mux.Handle("/llm/", b.llmHandler())
	}
	return mux
}

// Serve runs the broker over mTLS on addr until ctx is cancelled. serverCert/Key
// are PEM bytes for the broker's own TLS identity (issued from the same CA).
func (b *Broker) Serve(ctx context.Context, addr string, serverCertPEM, serverKeyPEM []byte) error {
	tlsCfg, err := b.ca.ServerTLSConfig(serverCertPEM, serverKeyPEM)
	if err != nil {
		return fmt.Errorf("broker: tls config: %w", err)
	}
	srv := &http.Server{Addr: addr, Handler: b.Handler(), TLSConfig: tlsCfg}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	b.log.Info("broker.serving", "addr", addr)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("broker: serve: %w", err)
	}
	return nil
}
