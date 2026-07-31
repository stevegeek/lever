package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
)

// LapseFunc is the natural-lapse observation hook: it is invoked during a
// handshake whose client cert is OUR CA's signature over an otherwise-valid
// client-auth leaf that is merely expired — the one failure shape that means
// "this identity aged out", as opposed to a foreign, forged, or malformed
// cert. The handshake is still REJECTED; the hook only observes. It runs on
// the handshake goroutine, so implementations must not block (send to a
// buffered channel and return).
type LapseFunc func(cn string)

// ServerTLSConfig builds a TLS config that verifies a client cert *if one is
// presented* (so the certless /enrol handshake can occur) and otherwise serves
// with the given server cert/key. Per-route enforcement uses RequireAgent.
func (c *CA) ServerTLSConfig(serverCertPEM, serverKeyPEM []byte) (*tls.Config, error) {
	return c.ServerTLSConfigLapse(serverCertPEM, serverKeyPEM, nil)
}

// ServerTLSConfigLapse is ServerTLSConfig with a natural-lapse observation
// hook (nil = no observation, identical enforcement).
func (c *CA) ServerTLSConfigLapse(serverCertPEM, serverKeyPEM []byte, onLapse LapseFunc) (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: server keypair: %w", err)
	}
	cfg := c.clientVerifyConfig(onLapse)
	cfg.Certificates = []tls.Certificate{serverCert}
	return cfg, nil
}

// ServerTLSConfigSource is ServerTLSConfig with a rotating serving cert: the
// source re-mints before expiry, so a broker running past certTTL keeps
// serving a valid cert instead of failing every handshake. onLapse (may be
// nil) observes natural-lapse client certs — see LapseFunc.
func (c *CA) ServerTLSConfigSource(src *ServerCertSource, onLapse LapseFunc) *tls.Config {
	cfg := c.clientVerifyConfig(onLapse)
	cfg.GetCertificate = src.GetCertificate
	return cfg
}

// clientVerifyConfig builds the shared client-auth posture: certless
// connections are allowed (per-route RequireAgent gates identity), presented
// certs are FULLY verified by verifyClientCert. ClientAuth is RequestClientCert
// (not VerifyClientCertIfGiven) because the stdlib's own verification cannot
// distinguish "expired but ours" from "invalid" — verification is ours, in
// VerifyConnection, with identical accept/reject outcomes plus the lapse
// observation. Consequently VerifiedChains is never populated; identity comes
// from PeerCertificates[0] (AgentFromConnState), which is safe precisely
// because VerifyConnection has verified the chain on every cert-bearing
// connection before any request is served.
func (c *CA) clientVerifyConfig(onLapse LapseFunc) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return &tls.Config{
		ClientAuth:       tls.RequestClientCert,
		ClientCAs:        pool, // informational (CertificateRequest hints); verification is VerifyConnection's
		MinVersion:       tls.VersionTLS12,
		VerifyConnection: c.verifyClientCert(pool, onLapse),
	}
}

// verifyClientCert returns the VerifyConnection callback: certless
// connections pass (per-route gates enforce identity); a presented cert must
// chain to our CA as a client-auth leaf. On failure the handshake is rejected
// exactly as stdlib verification would — with one addition: when the ONLY
// defect is expiry (the chain verifies at a time inside the leaf's own
// validity window), the lapse hook is invoked with the leaf CN before the
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
		if onLapse != nil && errors.As(err, &invalid) && invalid.Reason == x509.Expired && leaf.Subject.CommonName != "" {
			// Re-verify at a time when BOTH the leaf and our CA were valid (the
			// midpoint of their windows' intersection — a cert our CA really
			// signed always has one): if the chain verifies there, the sole
			// defect now is time — a natural lapse, not a forgery. x509.Verify
			// checks every chain cert's window at CurrentTime, so using the
			// leaf's own midpoint alone would false-negative whenever it falls
			// outside the CA's window.
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

// AgentFromConnState returns the agent identity (client cert CommonName) from
// an mTLS connection. The cert in PeerCertificates has ALWAYS been verified by
// the time a request handler runs: every server config built by this package
// installs VerifyConnection (clientVerifyConfig), which rejects the handshake
// for any presented cert that does not verify against the instance CA. Fails
// closed if no client cert is present.
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
// verified caller identity, or an error if the request carried no verified
// client cert. Fails closed.
func RequireAgent(r *http.Request) (string, error) {
	if r.TLS == nil {
		return "", errors.New("ca: request has no TLS state")
	}
	return AgentFromConnState(*r.TLS)
}
