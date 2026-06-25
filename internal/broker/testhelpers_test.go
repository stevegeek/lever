package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/lever-to/lever/internal/broker/registry"
)

// makeCSRForCN builds a PEM CSR for cn and discards the private key (the broker
// only ever sees the CSR). Returns the CSR PEM.
func makeCSRForCN(t *testing.T, cn string) []byte {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// csrWithKey returns both the CSR PEM and the private key PEM, for tests that
// must present the resulting cert as a client (enrol/renew/gateway).
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

func base64urlNoPad(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func regTool(name, backend, op string) registry.Tool {
	return registry.Tool{Name: name, Backend: backend,
		Operations: map[string]registry.Operation{op: {Name: op}}}
}
