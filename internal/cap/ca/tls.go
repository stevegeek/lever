package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
)

// ServerTLSConfig builds a TLS config that verifies a client cert *if one is
// presented* (so the certless /enrol handshake can occur) and otherwise serves
// with the given server cert/key. Per-route enforcement uses RequireAgent.
func (c *CA) ServerTLSConfig(serverCertPEM, serverKeyPEM []byte) (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: server keypair: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// AgentFromConnState returns the agent identity (client cert CommonName) from a
// verified mTLS connection. Fails closed if no verified client cert is present.
func AgentFromConnState(cs tls.ConnectionState) (string, error) {
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return "", errors.New("ca: no verified client certificate")
	}
	cn := cs.VerifiedChains[0][0].Subject.CommonName
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
