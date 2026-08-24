package broker

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/scion"
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

// ---- Config builders ----
//
// testConfig is the ONE broker Config builder; every fixture below is a thin
// composition of it with option funcs. The base is a fully keyed broker
// (keys, CA, ticket store), the default policy + registry (analyst obtains
// db.read; manager delegates db.read to worker; tool "db"), manager identity
// "manager" (slug = CN), one declared worker "worker", and a
// discarded log.

type configOpt func(*Config)

func testConfig(t *testing.T, opts ...configOpt) Config {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	rl.AllowObtain("analyst", "db", "read")
	rl.AllowDelegate("manager", "db", "read", "worker")
	reg := registry.New()
	_ = reg.Register(regTool("db", "http://127.0.0.1:3201", "read"))
	cfg := Config{
		Identity: IdentityConfig{
			Keys: kp, CA: c, Tickets: ca.NewTicketStore(), Rules: rl, Registry: reg,
			ManagerIdentity: "manager", GrantTTL: time.Hour, TicketTTL: 10 * time.Minute,
		},
		Dispatch: DispatchConfig{Workers: []WorkerSpec{{Name: "worker"}}},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// withAudit captures the broker.decision audit log in buf so tests can pin
// exact lines.
func withAudit(buf *bytes.Buffer) configOpt {
	return func(c *Config) { c.Log = slog.New(slog.NewTextHandler(buf, nil)) }
}

// withPolicy replaces the default policy with one built by build (nil ⇒ an
// empty default-deny policy).
func withPolicy(build func(*rules.Policy)) configOpt {
	return func(c *Config) {
		pol := rules.NewPolicy()
		if build != nil {
			build(pol)
		}
		c.Identity.Rules = pol
	}
}

// withTools replaces the default registry with one holding exactly tools.
func withTools(t *testing.T, tools ...registry.Tool) configOpt {
	t.Helper()
	return func(c *Config) {
		reg := registry.New()
		for _, tool := range tools {
			if err := reg.Register(tool); err != nil {
				t.Fatal(err)
			}
		}
		c.Identity.Registry = reg
	}
}

// withManager sets the manager cert CN and its scion slug.
func withManager(cn, slug string) configOpt {
	return func(c *Config) { c.Identity.ManagerIdentity, c.Identity.ManagerSlug = cn, slug }
}

// withRuntime wires the worker dispatch side: the runtime, the declared
// workers, the bootstrap CA/URL staged for them and the instance project.
func withRuntime(rt WorkerRuntime, specs ...WorkerSpec) configOpt {
	return func(c *Config) {
		c.Dispatch.Runtime, c.Dispatch.Workers = rt, specs
		c.Dispatch.BrokerCAPEM, c.Dispatch.BrokerURL = "CA-PEM", "https://10.0.0.2:8080"
		c.Dispatch.InstanceProject = testInstanceProject
	}
}

// withLLM wires the /llm proxy: the reserved llm pseudo-tool registered, a
// policy letting "worker" self-obtain llm.generate, the key and a fake
// upstream.
func withLLM(t *testing.T, apiKey []byte, upstreamURL string) configOpt {
	t.Helper()
	tools := withTools(t, registry.Tool{
		Name:       ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{ReservedLLMOp: {Name: ReservedLLMOp}},
		FirstParty: true,
	})
	policy := withPolicy(func(p *rules.Policy) { p.AllowObtain("worker", ReservedLLMTool, ReservedLLMOp) })
	return func(c *Config) {
		tools(c)
		policy(c)
		c.LLM = LLMConfig{APIKey: apiKey, Upstream: upstreamURL}
	}
}

// auditConfig returns testConfig with the audit log captured in a buffer.
func auditConfig(t *testing.T) (Config, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return testConfig(t, withAudit(&buf)), &buf
}

// restrictedConfig builds a broker whose "db" tool restricts the "table"
// constraint to {A,B} via AllowedValues, and lets analyst self-obtain db.read.
// Used to exercise the broker's mint-time constraint validation + baking.
func restrictedConfig(t *testing.T) Config {
	t.Helper()
	return testConfig(t,
		withPolicy(func(p *rules.Policy) { p.AllowObtain("analyst", "db", "read") }),
		withTools(t, registry.Tool{
			Name: "db", Backend: "http://127.0.0.1:3201",
			Operations:    map[string]registry.Operation{"read": {Name: "read"}},
			AllowedValues: map[string][]string{"table": {"A", "B"}},
		}))
}

// coarseConfig builds a broker with a single coarse, externally-fronted tool
// "utilities" (registry has ONLY the synthetic WildcardOp, mirroring a real
// gate:coarse tool). grantWildcard controls whether "analyst" holds the
// {utilities, "*"} obtain grant. The returned buffer captures audit log
// output so tests can assert on the op-coercion detail.
func coarseConfig(t *testing.T, grantWildcard bool) (Config, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return testConfig(t,
		withAudit(&buf),
		withPolicy(func(p *rules.Policy) {
			if grantWildcard {
				p.AllowObtain("analyst", "utilities", registry.WildcardOp)
			}
		}),
		withTools(t, registry.Tool{
			Name: "utilities", Backend: "127.0.0.1:3103", External: true, Coarse: true,
			Operations: map[string]registry.Operation{registry.WildcardOp: {Name: registry.WildcardOp}},
		})), &buf
}

// llmBrokerConfig builds a broker wired for the /llm proxy (see withLLM).
func llmBrokerConfig(t *testing.T, apiKey, upstreamURL string) Config {
	t.Helper()
	return testConfig(t, withLLM(t, []byte(apiKey), upstreamURL))
}

// newTestBrokerForLLM builds a broker wired for the /llm proxy and returns
// it with the caller identity ("worker").
func newTestBrokerForLLM(t *testing.T, apiKey []byte, upstreamURL string) (*Broker, string) {
	t.Helper()
	return New(testConfig(t, withLLM(t, apiKey, upstreamURL))), "worker"
}

// toolsBroker builds a broker with an EMPTY registry so a test can register
// exactly the tools it needs.
func toolsBroker(t *testing.T) *Broker {
	t.Helper()
	return New(testConfig(t, withTools(t)))
}

// newTestBroker builds a Broker with a fake runtime and a single declared
// worker under manager CN "test-manager".
func newTestBroker(t *testing.T, rt WorkerRuntime, spec WorkerSpec) *Broker {
	t.Helper()
	return New(testConfig(t, withManager("test-manager", ""), withRuntime(rt, spec)))
}

// reenrolBroker is newTestBroker plus the auto-re-enrol healer wiring: mode,
// manager slug "appname" and a temp manager bootstrap dir (returned).
func reenrolBroker(t *testing.T, rt WorkerRuntime, mode string) (*Broker, WorkerSpec, string) {
	t.Helper()
	spec := WorkerSpec{Name: "scratch", WorkspaceSubdir: "workers/scratch",
		HostWorkspace: filepath.Join(t.TempDir(), "ws"),
		BootstrapDir:  filepath.Join(t.TempDir(), ".lever")}
	managerDir := filepath.Join(t.TempDir(), ".lever")
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, spec),
		func(c *Config) { c.Dispatch.AutoReenrol, c.Dispatch.ManagerBootstrapDir = mode, managerDir }))
	return b, spec, managerDir
}

// msgWorkers are the declared workers of the messaging fixtures.
var msgWorkers = []WorkerSpec{
	{Name: "scratch", WorkspaceSubdir: "workers/scratch"},
	{Name: "worker", WorkspaceSubdir: "workers/worker"},
}

// msgBroker's fixture deliberately makes the manager cert CN ("manager") and
// the manager scion agent SLUG ("assistant", the app name) DIFFER: a live bug
// (scion: `Agent "manager" not found in project`) hid behind an earlier
// fixture where CN == slug, so routing to agent:<CN> passed by coincidence.
// No runtime is wired: it serves the pure target-resolution tests.
func msgBroker(t *testing.T, g2g bool) *Broker {
	t.Helper()
	return New(testConfig(t, withManager("manager", "assistant"), withRuntime(nil, msgWorkers...),
		func(c *Config) { c.Dispatch.WorkerToWorker = g2g }))
}

// newMsgTestBroker is msgBroker with a fakeMsgRuntime wired and the audit
// output captured in the returned buffer.
func newMsgTestBroker(t *testing.T, g2g bool) (*Broker, *fakeMsgRuntime, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	rt := &fakeMsgRuntime{WorkerRuntime: &fakeRuntime{agents: map[string][]scion.Agent{}}}
	b := New(testConfig(t, withAudit(&buf), withManager("manager", "assistant"), withRuntime(rt, msgWorkers...),
		func(c *Config) { c.Dispatch.WorkerToWorker = g2g }))
	return b, rt, &buf
}

// directiveTestBroker builds a Broker wired for the directive admin channel:
// a real ssh-keygen operator key/allowed_signers pair, instance "testinst",
// a 24h expiry cap, a declared "worker" WorkerSpec, and a message-capturing
// fake runtime.
func directiveTestBroker(t *testing.T) (b *Broker, priv string, allowedSigners string, rt *fakeDirectiveRuntime) {
	t.Helper()
	priv, as := genOperatorKey(t)
	rt = &fakeDirectiveRuntime{fakeRuntime: fakeRuntime{agents: map[string][]scion.Agent{}}}
	cfg := testConfig(t, withRuntime(rt, WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker"}),
		func(c *Config) {
			c.Directives = DirectiveConfig{
				Verifier:   &opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"},
				InstanceID: "testinst",
				ExpiryMax:  24 * time.Hour,
			}
		})
	return New(cfg), priv, as, rt
}

// ---- Client-cert fakes, one per fidelity level ----

// fakeTLSWithCN returns a synthetic TLS connection state whose verified client
// cert CN is cn — no CA involved. Enough for ca.RequireAgent in handler tests.
func fakeTLSWithCN(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

// signedCert builds a real tls.Certificate (leaf + key) for cn signed by the
// broker CA, for clients that present it over real mTLS.
func signedCert(t *testing.T, b *Broker, cn string) tls.Certificate {
	t.Helper()
	csrPEM, keyPEM := csrWithKey(t, cn)
	certPEM, err := b.ca.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// leafFor builds a verified-chain TLS state carrying a CA-signed leaf for cn
// (signedCert's leaf), for httptest requests against handlers directly.
func leafFor(t *testing.T, b *Broker, cn string) *tls.ConnectionState {
	t.Helper()
	return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{signedCert(t, b, cn).Leaf}}
}
