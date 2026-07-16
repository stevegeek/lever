package agent

import (
	"crypto/tls"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// writeLeaf mints a client leaf (CN=worker) signed by caInst and writes the
// agent.crt/agent.key/ca.crt trio into dir, returning the leaf serial. mtimeAt,
// when non-zero, is stamped on agent.crt so mtime-based reload is deterministic.
func writeLeaf(t *testing.T, dir string, caInst *ca.CA, mtimeAt time.Time) *big.Int {
	t.Helper()
	csrPEM, keyPEM := csrWithKey(t, "worker")
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatalf("writeLeaf: sign CSR: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caInst.CertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtimeAt.IsZero() {
		if err := os.Chtimes(filepath.Join(dir, "agent.crt"), mtimeAt, mtimeAt); err != nil {
			t.Fatal(err)
		}
	}
	return parseLeaf(t, certPEM).SerialNumber
}

func TestClientCertSourceReloadsOnMtimeChange(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	serial1 := writeLeaf(t, dir, caInst, t0)

	src, err := newClientCertSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Leaf.SerialNumber.Cmp(serial1) != 0 {
		t.Fatalf("first serial = %s, want %s", got.Leaf.SerialNumber, serial1)
	}

	// Rewrite the leaf with a newer mtime → next call must return the NEW serial.
	serial2 := writeLeaf(t, dir, caInst, t0.Add(time.Minute))
	if serial1.Cmp(serial2) == 0 {
		t.Fatal("rewrite produced the same serial; test cannot distinguish rotation")
	}
	got, err = src.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Leaf.SerialNumber.Cmp(serial2) != 0 {
		t.Fatalf("after rotation serial = %s, want %s", got.Leaf.SerialNumber, serial2)
	}
}

func TestClientCertSourceFailSoft(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	serial1 := writeLeaf(t, dir, caInst, t0)

	src, err := newClientCertSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	src.now = func() time.Time { return time.Now() } // cached leaf is still valid

	// Corrupt agent.key with a NEWER mtime on agent.crt so reload triggers and fails.
	if err := os.WriteFile(filepath.Join(dir, "agent.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "agent.crt"), t0.Add(time.Minute), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := src.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("fail-soft: expected cached cert, got error: %v", err)
	}
	if got.Leaf.SerialNumber.Cmp(serial1) != 0 {
		t.Fatalf("fail-soft served serial %s, want cached %s", got.Leaf.SerialNumber, serial1)
	}

	// Once the cached leaf is treated as expired, the failed re-read must surface.
	src.now = func() time.Time { return got.Leaf.NotAfter.Add(time.Minute) }
	if _, err := src.GetClientCertificate(nil); err == nil {
		t.Fatal("fail-soft must error once the cached cert is expired and re-read fails")
	}
}

func TestNewClientCertSourceFailsOnMissingIDDir(t *testing.T) {
	if _, err := newClientCertSource(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("newClientCertSource must fail eagerly on a missing id-dir")
	}
}

// recordingBackend is a fake mTLS broker: it records each presented client-cert
// serial and echoes the request path + body so the proxy round-trip is verifiable.
type recordingBackend struct {
	mu      sync.Mutex
	serials []*big.Int
}

func (b *recordingBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	if len(r.TLS.PeerCertificates) > 0 {
		b.serials = append(b.serials, r.TLS.PeerCertificates[0].SerialNumber)
	}
	b.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("X-Echo-Path", r.URL.Path)
	_, _ = w.Write(append([]byte("echo:"), body...))
}

func (b *recordingBackend) lastSerial() *big.Int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.serials) == 0 {
		return nil
	}
	return b.serials[len(b.serials)-1]
}

// TestNewReloadingClientPresentsRotatingCert proves the serve-capability fix: a
// client from NewReloadingClient presents the ROTATED leaf on a fresh handshake,
// where a static Identity.Client() would keep presenting the boot leaf forever
// (the recurring cert-expiry outage).
func TestNewReloadingClientPresentsRotatingCert(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{}
	srvCertPEM, srvKeyPEM, err := caInst.IssueServerCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewUnstartedServer(backend)
	fake.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientAuth: tls.RequireAnyClientCert}
	fake.StartTLS()
	defer fake.Close()

	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	serial1 := writeLeaf(t, dir, caInst, t0)

	client, err := NewReloadingClient(dir, caInst.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	post := func() {
		resp, perr := client.Post(fake.URL+"/request", "application/json", strings.NewReader("{}"))
		if perr != nil {
			t.Fatalf("post: %v", perr)
		}
		resp.Body.Close()
	}

	post()
	if s := backend.lastSerial(); s == nil || s.Cmp(serial1) != 0 {
		t.Fatalf("first handshake serial = %v, want %s", s, serial1)
	}

	// Rotate the leaf (newer mtime) and force a fresh handshake — exactly what
	// IdleConnTimeout does in production. The reloading client must present the NEW
	// leaf; a static client would still send serial1.
	serial2 := writeLeaf(t, dir, caInst, t0.Add(time.Minute))
	if serial1.Cmp(serial2) == 0 {
		t.Fatal("rotation produced the same serial; test cannot distinguish")
	}
	client.CloseIdleConnections()
	post()
	if s := backend.lastSerial(); s == nil || s.Cmp(serial2) != 0 {
		t.Fatalf("after rotation serial = %v, want %s (reloading client must present the rotated leaf)", s, serial2)
	}
}

func TestGatewayProxyPresentsRotatingCert(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// Fake mTLS broker: RequireAnyClientCert records the presented leaf without
	// needing it to chain (we assert on the serial, not on verification).
	backend := &recordingBackend{}
	srvCertPEM, srvKeyPEM, err := caInst.IssueServerCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewUnstartedServer(backend)
	fake.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientAuth: tls.RequireAnyClientCert}
	fake.StartTLS()
	defer fake.Close()

	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	serial1 := writeLeaf(t, dir, caInst, t0)

	proxy, err := newGatewayProxy(fake.URL, caInst.CertPEM(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Plaintext loopback front — this is what Claude talks to.
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Post(front.URL+"/mcp/db/", "application/json", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "echo:hello" {
		t.Errorf("body round-trip = %q, want %q", got, "echo:hello")
	}
	if resp.Header.Get("X-Echo-Path") != "/mcp/db/" {
		t.Errorf("path round-trip = %q, want /mcp/db/", resp.Header.Get("X-Echo-Path"))
	}
	if s := backend.lastSerial(); s == nil || s.Cmp(serial1) != 0 {
		t.Fatalf("backend saw serial %v, want %s", s, serial1)
	}

	// Rotate the leaf, then force a fresh handshake (IdleConnTimeout is 5m — too
	// long for a test, so close idle conns directly; this is exactly the reuse
	// hazard IdleConnTimeout guards against in production).
	serial2 := writeLeaf(t, dir, caInst, t0.Add(time.Minute))
	proxy.Transport.(*http.Transport).CloseIdleConnections()

	resp, err = http.Post(front.URL+"/mcp/db/", "application/json", strings.NewReader("again"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if s := backend.lastSerial(); s == nil || s.Cmp(serial2) != 0 {
		t.Fatalf("after rotation backend saw serial %v, want new serial %s", s, serial2)
	}
}

func TestGatewayProxyIdleConnTimeout(t *testing.T) {
	// Pin the anti-staleness guard: per-handshake cert reads only help if pooled
	// connections re-handshake regularly, so IdleConnTimeout must stay well under
	// the 12h renew / 24h expiry.
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLeaf(t, dir, caInst, time.Now())
	proxy, err := newGatewayProxy("https://broker.example", caInst.CertPEM(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := proxy.Transport.(*http.Transport).IdleConnTimeout; got != 5*time.Minute {
		t.Errorf("IdleConnTimeout = %s, want 5m", got)
	}
}

func TestGatewayRejectsNonLoopbackListen(t *testing.T) {
	err := Gateway("0.0.0.0:8462", "https://broker.example", []byte("ca"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Gateway must reject a non-loopback listen addr, got err=%v", err)
	}
}
