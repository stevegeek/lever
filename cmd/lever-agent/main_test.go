package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"gopkg.in/yaml.v3"
)

// svcSpec mirrors the subset of scion's api.ServiceSpec that the renew sidecar
// uses, for parsing the emitted scion-services.yaml back in tests.
type svcSpec struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Restart string   `yaml:"restart"`
}

func TestWriteRenewServicesAPIKey(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	const brokerURL = "https://host.orb.internal:8443"
	if err := os.WriteFile(bootstrap, []byte(`{"broker_url":"`+brokerURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	idDir := filepath.Join(home, ".lever-id")
	settings := filepath.Join(home, ".claude", "settings.json")

	if err := writeRenewServices(home, idDir, bootstrap, settings, "api-key"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}

	out := filepath.Join(home, ".scion", "scion-services.yaml")
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read services file: %v", err)
	}
	var specs []svcSpec
	if err := yaml.Unmarshal(b, &specs); err != nil {
		t.Fatalf("parse services yaml: %v", err)
	}
	// Two sidecars: lever-gateway (the loopback mTLS proxy) then lever-renew.
	if len(specs) != 2 {
		t.Fatalf("want 2 services, got %d: %s", len(specs), b)
	}

	// Gateway MUST be emitted first (up before renew) and carry baked absolute
	// flags — sidecars get no CWD, so it must never fall back to a bootstrap path.
	gw := specs[0]
	if gw.Name != "lever-gateway" {
		t.Errorf("specs[0].name = %q, want lever-gateway (emitted first)", gw.Name)
	}
	if gw.Restart != "on-failure" {
		t.Errorf("gateway restart = %q, want on-failure", gw.Restart)
	}
	gwCmd := strings.Join(gw.Command, " ")
	for _, want := range []string{
		"lever-agent gateway",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // baked; no sidecar bootstrap file-read
		"--listen 127.0.0.1:8462",
	} {
		if !strings.Contains(gwCmd, want) {
			t.Errorf("gateway command %q missing %q", gwCmd, want)
		}
	}

	s := specs[1]
	if s.Name != "lever-renew" {
		t.Errorf("specs[1].name = %q, want lever-renew", s.Name)
	}
	if s.Restart != "on-failure" {
		t.Errorf("restart = %q, want on-failure", s.Restart)
	}
	cmd := strings.Join(s.Command, " ")
	for _, want := range []string{
		"lever-agent renew --loop",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // resolved at boot; no sidecar file-read
		"--llm-auth api-key",
		"--settings " + settings,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
}

// TestWriteRenewServicesNoBootstrapIsNoop: a non-brokered agent (no bootstrap
// file) gets no sidecar — there is nothing to renew against.
func TestWriteRenewServicesNoBootstrapIsNoop(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "ws", ".lever", "bootstrap.json")
	if err := writeRenewServices(home, filepath.Join(home, ".lever-id"), missing, "", "subscription"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("services file should not exist for a non-brokered agent; stat err = %v", err)
	}
}

// TestWriteRenewServicesEmptyBrokerURLIsNoop: a bootstrap that exists but carries
// no broker URL (brokerless) is a distinct path from a missing bootstrap — it too
// gets no sidecar, since there is nothing to renew against.
func TestWriteRenewServicesEmptyBrokerURLIsNoop(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"ticket":"tk","broker_url":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRenewServices(home, filepath.Join(home, ".lever-id"), bootstrap, "", "api-key"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("services file should not exist for a brokerless bootstrap; stat err = %v", err)
	}
}

func TestWriteClaudeSettingsEnvMergesNotClobbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing settings: an unrelated top-level key + an existing env var.
	if err := os.WriteFile(path, []byte(`{"model":"sonnet","env":{"EXISTING":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := writeClaudeSettingsEnv(path)
	if err := w(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_BASE_URL": "http://x/llm"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["model"] != "sonnet" {
		t.Errorf("clobbered unrelated top-level key: model=%v", got["model"])
	}
	env, ok := got["env"].(map[string]any)
	if !ok {
		t.Fatalf("env is not an object: %v", got["env"])
	}
	if env["EXISTING"] != "keep" {
		t.Errorf("clobbered pre-existing env var: EXISTING=%v", env["EXISTING"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" || env["ANTHROPIC_BASE_URL"] != "http://x/llm" {
		t.Errorf("dynamic vars not merged into env: %v", env)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestWriteClaudeSettingsEnvCreatesWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeClaudeSettingsEnv(path)(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	env, _ := got["env"].(map[string]any)
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("absent-file case did not create env block: %v", got)
	}
}

func TestWriteClaudeSettingsEnvEmptyPathNoop(t *testing.T) {
	if err := writeClaudeSettingsEnv("")(map[string]string{"X": "y"}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestCLICapabilityVerbsValidateArgsBeforeAnythingElse(t *testing.T) {
	// lever-agent sits on $PATH inside every agent jail, so `delegate` here is
	// the same mint path the capability MCP tool exposes — and it had the same
	// defect: an absent -to sent an empty bind target, which the broker defaults
	// to the caller, printing a SELF-bound token as a success. The checks must
	// also run BEFORE the identity load, so a bad invocation reports the bad
	// argument rather than whatever unrelated thing fails first.
	missingID := filepath.Join(t.TempDir(), "no-such-id-dir")
	for _, tc := range []struct {
		name, verb, want string
		args             []string
	}{
		{"delegate without -to", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read"}},
		{"delegate blank -to", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read", "-to", "  "}},
		// A positional `to=worker` is swallowed as a CONSTRAINT while -to stays
		// empty — the CLI shape of the misspelt-argument bug.
		{"delegate with positional to=", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read", "to=worker"}},
		{"delegate without -tool", "delegate", `"-tool"`, []string{"-op", "read", "-to", "worker"}},
		{"request without -tool", "request", `"-tool"`, []string{"-op", "read"}},
		{"request without -op", "request", `"-op"`, []string{"-tool", "db"}},
	} {
		err := cmdCLI(tc.verb, append([]string{"-id-dir", missingID}, tc.args...))
		if err == nil {
			t.Fatalf("%s: must error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q, want it to name %s", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "no identity") {
			t.Fatalf("%s: argument checks must run before the identity load, got %q", tc.name, err)
		}
	}
}

func TestUnknownSubcommandErrors(t *testing.T) {
	if err := run([]string{"lever-agent", "bogus"}); err == nil {
		t.Fatal("unknown subcommand must error")
	}
}

func TestRunRequiresSubcommand(t *testing.T) {
	if err := run([]string{"lever-agent"}); err == nil {
		t.Fatal("missing subcommand must error")
	}
}

// TestRenewFlagAcceptance verifies that the renew flagset accepts --loop and
// --interval without a parse error (reconciles manifest.json sidecar declaration).
func TestRenewFlagAcceptance(t *testing.T) {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	fs.String("id-dir", defaultIDDir, "")
	fs.String("broker-url", "", "")
	fs.String("bootstrap", "", "")
	loop := fs.Bool("loop", false, "")
	interval := fs.Duration("interval", 12*time.Hour, "")

	if err := fs.Parse([]string{"--loop", "--interval", "6h"}); err != nil {
		t.Fatalf("flag parse error (manifest sidecar would crash): %v", err)
	}
	if !*loop {
		t.Error("--loop should be true after parse")
	}
	if *interval != 6*time.Hour {
		t.Errorf("--interval: got %v, want 6h", *interval)
	}
}

// TestRenewOnceNoIdentityErrors verifies that renewOnce returns an error (not a
// panic or hang) when no identity exists in the directory.
func TestRenewOnceNoIdentityErrors(t *testing.T) {
	tmp := t.TempDir()
	err := renewOnce(renewOpts{idDir: tmp})
	if err == nil {
		t.Fatal("renewOnce with empty dir must return an error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRenewOnceAPIKeyRefreshesOverlayAndSettings exercises the api-key branch of
// renewOnce (the overlay build + llm-token refresh + settings rewrite) against a
// real mTLS broker. It pins the five env keys the rewritten settings.json must
// carry: the three identity paths, a fresh ANTHROPIC_AUTH_TOKEN, and a
// gateway-hosted ANTHROPIC_BASE_URL — NOT the broker (the aa63f9f contract). The
// branch had no coverage before (only the no-identity error path was tested).
func TestRenewOnceAPIKeyRefreshesOverlayAndSettings(t *testing.T) {
	srv, caInst := newRenewTestBroker(t)
	ticket := provisionWorkerTicket(t, caInst, srv.URL, "worker")

	idDir := t.TempDir()
	id, err := agent.Enrol(context.Background(), srv.URL, caInst.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatalf("enrol worker: %v", err)
	}
	if err := id.Write(idDir); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := renewOnce(renewOpts{
		idDir:        idDir,
		brokerURL:    srv.URL,
		llmAuth:      "api-key",
		settingsPath: settingsPath,
	}); err != nil {
		t.Fatalf("renewOnce: %v", err)
	}

	env := readSettingsEnv(t, settingsPath)
	for _, tc := range []struct{ key, want string }{
		{"CLAUDE_CODE_CLIENT_CERT", filepath.Join(idDir, "agent.crt")},
		{"CLAUDE_CODE_CLIENT_KEY", filepath.Join(idDir, "agent.key")},
		{"NODE_EXTRA_CA_CERTS", filepath.Join(idDir, "ca.crt")},
	} {
		if env[tc.key] != tc.want {
			t.Errorf("env[%s] = %q, want %q", tc.key, env[tc.key], tc.want)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Error("api-key renew must write a fresh ANTHROPIC_AUTH_TOKEN")
	}
	if want := "http://127.0.0.1:8462/llm"; env["ANTHROPIC_BASE_URL"] != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q (loopback gateway, not the broker)", env["ANTHROPIC_BASE_URL"], want)
	}
	if strings.HasPrefix(env["ANTHROPIC_BASE_URL"], srv.URL) {
		t.Errorf("ANTHROPIC_BASE_URL = %q points at the broker (%s) — renew must write the gateway URL", env["ANTHROPIC_BASE_URL"], srv.URL)
	}
}

// newRenewTestBroker starts a real mTLS broker permitting the worker to
// self-mint an llm capability token, returning the TLS server and its CA.
func newRenewTestBroker(t *testing.T) (*httptest.Server, *ca.CA) {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	rl.AllowObtain("worker", broker.ReservedLLMTool, broker.ReservedLLMOp)
	reg := registry.New()
	if err := reg.Register(registry.Tool{
		Name:       broker.ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{broker.ReservedLLMOp: {Name: broker.ReservedLLMOp}},
		FirstParty: true,
	}); err != nil {
		t.Fatal(err)
	}
	b := broker.New(broker.Config{
		Keys:            kp,
		CA:              caInst,
		Tickets:         ca.NewTicketStore(),
		Rules:           rl,
		Registry:        reg,
		ManagerIdentity: "manager",
		Agents:          []string{"manager", "worker"},
		GrantTTL:        time.Hour,
		ServerName:      "127.0.0.1",
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
	return srv, caInst
}

// provisionWorkerTicket mints a one-use enrol ticket for worker by POSTing
// /provision as a manager cert signed by the broker CA.
func provisionWorkerTicket(t *testing.T, caInst *ca.CA, brokerURL, worker string) string {
	t.Helper()
	csrPEM, keyPEM, err := agent.GenerateCSR("manager")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatalf("sign manager CSR: %v", err)
	}
	managerCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caInst.Cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{managerCert},
	}}}
	body, _ := json.Marshal(map[string]string{"worker": worker})
	resp, err := client.Post(brokerURL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provision status %d", resp.StatusCode)
	}
	var pr struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil || pr.Ticket == "" {
		t.Fatalf("provision decode=%v ticket=%q", err, pr.Ticket)
	}
	return pr.Ticket
}

// readSettingsEnv reads the env block from a claude settings.json.
func readSettingsEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("settings.json not valid JSON: %v", err)
	}
	return s.Env
}

// TestRenewNonLoopReturnsErrorImmediately verifies that run with renew (no
// --loop) returns immediately with an error for an empty id-dir (no hang).
func TestRenewNonLoopReturnsErrorImmediately(t *testing.T) {
	tmp := t.TempDir()
	err := run([]string{"lever-agent", "renew", "--id-dir", tmp})
	if err == nil {
		t.Fatal("renew with no identity must error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRenewLoopFlagsAcceptedByRealCmd exercises the REAL dispatch path through
// run() to prove that cmdRenew accepts --loop and --interval without a
// "flag provided but not defined" parse error. Loop mode only exits on
// SIGINT/SIGTERM, so we send SIGINT to ourselves after a brief delay to unblock
// it. The test asserts that any returned error is NOT a flag-parse error (an
// "no identity" or nil return both indicate the flags were accepted).
func TestRenewLoopFlagsAcceptedByRealCmd(t *testing.T) {
	tmp := t.TempDir()

	// Send SIGINT to ourselves after 50ms to unblock the loop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	err := run([]string{"lever-agent", "renew", "--id-dir", tmp, "--loop", "--interval", "24h"})
	// Loop mode exits cleanly (nil) on signal. Either way, the error must NOT be
	// a flag-parse error — that would mean cmdRenew doesn't define --loop/--interval.
	if err != nil && (strings.Contains(err.Error(), "flag provided but not defined") ||
		strings.Contains(err.Error(), "flag: help requested")) {
		t.Fatalf("real cmdRenew rejected --loop/--interval (manifest sidecar would break): %v", err)
	}
}

// TestProvisionVerbAcceptedByRun verifies that run() dispatches "provision" and
// that the provision flags parse correctly. It uses a temp dir as -id-dir so there
// is no identity — cmdProvision errors with "no identity", which proves dispatch
// and flag parsing succeeded without a "flag provided but not defined" error.
func TestProvisionVerbAcceptedByRun(t *testing.T) {
	err := run([]string{"lever-agent", "provision", "-worker", "worker", "-out", t.TempDir() + "/w.json", "-id-dir", t.TempDir()})
	if err == nil {
		t.Fatal("expected an error (no identity), got nil")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("provision flags must parse: %v", err)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns whatever
// fn printed. Small outputs only (the pipe buffer is not drained concurrently).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// callVerbWorkerID enrols a worker identity against a real mTLS broker and writes
// it to a temp dir, returning the dir. The `call` verb needs a loadable identity
// (id.Client builds the mTLS client), but the broker it dials for the actual tool
// call is a separate stub, so any valid identity suffices.
func callVerbWorkerID(t *testing.T) string {
	t.Helper()
	srv, caInst := newRenewTestBroker(t)
	ticket := provisionWorkerTicket(t, caInst, srv.URL, "worker")
	id, err := agent.Enrol(context.Background(), srv.URL, caInst.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatalf("enrol worker: %v", err)
	}
	idDir := t.TempDir()
	if err := id.Write(idDir); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return idDir
}

// TestCallVerbSemantics pins the wire + I/O contract of the `call` verb, whose
// only prior coverage was the live acceptance harness (not run by `go test`):
// it POSTs a JSON-RPC tools/call to /mcp/<tool>/ with Content-Type
// application/json, the token in arguments._capability, prints the raw response
// body to stdout EVEN on a non-200, and maps a non-200 to `call: status %d`.
func TestCallVerbSemantics(t *testing.T) {
	idDir := callVerbWorkerID(t)

	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"success", http.StatusOK, `{"result":"ok"}`, ""},
		{"denied", http.StatusForbidden, `{"error":"denied"}`, "call: status 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotCT, gotBody string
			// Plain-HTTP stub: id.Client's mTLS transport dials http:// fine
			// (its TLS config is inert for a non-TLS scheme).
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotCT = r.Header.Get("Content-Type")
				var b bytes.Buffer
				b.ReadFrom(r.Body)
				gotBody = b.String()
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer stub.Close()

			var err error
			out := captureStdout(t, func() {
				err = cmdCLI("call", []string{
					"-id-dir", idDir,
					"-broker-url", stub.URL,
					"-tool", "db",
					"-op", "read",
					"-token", "tok_xyz",
					"table=users",
				})
			})

			if gotPath != "/mcp/db/" {
				t.Errorf("POST path = %q, want /mcp/db/", gotPath)
			}
			if gotCT != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotCT)
			}
			if !strings.Contains(gotBody, `"_capability":"tok_xyz"`) {
				t.Errorf("request body missing _capability token: %s", gotBody)
			}
			if !strings.Contains(gotBody, `"table":"users"`) {
				t.Errorf("request body missing constraint arg: %s", gotBody)
			}
			// Body is printed to stdout regardless of status.
			if !strings.Contains(out, tc.body) {
				t.Errorf("stdout = %q, want it to contain response body %q", out, tc.body)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil || err.Error() != tc.wantErr {
					t.Errorf("error = %v, want %q", err, tc.wantErr)
				}
			}
		})
	}
}

// TestMCPAddArgsUsesUserScope pins the fix for the worker/manager MCP-wiring gap:
// the pre-start hook runs `claude mcp add` from the agent home, but the claude
// session runs in /workspace. `claude mcp add`'s default (local) scope is
// keyed by CWD, so servers registered from the home are invisible to the
// session. --scope user makes them global (CWD-independent) — required for both
// the http broker tools and the stdio capability server.
func TestMCPAddArgsUsesUserScope(t *testing.T) {
	got := mcpAddArgs("db", []string{"--transport", "http", "https://broker/mcp/db/"})
	want := []string{"mcp", "add", "--scope", "user", "db", "--transport", "http", "https://broker/mcp/db/"}
	if !slices.Equal(got, want) {
		t.Fatalf("http tool args = %v, want %v", got, want)
	}
	// The stdio capability server must also be user-scoped.
	cap := mcpAddArgs("lever-capability", []string{"lever-agent", "serve-capability"})
	if strings.Join(cap[:4], " ") != "mcp add --scope user" {
		t.Fatalf("capability server args must lead with `mcp add --scope user`, got %v", cap)
	}
	if cap[4] != "lever-capability" {
		t.Fatalf("scope must precede the server name, got %v", cap)
	}
}

// TestMCPRemoveArgsUserScope pins the remove targets the same user scope as add.
func TestMCPRemoveArgsUserScope(t *testing.T) {
	got := mcpRemoveArgs("db")
	want := []string{"mcp", "remove", "--scope", "user", "db"}
	if !slices.Equal(got, want) {
		t.Fatalf("mcpRemoveArgs = %v, want %v", got, want)
	}
}

// TestClaudeMCPAddIsIdempotent verifies claudeMCPAdd removes before adding and
// ignores a failing remove (absent server), so a re-boot (scion resume) can't
// fail the pre-start hook on "already exists".
func TestClaudeMCPAddIsIdempotent(t *testing.T) {
	var calls [][]string
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 1 && args[1] == "remove" {
			return []byte("No MCP server named \"db\""), errors.New("exit status 1") // absent → non-zero
		}
		return nil, nil
	}
	if err := claudeMCPAdd("db", "--transport", "http", "https://broker/mcp/db/"); err != nil {
		t.Fatalf("a failing remove must be ignored; claudeMCPAdd returned %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want remove then add (2 calls), got %d: %v", len(calls), calls)
	}
	if calls[0][2] != "remove" || calls[1][2] != "add" {
		t.Fatalf("must remove before add; got %v then %v", calls[0], calls[1])
	}
	// remove: [claude mcp remove --scope user db]; add: [claude mcp add --scope user db --transport …]
	if calls[0][5] != "db" || calls[1][5] != "db" {
		t.Fatalf("both must target the same server name; got %v / %v", calls[0], calls[1])
	}
}

// TestClaudeMCPAddSurfacesAddError: a failing ADD (not remove) must surface.
func TestClaudeMCPAddSurfacesAddError(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "add" {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if err := claudeMCPAdd("db", "--transport", "http", "u"); err == nil {
		t.Fatal("a failing add must return an error")
	}
}

// TestCommonFlagsDefinesTrio pins the id-dir/broker-url/bootstrap trio that
// commonFlags registers for the five lazy-resolving subcommands: flag names, the
// per-command-configurable help strings, and the defaults (id-dir →
// $HOME/.lever-id, the other two empty). D3 collapses five hand-rolled copies
// onto this one helper, so this is the guard for that single source of truth.
func TestCommonFlagsDefinesTrio(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	idDir, brokerURL, bootstrap := commonFlags(fs, "id help", "bootstrap help")

	wantIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	if *idDir != wantIDDir {
		t.Errorf("id-dir default: got %q, want %q", *idDir, wantIDDir)
	}
	if *brokerURL != "" {
		t.Errorf("broker-url default: got %q, want empty", *brokerURL)
	}
	if *bootstrap != "" {
		t.Errorf("bootstrap default: got %q, want empty", *bootstrap)
	}

	for _, tc := range []struct {
		name, usage string
	}{
		{"id-dir", "id help"},
		{"broker-url", "broker URL (overrides bootstrap)"},
		{"bootstrap", "bootstrap help"},
	} {
		f := fs.Lookup(tc.name)
		if f == nil {
			t.Errorf("flag %q not defined", tc.name)
			continue
		}
		if f.Usage != tc.usage {
			t.Errorf("flag %q usage: got %q, want %q", tc.name, f.Usage, tc.usage)
		}
	}
}

// TestResolveBrokerURLFallbackChain pins the three-step resolution order of
// resolveBrokerURL: an explicit --broker-url wins outright without touching any
// file; otherwise the broker URL is read from a bootstrap file located by
// --bootstrap, then $LEVER_BOOTSTRAP, then ./.lever/bootstrap.json. D3 folds the
// path-resolution half into bootstrapPathOrDefault, so this guards the merge —
// neither resolveBrokerURL nor bootstrapPathOrDefault had a test before.
func TestResolveBrokerURLFallbackChain(t *testing.T) {
	writeBootstrap := func(t *testing.T, dir, url string) string {
		t.Helper()
		p := filepath.Join(dir, "bootstrap.json")
		data, err := json.Marshal(agent.Bootstrap{BrokerURL: url})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("explicit broker-url wins without reading any file", func(t *testing.T) {
		// $LEVER_BOOTSTRAP points at a nonexistent file; if it were consulted the
		// call would error. It must not be — the explicit flag short-circuits.
		t.Setenv("LEVER_BOOTSTRAP", filepath.Join(t.TempDir(), "does-not-exist.json"))
		got, err := resolveBrokerURL("https://explicit.example", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://explicit.example" {
			t.Errorf("got %q, want the explicit flag value", got)
		}
	})

	t.Run("bootstrap flag path is read when broker-url is empty", func(t *testing.T) {
		// env points elsewhere; the explicit --bootstrap path must take precedence.
		t.Setenv("LEVER_BOOTSTRAP", filepath.Join(t.TempDir(), "wrong.json"))
		p := writeBootstrap(t, t.TempDir(), "https://from-flag.example")
		got, err := resolveBrokerURL("", p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-flag.example" {
			t.Errorf("got %q, want the URL from the --bootstrap file", got)
		}
	})

	t.Run("LEVER_BOOTSTRAP is used when both flags are empty", func(t *testing.T) {
		p := writeBootstrap(t, t.TempDir(), "https://from-env.example")
		t.Setenv("LEVER_BOOTSTRAP", p)
		got, err := resolveBrokerURL("", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-env.example" {
			t.Errorf("got %q, want the URL from $LEVER_BOOTSTRAP", got)
		}
	})

	t.Run("defaults to ./.lever/bootstrap.json when nothing is set", func(t *testing.T) {
		t.Setenv("LEVER_BOOTSTRAP", "")
		dir := t.TempDir()
		leverDir := filepath.Join(dir, ".lever")
		if err := os.MkdirAll(leverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(agent.Bootstrap{BrokerURL: "https://from-default.example"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leverDir, "bootstrap.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		got, err := resolveBrokerURL("", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-default.example" {
			t.Errorf("got %q, want the URL from ./.lever/bootstrap.json", got)
		}
	})
}
