package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalGatewayAddr is the loopback address the gateway sidecar listens on and
// LocalGatewayURL is the base URL Claude's MCP/LLM config points at. The
// gateway proxies that plaintext traffic to the broker over mTLS with the
// current agent leaf, so Claude never holds the rotating cert, which it would
// cache for its whole lifetime.
const (
	LocalGatewayAddr = "127.0.0.1:8462"
	LocalGatewayURL  = "http://" + LocalGatewayAddr
)

// idleConnTimeout forces regular fresh handshakes. GetClientCertificate runs
// per handshake, not per request, so a warm keep-alive connection would keep
// presenting its original leaf past renewal and expiry.
const idleConnTimeout = 5 * time.Minute

// clientCertSource re-reads the rotating agent leaf when agent.crt's mtime
// advances. It must be concurrency-safe: GetClientCertificate is invoked from
// many handshake goroutines.
type clientCertSource struct {
	idDir string
	now   func() time.Time // test seam; nil means time.Now

	mu    sync.Mutex
	cert  *tls.Certificate
	mtime time.Time
}

// newClientCertSource loads eagerly so a broken id-dir fails at startup, not on
// the first live handshake.
func newClientCertSource(idDir string) (*clientCertSource, error) {
	s := &clientCertSource{idDir: idDir}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetClientCertificate returns the current leaf. Fail-soft: if a re-read fails
// but the cached leaf is still valid, it logs and serves it.
func (s *clientCertSource) GetClientCertificate(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fi, err := os.Stat(filepath.Join(s.idDir, "agent.crt")); err == nil && fi.ModTime().After(s.mtime) {
		if rerr := s.reloadLocked(); rerr != nil {
			now := time.Now
			if s.now != nil {
				now = s.now
			}
			if s.cert == nil || now().After(s.cert.Leaf.NotAfter) {
				return nil, rerr
			}
			slog.Warn("re-reading the agent leaf failed; serving the cached one", "err", rerr)
		}
	}
	return s.cert, nil
}

// reloadLocked updates the cache on success only. Caller holds s.mu. It stats
// before reading so an interleaved write cannot cache a newer mtime with older
// content, which would skip the next rotation.
func (s *clientCertSource) reloadLocked() error {
	crt := filepath.Join(s.idDir, "agent.crt")
	fi, err := os.Stat(crt)
	if err != nil {
		return fmt.Errorf("gateway: stat agent leaf: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(crt, filepath.Join(s.idDir, "agent.key"))
	if err != nil {
		return fmt.Errorf("gateway: load agent leaf: %w", err)
	}
	if pair.Leaf == nil { // older Go leaves Leaf unparsed
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return fmt.Errorf("gateway: parse agent leaf: %w", err)
		}
		pair.Leaf = leaf
	}
	s.cert = &pair
	s.mtime = fi.ModTime()
	return nil
}

var errBadCAPEM = errors.New("agent: bad CA PEM")

var errNotLoopback = errors.New("gateway: listen addr must be loopback")

func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errBadCAPEM
	}
	return pool, nil
}

// reloadingTransport is shared by every long-lived broker client and the
// gateway proxy: it presents the rotating leaf per handshake and caps idle
// connection reuse (idleConnTimeout).
func reloadingTransport(idDir string, caPEM []byte) (*http.Transport, error) {
	src, err := newClientCertSource(idDir)
	if err != nil {
		return nil, err
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:              pool,
			GetClientCertificate: src.GetClientCertificate,
		},
		IdleConnTimeout: idleConnTimeout,
	}, nil
}

// NewReloadingClient builds an mTLS http.Client for a long-lived
// direct-to-broker client that re-reads the rotating agent leaf per TLS
// handshake. caPEM is the pinned CA; only the leaf rotates. A continuously
// busy connection is not recycled by idleConnTimeout, which is acceptable for
// bursty callers only.
func NewReloadingClient(idDir string, caPEM []byte) (*http.Client, error) {
	tr, err := reloadingTransport(idDir, caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr}, nil
}

// GatewayConfig drives Gateway.
type GatewayConfig struct {
	Listen    string // loopback address to serve plaintext on (LocalGatewayAddr in production)
	BrokerURL string // real broker origin the proxy forwards to over mTLS
	CAPEM     []byte // CA that signed the broker's serving cert
	IDDir     string // directory holding the rotating agent.{crt,key}
}

// gatewayShutdownGrace bounds how long a cancelled Gateway waits for in-flight
// proxied requests before dropping them.
const gatewayShutdownGrace = 5 * time.Second

// Gateway runs the loopback reverse-proxy: it accepts plaintext HTTP from
// in-container Claude on c.Listen and forwards to the real broker at c.BrokerURL
// over mTLS. Blocks until ctx is cancelled or the listener fails. A cancel is
// a clean stop returning nil: in-flight requests get gatewayShutdownGrace,
// then anything still open (a long SSE stream) is closed.
func Gateway(ctx context.Context, c GatewayConfig) error {
	if err := requireLoopback(c.Listen); err != nil {
		return err
	}
	proxy, err := newGatewayProxy(c.BrokerURL, c.CAPEM, c.IDDir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("gateway: listen %s: %w", c.Listen, err)
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), gatewayShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close() // grace expired: drop what is still open
	}
	<-errc // Serve returns ErrServerClosed as soon as Shutdown begins
	return nil
}

// requireLoopback fails closed unless listenAddr binds a loopback IP. The proxy
// presents the agent's private key, so it must never be reachable off-host.
func requireLoopback(listenAddr string) error {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("gateway: parse listen addr %q: %w", listenAddr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w, got %q", errNotLoopback, listenAddr)
	}
	return nil
}

// newGatewayProxy passes headers through unscrubbed: it is the agent's own
// proxy, and the broker gateway scrubs forwarding and X-Lever-* headers.
func newGatewayProxy(brokerURL string, caPEM []byte, idDir string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(brokerURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse broker URL %q: %w", brokerURL, err)
	}
	tr, err := reloadingTransport(idDir, caPEM)
	if err != nil {
		return nil, err
	}
	// target has no path, so the incoming path is forwarded verbatim.
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = tr
	// MCP tool responses stream over SSE; buffering stalls streaming tool
	// calls.
	proxy.FlushInterval = -1
	return proxy, nil
}
