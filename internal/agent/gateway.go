package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalGatewayURL is the loopback address Claude's MCP/LLM config points at. The
// gateway sidecar reverse-proxies that plaintext traffic to the real broker over
// mTLS, presenting the always-current agent leaf — so Claude never holds the
// rotating cert (which it would otherwise cache for its whole lifetime).
const LocalGatewayURL = "http://127.0.0.1:8462"

// idleConnTimeout caps how long a pooled broker connection lingers before the
// proxy must re-handshake. GetClientCertificate runs per-HANDSHAKE, not
// per-request, so a warm keep-alive connection keeps presenting the leaf it was
// opened with — past the 12h renew and 24h expiry. Capping idle reuse well under
// the renewal interval forces regular fresh handshakes (and fresh leaf reads), so
// a rotated cert reaches the broker long before the old one expires.
const idleConnTimeout = 5 * time.Minute

// clientCertSource re-reads the rotating agent leaf that lever-renew writes back
// to <idDir>/agent.{crt,key}. It caches the parsed keypair and re-reads only when
// agent.crt's mtime advances, keeping per-handshake calls cheap. Concurrency-safe:
// GetClientCertificate is invoked from many handshake goroutines. Mirrors the
// fail-soft + eager-mint shape of ca.ServerCertSource (the server side).
type clientCertSource struct {
	idDir string
	now   func() time.Time // test seam; nil means time.Now

	mu    sync.Mutex
	cert  *tls.Certificate
	mtime time.Time
}

// newClientCertSource mints eagerly so a broken id-dir fails at startup, not on
// the first live handshake (matches ca.NewServerCertSource).
func newClientCertSource(idDir string) (*clientCertSource, error) {
	s := &clientCertSource{idDir: idDir}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetClientCertificate returns the current leaf, re-reading it when agent.crt's
// mtime has advanced since the cache. Fail-soft: if the re-read fails but the
// cached leaf is still valid, log and serve it; error only when there is no
// usable cert (mirrors ca.ServerCertSource.GetCertificate).
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
			log.Printf("agent: re-read agent leaf failed, serving cached: %v", rerr)
		}
	}
	return s.cert, nil
}

// reloadLocked re-reads and parses <idDir>/agent.{crt,key}, updating the cached
// cert and mtime on success only (a failed read leaves the prior cert intact for
// fail-soft). Caller holds s.mu. Stats BEFORE reading so an interleaved write
// during the read doesn't cache a newer mtime alongside older content (which would
// silently skip the next rotation); a stale mtime just re-reads once more.
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

// NewReloadingClient builds an mTLS http.Client for a LONG-LIVED direct-to-broker
// client (e.g. serve-capability) that re-reads the rotating agent leaf per TLS
// handshake — the same fix the gateway applies to Claude's proxied traffic. A
// plain Identity.Client() freezes the boot leaf as a static tls.Certificate, so a
// daemon holding it keeps presenting an expired cert after the 24h leaf TTL even
// though lever-renew has written a fresh one to disk; this closes that gap for
// every remaining long-lived broker client. caPEM is the pinned CA (static — only
// the leaf rotates). IdleConnTimeout recycles idle connections so a fresh dial
// (and leaf re-read) happens well within the leaf TTL; a continuously-busy
// connection isn't capped by it, but this sidecar's mints are bursty and idle
// often (same reasoning as the gateway).
// Mints eagerly so a broken id-dir fails now, not on the first live handshake.
func NewReloadingClient(idDir string, caPEM []byte) (*http.Client, error) {
	src, err := newClientCertSource(idDir)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agent: bad CA PEM")
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:              pool,
			GetClientCertificate: src.GetClientCertificate,
		},
		IdleConnTimeout: idleConnTimeout,
	}}, nil
}

// Gateway runs the loopback reverse-proxy: it accepts plaintext HTTP from
// in-container Claude on listenAddr and forwards to the real broker at brokerURL
// over mTLS, presenting the rotating agent leaf from idDir. Blocks until the
// listener closes (process signal).
func Gateway(listenAddr, brokerURL string, caPEM []byte, idDir string) error {
	if err := requireLoopback(listenAddr); err != nil {
		return err
	}
	proxy, err := newGatewayProxy(brokerURL, caPEM, idDir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen %s: %w", listenAddr, err)
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// requireLoopback fails closed unless listenAddr binds a loopback IP. The proxy
// presents the agent's private key, so it must never be reachable off-host
// (mirrors the broker admin-listener guard in internal/broker/server.go).
func requireLoopback(listenAddr string) error {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("gateway: parse listen addr %q: %w", listenAddr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("gateway: listen addr must be loopback, got %q", listenAddr)
	}
	return nil
}

// newGatewayProxy builds the mTLS reverse-proxy to brokerURL. Split from Gateway
// so tests can drive the proxy handler directly without binding a socket.
func newGatewayProxy(brokerURL string, caPEM []byte, idDir string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(brokerURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse broker URL %q: %w", brokerURL, err)
	}
	src, err := newClientCertSource(idDir)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("gateway: bad CA PEM")
	}
	// target is the broker origin (no path), so SingleHostReverseProxy forwards the
	// incoming path verbatim (/mcp/<tool>/…, /llm) with method, body, and headers.
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:              pool,
			GetClientCertificate: src.GetClientCertificate,
		},
		IdleConnTimeout: idleConnTimeout,
	}
	// MCP tool responses stream over SSE; -1 flushes every write immediately so
	// streamed chunks reach Claude without buffering (the default buffers and
	// streaming tool calls stall).
	proxy.FlushInterval = -1
	return proxy, nil
}
