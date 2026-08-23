package brokerctl

// agentclient_test.go holds the simulated-agent helpers shared by the
// integration tests: a CA-issued client cert and an mTLS client that pins the
// broker CA.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// csrWithKey returns a PEM CSR for cn plus the matching EC private key PEM, so the
// test can present the CA-signed cert as a client (the simulated worker).
func csrWithKey(t *testing.T, cn string) (csrPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// serverName is the orbstack HostToolAlias, which is what these integration
// tests issue the broker's server cert for and verify against.
const serverName = "host.orb.internal"

// workerClient builds an mTLS client that pins the broker CA and presents the
// worker's CA-issued cert, dialing 127.0.0.1 but verifying the server cert
// against the broker's serverName (host.orb.internal) — the OrbStack hostname the
// real server cert is issued for.
func workerClient(t *testing.T, caInst *ca.CA, cert tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(caInst.Cert)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		ServerName:   serverName, // "host.orb.internal"
		Certificates: []tls.Certificate{cert},
	}}}
}

// workerCert issues a CA-signed client cert for cn (the simulated-agent technique
// from the broker e2e: skip provision/enrol, mint the leaf directly from the CA).
func workerCert(t *testing.T, caInst *ca.CA, cn string) tls.Certificate {
	t.Helper()
	csrPEM, keyPEM := csrWithKey(t, cn)
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}
