// Package servicehost runs an instance's MCP servers on another machine:
// `lever service-host serve` on that machine, and the pinned mutual-TLS
// pairing the broker uses to reach it.
package servicehost

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Fingerprint is "sha256:" and the lowercase hex SHA-256 of a certificate's
// SubjectPublicKeyInfo.
type Fingerprint string

var fingerprintRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseFingerprint accepts upper- or lowercase hex and returns the canonical
// lowercase form.
func ParseFingerprint(s string) (Fingerprint, error) {
	f := strings.ToLower(strings.TrimSpace(s))
	if !fingerprintRE.MatchString(f) {
		return "", fmt.Errorf("fingerprint %q: want sha256: and 64 hex digits", s)
	}
	return Fingerprint(f), nil
}

// FingerprintOf pins the key, not the certificate, so re-issuing a
// certificate for the same key keeps the pairing.
func FingerprintOf(cert *x509.Certificate) Fingerprint {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return Fingerprint("sha256:" + hex.EncodeToString(sum[:]))
}

func (f Fingerprint) matches(cert *x509.Certificate) bool {
	if f == "" || cert == nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(FingerprintOf(cert)), []byte(f)) == 1
}

// ErrPinMismatch is a peer whose key is not the pinned one.
var ErrPinMismatch = errors.New("certificate pin mismatch")

// GenerateSelfSigned makes an ECDSA P-256 key and a self-signed certificate
// for it. The dates are not checked by either side: the pin is the identity.
func GenerateSelfSigned(cn string, eku x509.ExtKeyUsage) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

// LoadKeyPair reads a PEM certificate and key and returns the pair and the
// key's fingerprint.
func LoadKeyPair(certPEM, keyPEM []byte) (tls.Certificate, Fingerprint, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", err
	}
	c.Leaf = leaf
	return c, FingerprintOf(leaf), nil
}

// verifyPin is a VerifyConnection hook, which unlike VerifyPeerCertificate
// also runs on a resumed session.
func verifyPin(pin Fingerprint) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no peer certificate")
		}
		if !pin.matches(cs.PeerCertificates[0]) {
			return fmt.Errorf("%w: got %s", ErrPinMismatch, FingerprintOf(cs.PeerCertificates[0]))
		}
		return nil
	}
}

// ServerTLSConfig requires a client certificate whose key is clientPin.
// Session tickets are off so every connection proves the client key afresh.
func ServerTLSConfig(cert tls.Certificate, clientPin Fingerprint) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		ClientAuth:             tls.RequireAnyClientCert,
		VerifyConnection:       verifyPin(clientPin),
		SessionTicketsDisabled: true,
	}
}

// ClientTLSConfig presents cert and accepts only a server whose key is
// serverPin. Chain and host name verification are off because the pin
// replaces them; VerifyConnection always runs.
func ClientTLSConfig(cert tls.Certificate, serverPin Fingerprint) *tls.Config {
	return clientTLSConfig(func() (*tls.Certificate, error) { return &cert, nil }, serverPin)
}

func clientTLSConfig(cert func() (*tls.Certificate, error), serverPin Fingerprint) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Always sent: the Certificates field would withhold it from a server
		// that names acceptable CAs the self-signed pair is not under.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert() },
		InsecureSkipVerify:   true,
		VerifyConnection:     verifyPin(serverPin),
	}
}
