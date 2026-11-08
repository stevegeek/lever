package ca

import (
	"context"
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

// One server: certless requests reach the handler (RequireAgent -> 401) and
// cert-bearing requests recover the CN.
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

func lapseServerFor(t *testing.T, c *CA, onLapse LapseFunc) *httptest.Server {
	t.Helper()
	cfg := serverTLSConfig(t, c, onLapse)
	srv := httptest.NewUnstartedServer(LapseGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), onLapse, nil))
	srv.Config.ConnContext = ConnContext
	srv.TLS = cfg
	srv.StartTLS()
	return srv
}

// A lapse-shaped leaf completes the handshake (which proves the key) and is
// refused with 401 by LapseGate, which fires the hook. Every other expired or
// foreign leaf fails the handshake, and the hook never fires for it.
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

	// dial returns the HTTP status, or an error when the handshake fails.
	dial := func(cert *tls.Certificate) (int, error) {
		cfg := &tls.Config{RootCAs: pool, ServerName: "example.test"}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		defer client.CloseIdleConnections()
		resp, err := client.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	drainLapse := func() (string, bool) {
		select {
		case cn := <-lapses:
			return cn, true
		case <-time.After(200 * time.Millisecond):
			return "", false
		}
	}
	mustFailHandshake := func(what string, cert *tls.Certificate) {
		t.Helper()
		if code, err := dial(cert); err == nil {
			t.Fatalf("%s must fail the handshake, got HTTP %d", what, code)
		}
		if cn, ok := drainLapse(); ok {
			t.Fatalf("lapse hook must not fire for %s, fired with %q", what, cn)
		}
	}

	// (a) our expired cert, with its key: handshake succeeds, the request is
	// refused, the hook fires with the CN.
	ours := expiredClientCert(t, c, "scratch")
	if code, err := dial(&ours); err != nil || code != http.StatusServiceUnavailable {
		t.Fatalf("lapsed cert: got (%d, %v), want 503 after a completed handshake", code, err)
	}
	if cn, ok := drainLapse(); !ok || cn != "scratch" {
		t.Fatalf("lapse hook: got (%q,%v), want (scratch,true)", cn, ok)
	}

	// (a2) the same expired cert presented WITHOUT its key (someone holding a
	// copy of the public cert): the handshake's CertificateVerify fails, and
	// the hook must not fire. This is the case the gate exists for: Go runs
	// VerifyConnection before CertificateVerify.
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	copied := tls.Certificate{Certificate: ours.Certificate, PrivateKey: stranger}
	mustFailHandshake("an expired cert presented without its key", &copied)

	// (b) foreign CA's EXPIRED cert.
	foreignExpired := expiredClientCert(t, other, "scratch")
	mustFailHandshake("a foreign CA's expired cert", &foreignExpired)

	// (c) foreign CA's VALID cert.
	foreignValid := signedClientCert(t, other, "scratch")
	mustFailHandshake("a foreign CA's valid cert", &foreignValid)

	// (d) our valid cert: served, no hook.
	valid := signedClientCert(t, c, "scratch")
	if code, err := dial(&valid); err != nil || code != http.StatusOK {
		t.Fatalf("valid cert: got (%d, %v), want 200", code, err)
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a valid cert, fired with %q", cn)
	}

	// (e) certless: served (per-route gates enforce identity), no hook.
	if code, err := dial(nil); err != nil || code != http.StatusOK {
		t.Fatalf("certless: got (%d, %v), want 200", code, err)
	}
	if cn, ok := drainLapse(); ok {
		t.Fatalf("lapse hook must not fire for a certless connection, fired with %q", cn)
	}

	// (f) our NOT-YET-VALID cert (future window): x509 reports this with the
	// same Reason (Expired) as a genuine age-out, but it is clock skew, not a
	// lapse.
	future := caSignedCert(t, c, "scratch",
		time.Now().Add(10*time.Minute), time.Now().Add(50*time.Minute),
		x509.ExtKeyUsageClientAuth)
	mustFailHandshake("a not-yet-valid cert", &future)

	// (g) our EXPIRED cert with the WRONG EKU (server-auth): the time defect
	// surfaces first in Verify, but the midpoint re-verify must catch the EKU
	// mismatch.
	wrongEKU := caSignedCert(t, c, "scratch",
		time.Now().Add(-50*time.Minute), time.Now().Add(-10*time.Minute),
		x509.ExtKeyUsageServerAuth)
	mustFailHandshake("an expired wrong-EKU cert", &wrongEKU)

	// (h) our cert expired less than LapseGrace ago: it fails the handshake
	// as before, so a connection accepted before its NotAfter can never pass
	// as a lapse or as valid.
	justExpired := caSignedCert(t, c, "scratch",
		time.Now().Add(-50*time.Minute), time.Now().Add(-LapseGrace/2),
		x509.ExtKeyUsageClientAuth)
	mustFailHandshake("a cert within the lapse grace", &justExpired)
}

// Without a hook the server keeps refusing every expired leaf in the
// handshake.
func TestNoLapseHookRejectsExpiredInHandshake(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	srv := lapseServerFor(t, c, nil)
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	ours := expiredClientCert(t, c, "scratch")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: "example.test", Certificates: []tls.Certificate{ours},
	}}}
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatalf("expired cert without a lapse hook must fail the handshake, got HTTP %d", resp.StatusCode)
	}
}

// lapsedLeaf refuses a leaf that had expired at accept or is now past the
// grace, and reports only one that was past the grace before the accept.
// Without a recorded accept time it fails closed and never reports.
func TestLapsedLeafUsesTheAcceptTime(t *testing.T) {
	now := time.Now()
	req := func(notAfter time.Time, accepted *time.Time) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: "scratch"}, NotAfter: notAfter,
		}}}
		if accepted != nil {
			r = r.WithContext(context.WithValue(r.Context(), acceptTimeKey{}, *accepted))
		}
		return r
	}
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	cases := []struct {
		name                  string
		notAfter              time.Time
		accepted              *time.Time
		refuse, report, known bool
	}{
		{"lapsed before accept", now.Add(-3 * time.Minute), at(-time.Minute), true, true, true},
		{"expired just before accept", now.Add(-30 * time.Second), at(-10 * time.Second), true, false, true},
		{"expired while open, within grace", now.Add(-30 * time.Second), at(-2 * time.Minute), false, false, true},
		{"expired while open, past grace", now.Add(-2 * time.Minute), at(-time.Hour), true, false, true},
		{"accept after a clock step", now.Add(-2 * time.Minute), at(time.Hour), true, true, true},
		{"valid", now.Add(time.Hour), at(-time.Minute), false, false, true},
		{"no accept time, expired", now.Add(-time.Minute), nil, true, false, false},
		{"no accept time, valid", now.Add(time.Hour), nil, false, false, false},
	}
	for _, tc := range cases {
		_, refuse, report, known := lapsedLeaf(req(tc.notAfter, tc.accepted))
		if refuse != tc.refuse || report != tc.report || known != tc.known {
			t.Errorf("%s: got (refuse %v, report %v, known %v), want (%v, %v, %v)",
				tc.name, refuse, report, known, tc.refuse, tc.report, tc.known)
		}
		_, err := RequireAgent(req(tc.notAfter, tc.accepted))
		if (err != nil) != tc.refuse {
			t.Errorf("%s: RequireAgent err = %v, want refused %v", tc.name, err, tc.refuse)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{}
	if _, refuse, _, _ := lapsedLeaf(r); refuse {
		t.Error("a certless request is not a lapse")
	}
}

// A refused request that is not a lapse (no accept time here) is not
// reported: it may be a valid connection whose leaf expired while open.
func TestLapseGateReportsOnlyALapse(t *testing.T) {
	fired := 0
	gate := LapseGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a lapsed request reached the handler")
	}), func(string) { fired++ }, nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
		Subject: pkix.Name{CommonName: "scratch"}, NotAfter: time.Now().Add(-time.Hour),
	}}}
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || fired != 0 {
		t.Fatalf("got %d (Retry-After %q) with %d reports, want 503 with Retry-After and none",
			w.Code, w.Header().Get("Retry-After"), fired)
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
