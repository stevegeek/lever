package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serverFor(t *testing.T, c *CA, h http.Handler) *httptest.Server {
	t.Helper()
	certPEM, keyPEM, err := c.IssueServerCert("example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.ServerTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = cfg
	srv.StartTLS()
	return srv
}

// TestVerifyIfGivenAllowsCertlessAndProvesIdentityWhenGiven exercises both
// paths against one server: certless requests reach the handler (RequireAgent
// returns an error -> 401), and cert-bearing requests recover the CN.
func TestVerifyIfGivenAllowsCertlessAndProvesIdentityWhenGiven(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, err := RequireAgent(r)
		if err != nil {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, agent)
	})
	srv := serverFor(t, c, h)
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)

	// (a) Certless request: handshake succeeds, handler runs, RequireAgent -> 401.
	certless := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: "example.test",
	}}}
	resp, err := certless.Get(srv.URL)
	if err != nil {
		t.Fatalf("certless request should reach the handler: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("certless status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// (b) Cert-bearing request: recovers the CN. Build a keypair + CSR, sign it.
	clientCert := signedClientCert(t, c, "scratch")
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{clientCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	resp2, err := withCert.Get(srv.URL)
	if err != nil {
		t.Fatalf("cert request: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body) != "scratch" {
		t.Errorf("recovered agent = %q, want scratch", string(body))
	}
}

func TestMTLSRejectsCertFromDifferentCA(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	srv := serverFor(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	foreign := signedClientCert(t, other, "scratch") // signed by a DIFFERENT CA
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{foreign}, RootCAs: pool, ServerName: "example.test",
	}}}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("expected handshake failure: client cert not signed by the server's CA")
	}
}

func TestAgentFromConnStateFailsClosed(t *testing.T) {
	if _, err := AgentFromConnState(tls.ConnectionState{}); err == nil {
		t.Fatal("expected error: no verified client certificate")
	}
}

func TestAgentFromConnStateRejectsEmptyCN(t *testing.T) {
	cs := tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{&x509.Certificate{Subject: pkix.Name{CommonName: ""}}}},
	}
	if _, err := AgentFromConnState(cs); err == nil {
		t.Fatal("expected error: verified client cert with empty common name")
	}
}

func signedClientCert(t *testing.T, c *CA, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	certPEM, err := c.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}
