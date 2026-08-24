// Package brokertest holds the broker fixtures the agent, brokerctl and
// captool tests share: an in-process broker behind an mTLS httptest server,
// CA-issued client certs and the clients that present them, the
// manager-side provision call, and a fake admin endpoint for the captool SDK.
//
// It imports internal/broker, so broker's own tests cannot use it; they keep
// their in-package helpers.
package brokertest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// Config selects what the test broker permits. The zero value is a broker
// that dispatches to one worker, "worker", for manager "manager".
type Config struct {
	// Workers are the dispatchable worker names (default: "worker").
	Workers []string
	// ManagerIdentity is the CN the broker accepts /provision from
	// (default: "manager").
	ManagerIdentity string
}

// Env holds every test-side handle for a broker under test, including the
// policy and registry it was built from so a test can drive them directly.
type Env struct {
	Broker   *broker.Broker
	Server   *httptest.Server
	CA       *ca.CA
	Keys     token.KeyPair
	Rules    *rules.Policy
	Registry *registry.Registry
}

// NewTestBroker builds a broker from cfg and serves its jail handler over
// mTLS on 127.0.0.1, with a CA-issued server cert pinned on the listener.
func NewTestBroker(t *testing.T, cfg Config) *Env {
	t.Helper()
	if len(cfg.Workers) == 0 {
		cfg.Workers = []string{"worker"}
	}
	if cfg.ManagerIdentity == "" {
		cfg.ManagerIdentity = "manager"
	}
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	pol := rules.NewPolicy()
	specs := make([]broker.WorkerSpec, 0, len(cfg.Workers))
	for _, w := range cfg.Workers {
		specs = append(specs, broker.WorkerSpec{Name: w})
	}
	b := broker.New(broker.Config{
		Identity: broker.IdentityConfig{
			Keys:            kp,
			CA:              caInst,
			Tickets:         ca.NewTicketStore(),
			Rules:           pol,
			Registry:        reg,
			ManagerIdentity: cfg.ManagerIdentity,
		},
		Dispatch: broker.DispatchConfig{Workers: specs},
	})
	src, err := caInst.NewServerCertSource("127.0.0.1", nil, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := caInst.ServerTLSConfigSource(src, nil)
	// httptest.StartTLS injects its own self-signed cert when Certificates is
	// empty, and the TLS stack only consults GetCertificate for SNI-bearing
	// hellos — an IP-dialled client sends none. Pin the source's cert.
	srvCert, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg.Certificates = []tls.Certificate{*srvCert}
	srv := httptest.NewUnstartedServer(b.JailHandler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &Env{Broker: b, Server: srv, CA: caInst, Keys: kp, Rules: pol, Registry: reg}
}

// CSRWithKey returns a PEM CSR for cn plus the matching EC private key PEM.
func CSRWithKey(t *testing.T, cn string) (csrPEM, keyPEM []byte) {
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

// Cert issues a CA-signed client cert for cn — the simulated-agent technique:
// skip provision/enrol and mint the leaf directly from the CA.
func Cert(t *testing.T, caInst *ca.CA, cn string) tls.Certificate {
	t.Helper()
	csrPEM, keyPEM := CSRWithKey(t, cn)
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

// Client builds an mTLS client that pins caInst and presents cert. A
// non-empty serverName verifies the server cert against that name rather
// than the dialled address (the broker's real cert names the host alias, not
// 127.0.0.1).
func Client(caInst *ca.CA, cert tls.Certificate, serverName string) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(caInst.Cert)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		ServerName:   serverName,
		Certificates: []tls.Certificate{cert},
	}}}
}

// ClientFor issues a cert for cn from the env's CA and returns an mTLS client
// that presents it to env.Server.
func (e *Env) ClientFor(t *testing.T, cn string) *http.Client {
	t.Helper()
	return Client(e.CA, Cert(t, e.CA, cn), "")
}

// ProvisionWorker POSTs /provision {worker} as the manager and returns the
// ticket the broker minted.
func (e *Env) ProvisionWorker(t *testing.T, worker string) string {
	t.Helper()
	return ProvisionWorker(t, e.ClientFor(t, "manager"), e.Server.URL, worker)
}

// ProvisionWorker POSTs /provision {worker} to brokerURL with client (which
// must present a manager cert) and returns the ticket.
func ProvisionWorker(t *testing.T, client *http.Client, brokerURL, worker string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"worker": worker})
	resp, err := client.Post(brokerURL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("provision: POST /provision: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provision: status %d", resp.StatusCode)
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("provision: decode: %v", err)
	}
	if result.Ticket == "" {
		t.Fatal("provision: empty ticket")
	}
	return result.Ticket
}

// FakeAdmin is a stand-in for the broker's admin listener as the captool SDK
// sees it: /register answers with a public key and the current epoch, /epoch
// with the epoch alone. Tests move Epoch to simulate a broker bump.
type FakeAdmin struct {
	Server *httptest.Server
	Keys   token.KeyPair
	Epoch  atomic.Int64

	mu         sync.Mutex
	registered []map[string]any
}

// FakeAdminServer starts a FakeAdmin at epoch.
func FakeAdminServer(t *testing.T, epoch int64) *FakeAdmin {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeAdmin{Keys: kp}
	f.Epoch.Store(epoch)
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.registered = append(f.registered, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"public_key": token.EncodePublicKey(kp.Public),
			"epoch":      int(f.Epoch.Load()),
		})
	})
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"epoch": int(f.Epoch.Load())})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// Registered returns the decoded bodies of every /register call so far.
func (f *FakeAdmin) Registered() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.registered))
	copy(out, f.registered)
	return out
}
