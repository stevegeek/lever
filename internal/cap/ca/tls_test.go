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
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serverTLSConfig builds a serving config for "example.test" over the
// production path (rotating cert source + ServerTLSConfigSource).
func serverTLSConfig(t *testing.T, c *CA, onLapse LapseFunc) *tls.Config {
	t.Helper()
	src, err := c.NewServerCertSource("example.test", []string{"example.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.ServerTLSConfigSource(src, onLapse)
}

func serverFor(t *testing.T, c *CA, h http.Handler) *httptest.Server {
	t.Helper()
	cfg := serverTLSConfig(t, c, nil)
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
		t.Fatal("expected error: no client certificate")
	}
}

func TestAgentFromConnStateRejectsEmptyCN(t *testing.T) {
	cs := tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: ""}}},
	}
	if _, err := AgentFromConnState(cs); err == nil {
		t.Fatal("expected error: client cert with empty common name")
	}
}

// expiredClientCert signs a client-auth leaf with our CA whose validity window
// is entirely in the past — the "natural lapse" shape: OUR signature, valid in
// every way except time.
func expiredClientCert(t *testing.T, c *CA, cn string) tls.Certificate {
	t.Helper()
	// Inside the CA's own window (Generate backdates it 1h) but ended in the
	// past — the realistic natural-lapse shape.
	return caSignedCert(t, c, cn,
		time.Now().Add(-50*time.Minute), time.Now().Add(-10*time.Minute),
		x509.ExtKeyUsageClientAuth)
}

// caSignedCert signs a leaf with our CA with an arbitrary validity window and
// EKU, for building the near-miss shapes the lapse classifier must reject.
func caSignedCert(t *testing.T, c *CA, cn string, notBefore, notAfter time.Time, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// lapseServerFor is serverFor with a lapse hook wired.
func lapseServerFor(t *testing.T, c *CA, onLapse LapseFunc) *httptest.Server {
	t.Helper()
	cfg := serverTLSConfig(t, c, onLapse)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = cfg
	srv.StartTLS()
	return srv
}

// TestLapseHookFiresOnlyForOurExpiredCert: the handshake must REJECT an
// expired leaf in every case; the hook must fire only when the leaf is OUR
// CA's, otherwise-valid, merely expired — never for a foreign CA's cert
// (expired or not) and never for a valid cert.
func TestLapseHookFiresOnlyForOurExpiredCert(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	lapses := make(chan string, 4)
	srv := lapseServerFor(t, c, func(cn string) { lapses <- cn })
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)

	dial := func(cert *tls.Certificate) error {
		cfg := &tls.Config{RootCAs: pool, ServerName: "example.test"}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := client.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	drainLapse := func() (string, bool) {
		select {
		case cn := <-lapses:
			return cn, true
		case <-time.After(200 * time.Millisecond):
			return "", false
		}
	}

	// (a) our expired cert: handshake fails, hook fires with the CN.
	ours := expiredClientCert(t, c, "scratch")
	if err := dial(&ours); err == nil {
		t.Fatal("expired cert must fail the handshake")
	}
	if cn, ok := drainLapse(); !ok || cn != "scratch" {
		t.Fatalf("lapse hook: got (%q,%v), want (scratch,true)", cn, ok)
	}

	// (b) foreign CA's EXPIRED cert: handshake fails, no hook.
	foreignExpired := expiredClientCert(t, other, "scratch")
	if err := dial(&foreignExpired); err == nil {
		t.Fatal("foreign expired cert must fail the handshake")
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a foreign CA's cert, fired with %q", cn)
	}

	// (c) foreign CA's VALID cert: handshake fails, no hook.
	foreignValid := signedClientCert(t, other, "scratch")
	if err := dial(&foreignValid); err == nil {
		t.Fatal("foreign valid cert must fail the handshake")
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a foreign CA's valid cert, fired with %q", cn)
	}

	// (d) our valid cert: handshake succeeds, no hook.
	valid := signedClientCert(t, c, "scratch")
	if err := dial(&valid); err != nil {
		t.Fatalf("valid cert handshake: %v", err)
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a valid cert, fired with %q", cn)
	}

	// (e) certless: handshake succeeds (per-route gates enforce), no hook.
	if err := dial(nil); err != nil {
		t.Fatalf("certless handshake: %v", err)
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a certless connection, fired with %q", cn)
	}

	// (f) our NOT-YET-VALID cert (future window): x509 reports this with the
	// same Reason (Expired) as a genuine age-out, but it is clock skew, not a
	// lapse — handshake fails, no hook, no automated bounce.
	future := caSignedCert(t, c, "scratch",
		time.Now().Add(10*time.Minute), time.Now().Add(50*time.Minute),
		x509.ExtKeyUsageClientAuth)
	if err := dial(&future); err == nil {
		t.Fatal("not-yet-valid cert must fail the handshake")
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a not-yet-valid cert, fired with %q", cn)
	}

	// (g) our EXPIRED cert with the WRONG EKU (server-auth): the time defect
	// surfaces first in Verify, but the midpoint re-verify must catch the EKU
	// mismatch — handshake fails, no hook.
	wrongEKU := caSignedCert(t, c, "scratch",
		time.Now().Add(-50*time.Minute), time.Now().Add(-10*time.Minute),
		x509.ExtKeyUsageServerAuth)
	if err := dial(&wrongEKU); err == nil {
		t.Fatal("expired wrong-EKU cert must fail the handshake")
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for an expired wrong-EKU cert, fired with %q", cn)
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
