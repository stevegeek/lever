package brokerctl

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/daemon"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// wiringApp returns a two-worker orbstack app; directives on iff signers != "".
func wiringApp(tree, signers string) *config.App {
	return &config.App{
		Name: "demo", Backend: "orbstack", Tree: tree,
		Manager:  config.Manager{Image: "img"},
		Broker:   config.Broker{LLMAuth: config.LLMAuthSubscription},
		Operator: config.Operator{AllowedSigners: signers},
		Workers: []config.Worker{
			{Name: "a", Dir: "workers/a"},
			{Name: "b", Dir: "workers/b"},
		},
	}
}

// decorateForTest builds a cfg and decorates it against a real orbstack backend
// with a deterministic environment (no jail-runner, no host-alias override), so
// the assertions see only the config-derived wiring.
func decorateForTest(t *testing.T, app *config.App, version string) (broker.Config, state.State) {
	t.Helper()
	// Deterministic env: unset the jail-runner and host-alias hooks so
	// decorateConfig takes the no-Runtime path and BrokerURL falls back to the
	// backend's host alias.

	kp, err := token.Generate()
	if err != nil {
		t.Fatalf("token.Generate: %v", err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatalf("ca.Generate: %v", err)
	}
	be, err := registry.Select(app.Backend, proc.RealRunner{}, "lever-"+app.Name)
	if err != nil {
		t.Fatalf("registry.Select: %v", err)
	}
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := decorateConfig(&cfg, app, st, be, version, ServeEnv{}); err != nil {
		t.Fatalf("decorateConfig: %v", err)
	}
	return cfg, st
}

func TestDecorateConfigWiresConfigDerivedFields(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "") // directives OFF
	cfg, _ := decorateForTest(t, app, "v1.2.3")

	if cfg.Identity.ManagerSlug != "demo" {
		t.Errorf("ManagerSlug = %q, want app name %q", cfg.Identity.ManagerSlug, "demo")
	}
	if len(cfg.Dispatch.Workers) != 2 {
		t.Fatalf("Workers = %d, want 2 (WorkerSpecs of the two config workers)", len(cfg.Dispatch.Workers))
	}
	// InstanceProject + the mount are the SELECTED backend's dest, not hardwired.
	if cfg.Dispatch.InstanceProject != "/lever" {
		t.Errorf("InstanceProject = %q, want %q (backend MountDest)", cfg.Dispatch.InstanceProject, "/lever")
	}
	// host falls back to the orbstack HostToolAlias (LEVER_HOST_ALIAS_IP
	// unset); port is the default jail port.
	if want := "https://host.orb.internal:8443"; cfg.Dispatch.BrokerURL != want {
		t.Errorf("BrokerURL = %q, want %q", cfg.Dispatch.BrokerURL, want)
	}
	if cfg.Version != "v1.2.3" {
		t.Errorf("Version = %q, want the passed-in version", cfg.Version)
	}
	if cfg.ConfigHash != ConfigHash(app) {
		t.Errorf("ConfigHash = %q, want ConfigHash(app) %q", cfg.ConfigHash, ConfigHash(app))
	}
	if !cfg.Dispatch.WorkerToWorker {
		t.Error("WorkerToWorker = false, want true (default)")
	}
	if cfg.Dispatch.AutoReenrol != "all" {
		t.Errorf("AutoReenrol = %q, want %q (default)", cfg.Dispatch.AutoReenrol, "all")
	}
	if want := filepath.Join(tree, ".lever"); cfg.Dispatch.ManagerBootstrapDir != want {
		t.Errorf("ManagerBootstrapDir = %q, want %q", cfg.Dispatch.ManagerBootstrapDir, want)
	}
	// Persist closures are the state's writers — non-nil so the broker can
	// write revocation/directive state through on mutation.
	if cfg.Persistence.PersistRevocation == nil {
		t.Error("PersistRevocation is nil, want state.SaveRevocation")
	}
	if cfg.Persistence.PersistDirectives == nil {
		t.Error("PersistDirectives is nil, want state.SaveDirectives")
	}
	// No jail-runner env ⇒ no worker-dispatch runtime is wired.
	if cfg.Dispatch.Runtime != nil {
		t.Error("Runtime is non-nil, want nil without LEVER_JAIL_USER/UID")
	}
	// Directives off ⇒ the directive-channel fields stay zero.
	if cfg.Directives.Verifier != nil {
		t.Error("DirectiveVerifier set with directives disabled")
	}
	if cfg.Directives.InstanceID != "" || cfg.Directives.AuditPath != "" || cfg.Directives.ExpiryMax != 0 {
		t.Errorf("directive fields set with directives disabled: id=%q audit=%q max=%v",
			cfg.Directives.InstanceID, cfg.Directives.AuditPath, cfg.Directives.ExpiryMax)
	}
}

func TestDecorateConfigWiresDirectiveFieldsWhenEnabled(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "signers") // directives ON
	cfg, st := decorateForTest(t, app, "v0")

	if cfg.Directives.Verifier == nil {
		t.Fatal("DirectiveVerifier is nil with directives enabled")
	}
	if cfg.Directives.InstanceID != "demo" {
		t.Errorf("InstanceID = %q, want app name", cfg.Directives.InstanceID)
	}
	if want := filepath.Join(st.Dir, "directives.log"); cfg.Directives.AuditPath != want {
		t.Errorf("DirectiveAuditPath = %q, want %q", cfg.Directives.AuditPath, want)
	}
	if cfg.Directives.ExpiryMax != 24*time.Hour {
		t.Errorf("DirectiveExpiryMax = %v, want 24h (default)", cfg.Directives.ExpiryMax)
	}
}

// freePort grabs an OS-assigned loopback TCP port then releases it, so a
// subsequent bind of that number almost always succeeds without colliding with
// the fixed default broker ports.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestBindListenersBindsAllThreeAndChmodsSocket(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "signers") // directives ON ⇒ UDS bound
	app.Broker.JailPort = freePort(t)
	app.Broker.AdminPort = freePort(t)

	// os.MkdirTemp (not t.TempDir): the deep t.TempDir path + ".lever-state" +
	// "directive.sock" overruns macOS's ~104-byte unix socket path limit.
	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)

	if jailLn == nil || adminLn == nil || dirLn == nil {
		t.Fatalf("listeners = (%v,%v,%v), want all non-nil", jailLn, adminLn, dirLn)
	}
	// The UDS was created at the directive-sock path and chmod'd to 0600.
	fi, err := os.Stat(st.DirectiveSock())
	if err != nil {
		t.Fatalf("stat directive socket: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("directive socket mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestBindListenersNoSocketWhenDirectivesDisabled(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "") // directives OFF
	app.Broker.JailPort = freePort(t)
	app.Broker.AdminPort = freePort(t)

	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)

	if dirLn != nil {
		t.Error("dirLn is non-nil with directives disabled, want nil (no channel)")
	}
	if _, err := os.Stat(st.DirectiveSock()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("directive socket exists with directives disabled: %v", err)
	}
}

func TestBindListenersClosesJailWhenAdminBindFails(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "") // directives off is irrelevant; admin fails first
	// Point jail and admin at the SAME port: jail binds, admin collides.
	port := freePort(t)
	app.Broker.JailPort = port
	app.Broker.AdminPort = port

	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
	if err == nil {
		daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)
		t.Fatal("bindListeners succeeded, want admin-bind failure on the shared port")
	}
	if jailLn != nil || adminLn != nil || dirLn != nil || opLn != nil {
		t.Fatalf("failure returned non-nil listeners: (%v,%v,%v,%v)", jailLn, adminLn, dirLn, opLn)
	}
	// The jail listener must have been closed on the failure path — proven by
	// the port being re-bindable now.
	reln, rerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if rerr != nil {
		t.Fatalf("jail port %d still bound after failure — jail listener leaked: %v", port, rerr)
	}
	_ = reln.Close()
}

// shortStateDir is a state dir outside tree whose socket paths fit the
// macOS limit (t.TempDir paths do not).
func shortStateDir(t *testing.T) state.State {
	t.Helper()
	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	return st
}

// TestBindListenersBindsTheOperatorSocket: the operator note socket is bound
// with directives off, 0600, replacing a stale one.
func TestBindListenersBindsTheOperatorSocket(t *testing.T) {
	app := wiringApp(t.TempDir(), "") // directives OFF
	app.Broker.JailPort = freePort(t)
	app.Broker.AdminPort = freePort(t)
	st := shortStateDir(t)
	if err := os.WriteFile(st.OperatorSock(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)
	if opLn == nil || dirLn != nil {
		t.Fatalf("opLn = %v, dirLn = %v; want the operator socket only", opLn, dirLn)
	}
	fi, err := os.Stat(st.OperatorSock())
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("operator socket = %v, %v; want a 0600 socket", fi, err)
	}
}

// TestBindListenersNoOperatorSocketInsideTheTree: with the state directory
// inside the tree the operator socket is not bound (an agent could reach it).
func TestBindListenersNoOperatorSocketInsideTheTree(t *testing.T) {
	st := shortStateDir(t)
	app := wiringApp(filepath.Dir(st.Dir), "")
	app.Broker.JailPort = freePort(t)
	app.Broker.AdminPort = freePort(t)
	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)
	if opLn != nil {
		t.Fatal("operator socket bound inside the tree")
	}
	if _, err := os.Stat(st.OperatorSock()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("operator socket exists inside the tree: %v", err)
	}
}

// TestBindSocketRefusesAnOverlongPath: a socket path past the platform limit
// is a clear error, not a bind failure deep in the kernel.
func TestBindSocketRefusesAnOverlongPath(t *testing.T) {
	sock := filepath.Join(t.TempDir(), strings.Repeat("d", 120), "operator.sock")
	if _, err := bindSocket(sock); err == nil || !strings.Contains(err.Error(), "longer than a UNIX socket path") {
		t.Fatalf("err = %v", err)
	}
}

// TestDecorateConfigWiresVerification: outside the tree the broker keeps the
// sent ledger and the record of uses in the state dir; with remote access on
// the web senders are the sign-ins' labels; verified chat needs allowed_users.
func TestDecorateConfigWiresVerification(t *testing.T) {
	app := wiringApp(t.TempDir(), "")
	app.Remote = config.Remote{Enabled: true, AllowedUsers: []config.RemoteUser{{Login: "Me@Example.com"}}}
	cfg, st := decorateForTest(t, app, "v1")
	c := cfg.Chat
	if !c.Configured || c.LedgerPath != st.ChatLedger() || c.SentLedgerDir != st.SentLedger() || c.UsedPath != st.ChatVerified() {
		t.Fatalf("chat config = %+v", c)
	}
	if len(c.WebSenders) != 1 || c.WebSenders[0] != "user:me@example.com" {
		t.Fatalf("web senders = %v", c.WebSenders)
	}
	app.Remote = config.Remote{}
	cfg, st = decorateForTest(t, app, "v1")
	if c := cfg.Chat; c.Configured || c.LedgerPath != "" || len(c.WebSenders) != 0 || c.SentLedgerDir != st.SentLedger() {
		t.Fatalf("remote off: chat config = %+v", c)
	}
}
