package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// LapseFunc observes a handshake whose client cert is an otherwise-valid
// client-auth leaf signed by our CA that has merely expired ("this identity
// aged out", as opposed to a foreign, forged or malformed cert). The
// handshake is still rejected. It runs on the handshake goroutine, so it
// must not block.
type LapseFunc func(cn string)

// ServerTLSConfigSource builds a TLS config that serves src's rotating cert
// and verifies a client cert only if one is presented, so the certless
// /enrol handshake works; per-route enforcement uses RequireAgent. onLapse
// may be nil.
//
// ClientAuth is RequestClientCert, not VerifyClientCertIfGiven, because the
// stdlib cannot tell "expired but ours" from "invalid": verifyClientCert in
// VerifyConnection gives the same accept/reject outcomes plus the lapse
// observation. VerifiedChains is therefore never populated; identity comes
// from PeerCertificates[0], which is safe only because VerifyConnection has
// verified the chain on every cert-bearing connection before any request is
// served.
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
// When the only defect is expiry, onLapse gets the leaf CN before the
// rejection is returned.
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
			leaf.Subject.CommonName != "" && time.Now().After(leaf.NotAfter) {
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
					onLapse(leaf.Subject.CommonName)
				}
			}
		}
		return fmt.Errorf("ca: client certificate verification failed: %w", err)
	}
}

// AgentFromConnState returns the client cert CommonName of an mTLS
// connection. It relies on VerifyConnection (ServerTLSConfigSource) having
// verified any presented cert against the instance CA before a handler
// runs. Fails closed if no client cert is present.
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
	return AgentFromConnState(*r.TLS)
}
