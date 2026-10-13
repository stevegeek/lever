package ca

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"
)

// rotateMargin is how much remaining validity triggers a re-mint.
const rotateMargin = certTTL / 4

// ServerCertSource is a self-rotating source of the broker's serving cert for
// tls.Config.GetCertificate. Without it a broker running longer than certTTL
// serves an expired cert, and agents cannot even reach /renew.
type ServerCertSource struct {
	ca  *CA
	cn  string
	dns []string
	ips []string
	now func() time.Time // test seam; nil means time.Now

	mu   sync.Mutex
	cert *tls.Certificate
}

// NewServerCertSource mints eagerly so bad inputs (an invalid IP SAN) fail at
// startup, not on the first handshake.
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
// rotateMargin of its validity remains.
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
			// Fail soft: inside the margin the cached cert is still valid. Error
			// only once it has expired.
			if now.After(s.cert.Leaf.NotAfter) {
				return nil, fmt.Errorf("ca: rotate server cert: %w", err)
			}
		}
	}
	return s.cert, nil
}

// mintLocked: caller holds s.mu.
func (s *ServerCertSource) mintLocked() error {
	certPEM, keyPEM, err := s.ca.IssueServerCertSANs(s.cn, s.dns, s.ips)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("ca: server keypair: %w", err)
	}
	s.cert = &pair
	return nil
}
