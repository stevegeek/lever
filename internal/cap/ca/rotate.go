package ca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"time"
)

// rotateMargin is how much remaining validity triggers a re-mint. A quarter of
// certTTL keeps rotation comfortably ahead of expiry while avoiding churn.
const rotateMargin = certTTL / 4

// ServerCertSource is a self-rotating source of the broker's serving cert for
// tls.Config.GetCertificate. The broker process holds the CA key, so it can
// re-mint its own leaf: without this, a broker running longer than certTTL
// serves an expired cert and every gateway handshake fails (agents then cannot
// even reach /renew to keep their own leafs fresh).
type ServerCertSource struct {
	ca  *CA
	cn  string
	dns []string
	ips []string
	now func() time.Time // test seam; nil means time.Now

	mu   sync.Mutex
	cert *tls.Certificate
}

// NewServerCertSource builds a rotating serving-cert source with the same SAN
// semantics as IssueServerCertSANs. It mints eagerly so bad inputs (e.g. an
// invalid IP SAN) fail at startup, not on the first handshake.
func (c *CA) NewServerCertSource(cn string, dnsNames, ips []string) (*ServerCertSource, error) {
	s := &ServerCertSource{ca: c, cn: cn, dns: dnsNames, ips: ips}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mintLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetCertificate returns the current serving cert, re-minting when less than
// rotateMargin of its validity remains. Shaped for tls.Config.GetCertificate;
// the hello is unused (one cert covers all SANs).
func (s *ServerCertSource) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowFn := s.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if now.After(s.cert.Leaf.NotAfter.Add(-rotateMargin)) {
		if err := s.mintLocked(); err != nil {
			// Fail soft: inside the margin the cached cert may still be valid —
			// serve it rather than failing handshakes. Error only once expired.
			if now.After(s.cert.Leaf.NotAfter) {
				return nil, fmt.Errorf("ca: rotate server cert: %w", err)
			}
		}
	}
	return s.cert, nil
}

// mintLocked re-mints the leaf. Caller holds s.mu.
func (s *ServerCertSource) mintLocked() error {
	certPEM, keyPEM, err := s.ca.IssueServerCertSANs(s.cn, s.dns, s.ips)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("ca: server keypair: %w", err)
	}
	if pair.Leaf == nil { // older Go leaves Leaf unparsed
		block, _ := pem.Decode(certPEM)
		if block == nil {
			return fmt.Errorf("ca: minted cert PEM is invalid")
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("ca: parse minted leaf: %w", err)
		}
		pair.Leaf = leaf
	}
	s.cert = &pair
	return nil
}
