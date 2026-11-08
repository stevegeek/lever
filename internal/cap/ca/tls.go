package ca

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// LapseFunc observes a client that proved it holds the key of an
// otherwise-valid client-auth leaf signed by our CA that has merely expired
// ("this identity aged out", as opposed to a foreign, forged or malformed
// cert). Its request is still refused. It runs on a request goroutine, so it
// must not block.
type LapseFunc func(cn string)

// LapseGrace is how long past its NotAfter a leaf must be before the
// handshake lets it through as a lapse. A server using onLapse must bound its
// handshake well below it (the jail server's handshake deadline is its 10s
// ReadHeaderTimeout): then a leaf the handshake let through as a lapse had
// expired before its connection was accepted, which is what LapseGate reports.
// A leaf less than LapseGrace past its NotAfter fails the handshake as
// before; the client's next attempt after the grace heals it.
const LapseGrace = time.Minute

// The handshake cannot prove a lapse by itself: Go runs VerifyConnection
// after the client's Certificate message but before its CertificateVerify,
// so at that point anyone holding a copy of the (public) expired cert looks
// the same as the agent holding its key. So a lapse-shaped leaf passes the
// handshake, which then checks the key, and LapseGate reports and refuses it
// on its first request. Any other defective leaf still fails the handshake.

// ServerTLSConfigSource builds a TLS config that serves src's rotating cert
// and verifies a client cert only if one is presented, so the certless
// /enrol handshake works; per-route enforcement uses RequireAgent. onLapse
// may be nil.
//
// ClientAuth is RequestClientCert, not VerifyClientCertIfGiven, because the
// stdlib cannot tell "expired but ours" from "invalid": verifyClientCert in
// VerifyConnection makes that call. VerifiedChains is therefore never
// populated; identity comes from PeerCertificates[0], which is safe only
// because VerifyConnection has verified the chain on every cert-bearing
// connection before any request is served, and because LapseGate and
// RequireAgent refuse a leaf that had expired when the connection was
// accepted.
//
// With onLapse non-nil the server must be built with ConnContext and its
// handler wrapped in LapseGate: a lapsed leaf then completes the handshake.
func (c *CA) ServerTLSConfigSource(src *ServerCertSource, onLapse LapseFunc) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return &tls.Config{
		ClientAuth:       tls.RequestClientCert,
		ClientCAs:        pool, // informational (CertificateRequest hints); verification is VerifyConnection's
		MinVersion:       tls.VersionTLS12,
		VerifyConnection: c.verifyClientCert(pool, onLapse),
		GetCertificate:   src.GetCertificate,
	}
}

// verifyClientCert lets certless connections pass (per-route gates enforce
// identity); a presented cert must chain to our CA as a client-auth leaf.
// With onLapse non-nil, a leaf whose only defect is that it expired more than
// LapseGrace ago also passes, for LapseGate to report and refuse once the
// handshake has proved the client holds its key.
func (c *CA) verifyClientCert(pool *x509.CertPool, onLapse LapseFunc) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return nil
		}
		leaf := cs.PeerCertificates[0]
		opts := x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		for _, ic := range cs.PeerCertificates[1:] {
			if opts.Intermediates == nil {
				opts.Intermediates = x509.NewCertPool()
			}
			opts.Intermediates.AddCert(ic)
		}
		_, err := leaf.Verify(opts)
		if err == nil {
			return nil
		}
		var invalid x509.CertificateInvalidError
		// x509 reports Expired for both aged-out and not-yet-valid. Only the
		// former is a lapse: a future-dated leaf (broker clock stepped back)
		// must not trigger an automated agent bounce.
		if onLapse != nil && errors.As(err, &invalid) && invalid.Reason == x509.Expired &&
			leaf.Subject.CommonName != "" && time.Now().After(leaf.NotAfter.Add(LapseGrace)) {
			// Re-verify at the midpoint of the leaf's and the CA's validity
			// intersection: if the chain verifies there, the sole defect is time,
			// not forgery. x509.Verify checks every chain cert at CurrentTime, so
			// the leaf's own midpoint alone could fall outside the CA's window.
			lo, hi := leaf.NotBefore, leaf.NotAfter
			if c.Cert.NotBefore.After(lo) {
				lo = c.Cert.NotBefore
			}
			if c.Cert.NotAfter.Before(hi) {
				hi = c.Cert.NotAfter
			}
			if lo.Before(hi) {
				optsAt := opts
				optsAt.CurrentTime = lo.Add(hi.Sub(lo) / 2)
				if _, err2 := leaf.Verify(optsAt); err2 == nil {
					return nil // a lapse: LapseGate refuses it after the key check
				}
			}
		}
		return fmt.Errorf("ca: client certificate verification failed: %w", err)
	}
}

type acceptTimeKey struct{}

// ConnContext is an http.Server ConnContext that records when the connection
// was accepted, before its handshake. LapseGate and RequireAgent compare a
// leaf's NotAfter with it.
func ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, acceptTimeKey{}, time.Now())
}

// lapsedLeaf classifies r's client leaf. refuse: the leaf had expired when
// its connection was accepted, or is now more than LapseGrace past its
// NotAfter. The second covers every leaf the handshake let through as a lapse
// whatever the clocks did during the handshake, and a keep-alive connection
// whose leaf expired while it stayed open (one refusal; the client reconnects
// with its renewed leaf). report: the leaf is refused and had expired, by more
// than LapseGrace, before the connection was accepted, so the handshake that
// proved its key also found it lapsed; only this asks for a heal. Without a
// recorded accept time (no ConnContext) a leaf that has expired now is
// refused and never reported.
func lapsedLeaf(r *http.Request) (cn string, refuse, report, known bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false, false, true
	}
	leaf := r.TLS.PeerCertificates[0]
	now := time.Now()
	accepted, ok := r.Context().Value(acceptTimeKey{}).(time.Time)
	if !ok {
		return leaf.Subject.CommonName, now.After(leaf.NotAfter), false, false
	}
	beforeAccept := leaf.NotAfter.Before(accepted)
	pastGrace := now.After(leaf.NotAfter.Add(LapseGrace))
	return leaf.Subject.CommonName, beforeAccept || pastGrace, beforeAccept && pastGrace, true
}

// LapseGate wraps the whole handler of a server built from
// ServerTLSConfigSource, so that no route, certless ones included, serves a
// connection whose leaf had lapsed (net/http answers "OPTIONS *" itself,
// with no identity). Such a request is refused with 503 and Retry-After, a
// transient answer: a 401 would send Claude Code into a login or OAuth flow.
// A lapse (see lapsedLeaf) is reported to onLapse (nil: not reported); the
// completed handshake proved the client holds the leaf's key and that the
// leaf is ours. log gets a line naming the CN.
func LapseGate(next http.Handler, onLapse LapseFunc, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn, refuse, report, known := lapsedLeaf(r)
		if !refuse {
			next.ServeHTTP(w, r)
			return
		}
		// lever doctor's agent-certificate check looks for this wording.
		log.Warn("refused a client whose certificate has expired", "cn", cn, "remote", r.RemoteAddr)
		if !known && onLapse != nil {
			log.Error("lapse healer: the server records no accept time (ca.ConnContext unset); the lapse is not reported", "cn", cn)
		}
		if report && onLapse != nil && cn != "" {
			onLapse(cn)
		}
		w.Header().Set("Connection", "close")
		w.Header().Set("Retry-After", "30")
		http.Error(w, "client certificate has expired; the broker re-enrols this agent", http.StatusServiceUnavailable)
	})
}

// AgentFromConnState returns the client cert CommonName of an mTLS
// connection. It relies on VerifyConnection (ServerTLSConfigSource) having
// verified any presented cert against the instance CA before a handler
// runs; RequireAgent also refuses a lapsed leaf. Fails closed if no client
// cert is present.
func AgentFromConnState(cs tls.ConnectionState) (string, error) {
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("ca: no client certificate")
	}
	cn := cs.PeerCertificates[0].Subject.CommonName
	if cn == "" {
		return "", errors.New("ca: client certificate has empty common name")
	}
	return cn, nil
}

// RequireAgent is the per-route guard for gated handlers: it returns the
// verified caller identity, or an error when there is none. Fails closed.
func RequireAgent(r *http.Request) (string, error) {
	if r.TLS == nil {
		return "", errors.New("ca: request has no TLS state")
	}
	// LapseGate refuses these first; this keeps a route safe without it.
	if _, refuse, _, _ := lapsedLeaf(r); refuse {
		return "", errors.New("ca: client certificate has expired")
	}
	return AgentFromConnState(*r.TLS)
}
