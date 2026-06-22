package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ServerTLSConfig builds a TLS config that REQUIRES and verifies a client cert
// signed by this CA, using the given server cert/key for the server side.
func (c *CA) ServerTLSConfig(serverCertPEM, serverKeyPEM []byte) (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: server keypair: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
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
