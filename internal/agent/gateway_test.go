package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math/big"
	"net"
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

// writeLeaf returns the leaf serial. mtimeAt, when non-zero, is stamped on
// agent.crt so mtime-based reload is deterministic.
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

	// A newer mtime with a corrupt key makes the reload trigger and fail.
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

func TestCAPoolRejectsBadPEM(t *testing.T) {
	if _, err := caPool(nil); err == nil {
		t.Fatal("caPool must reject an empty CA PEM")
	}
	_, err := caPool([]byte("-----BEGIN CERTIFICATE-----\nnotpem\n-----END CERTIFICATE-----"))
	if !errors.Is(err, errBadCAPEM) {
		t.Fatalf("caPool must reject a malformed CA PEM, got %v", err)
	}
}

func TestReloadingTransportRejectsBadPEM(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLeaf(t, dir, caInst, time.Now())
	_, err = reloadingTransport(dir, []byte("garbage"))
	if !errors.Is(err, errBadCAPEM) {
		t.Fatalf("reloadingTransport must reject a malformed CA PEM, got %v", err)
	}
}

// recordingBackend is a fake mTLS broker that records each presented
// client-cert serial.
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

// TestNewReloadingClientPresentsRotatingCert: a reloading client presents the
// rotated leaf on a fresh handshake, where Identity.Client() would keep the
// boot leaf.
func TestNewReloadingClientPresentsRotatingCert(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{}
	srvCertPEM, srvKeyPEM, err := caInst.IssueServerCertSANs("127.0.0.1", nil, []string{"127.0.0.1"})
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

	// Closing idle connections stands in for IdleConnTimeout.
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
	// RequireAnyClientCert: the test asserts on the serial, not on chaining.
	backend := &recordingBackend{}
	srvCertPEM, srvKeyPEM, err := caInst.IssueServerCertSANs("127.0.0.1", nil, []string{"127.0.0.1"})
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

	// Closing idle connections stands in for IdleConnTimeout.
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
	// Per-handshake cert reads only help if pooled connections re-handshake
	// regularly, so IdleConnTimeout must stay well under the renew interval.
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
	err := Gateway(context.Background(), GatewayConfig{Listen: "0.0.0.0:8462", BrokerURL: "https://broker.example", CAPEM: []byte("ca"), IDDir: t.TempDir()})
	if !errors.Is(err, errNotLoopback) {
		t.Fatalf("Gateway must reject a non-loopback listen addr, got err=%v", err)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestGatewayStopsOnContextCancel: main's signal-bound ctx replaces the default
// terminate disposition, so Gateway must return nil promptly on cancel or the
// sidecar holding the agent leaf outlives a stop.
func TestGatewayStopsOnContextCancel(t *testing.T) {
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLeaf(t, dir, caInst, time.Now())
	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Gateway(ctx, GatewayConfig{Listen: addr, BrokerURL: "https://broker.invalid", CAPEM: caInst.CertPEM(), IDDir: dir})
	}()
	// The cancel must hit a serving gateway.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Gateway returned before the listener came up: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway listener never came up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Gateway after ctx cancel = %v, want nil (a signal is a clean stop)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway did not return within 2s of ctx cancel")
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatalf("listener on %s still accepting after Gateway returned", addr)
	}
}
