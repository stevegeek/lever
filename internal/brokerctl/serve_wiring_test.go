package brokerctl

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
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
func decorateForTest(t *testing.T, app *config.App, version string) (broker.Config, State) {
	t.Helper()
	// Deterministic env: unset the jail-runner and host-alias hooks so
	// decorateConfig takes the no-Runtime path and BrokerURL falls back to the
	// backend's host alias.
	t.Setenv("LEVER_JAIL_USER", "")
	t.Setenv("LEVER_JAIL_UID", "")
	t.Setenv("LEVER_HOST_ALIAS_IP", "")

	kp, err := token.Generate()
	if err != nil {
		t.Fatalf("token.Generate: %v", err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatalf("ca.Generate: %v", err)
	}
	be, err := registry.Select(app.Backend, leverexec.RealRunner{}, "lever-"+app.Name)
	if err != nil {
		t.Fatalf("registry.Select: %v", err)
	}
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	state := StateDir(t.TempDir())
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := decorateConfig(&cfg, app, state, be, version); err != nil {
		t.Fatalf("decorateConfig: %v", err)
	}
	return cfg, state
}

func TestDecorateConfigWiresConfigDerivedFields(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "") // directives OFF
	cfg, _ := decorateForTest(t, app, "v1.2.3")

	if cfg.ManagerSlug != "demo" {
		t.Errorf("ManagerSlug = %q, want app name %q", cfg.ManagerSlug, "demo")
	}
	if len(cfg.Workers) != 2 {
		t.Fatalf("Workers = %d, want 2 (WorkerSpecs of the two config workers)", len(cfg.Workers))
	}
	// InstanceProject + the mount are the SELECTED backend's dest, not hardwired.
	if cfg.InstanceProject != "/lever" {
		t.Errorf("InstanceProject = %q, want %q (backend MountDest)", cfg.InstanceProject, "/lever")
	}
	if cfg.ServerName != "host.orb.internal" {
		t.Errorf("ServerName = %q, want orbstack HostToolAlias", cfg.ServerName)
	}
	// host falls back to ServerName (LEVER_HOST_ALIAS_IP unset); port is the
	// default jail port.
	if want := "https://host.orb.internal:8443"; cfg.BrokerURL != want {
		t.Errorf("BrokerURL = %q, want %q", cfg.BrokerURL, want)
	}
	if cfg.Version != "v1.2.3" {
		t.Errorf("Version = %q, want the passed-in version", cfg.Version)
	}
	if cfg.ConfigHash != ConfigHash(app) {
		t.Errorf("ConfigHash = %q, want ConfigHash(app) %q", cfg.ConfigHash, ConfigHash(app))
	}
	if !cfg.WorkerToWorker {
		t.Error("WorkerToWorker = false, want true (default)")
	}
	if cfg.AutoReenrol != "all" {
		t.Errorf("AutoReenrol = %q, want %q (default)", cfg.AutoReenrol, "all")
	}
	if want := filepath.Join(tree, ".lever"); cfg.ManagerBootstrapDir != want {
		t.Errorf("ManagerBootstrapDir = %q, want %q", cfg.ManagerBootstrapDir, want)
	}
	// Persist closures are the state's writers — non-nil so the broker can
	// write revocation/directive state through on mutation.
	if cfg.PersistRevocation == nil {
		t.Error("PersistRevocation is nil, want state.SaveRevocation")
	}
	if cfg.PersistDirectives == nil {
		t.Error("PersistDirectives is nil, want state.SaveDirectives")
	}
	// No jail-runner env ⇒ no worker-dispatch runtime is wired.
	if cfg.Runtime != nil {
		t.Error("Runtime is non-nil, want nil without LEVER_JAIL_USER/UID")
	}
	// Directives off ⇒ the directive-channel fields stay zero.
	if cfg.DirectiveVerifier != nil {
		t.Error("DirectiveVerifier set with directives disabled")
	}
	if cfg.InstanceID != "" || cfg.DirectiveAuditPath != "" || cfg.DirectiveExpiryMax != 0 {
		t.Errorf("directive fields set with directives disabled: id=%q audit=%q max=%v",
			cfg.InstanceID, cfg.DirectiveAuditPath, cfg.DirectiveExpiryMax)
	}
}

func TestDecorateConfigWiresDirectiveFieldsWhenEnabled(t *testing.T) {
	tree := t.TempDir()
	app := wiringApp(tree, "signers") // directives ON
	cfg, state := decorateForTest(t, app, "v0")

	if cfg.DirectiveVerifier == nil {
		t.Fatal("DirectiveVerifier is nil with directives enabled")
	}
	if cfg.InstanceID != "demo" {
		t.Errorf("InstanceID = %q, want app name", cfg.InstanceID)
	}
	if want := filepath.Join(state.Dir, "directives.log"); cfg.DirectiveAuditPath != want {
		t.Errorf("DirectiveAuditPath = %q, want %q", cfg.DirectiveAuditPath, want)
	}
	if cfg.DirectiveExpiryMax != 24*time.Hour {
		t.Errorf("DirectiveExpiryMax = %v, want 24h (default)", cfg.DirectiveExpiryMax)
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
	state := StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, err := bindListeners(app, state)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer closeListeners(jailLn, adminLn, dirLn)

	if jailLn == nil || adminLn == nil || dirLn == nil {
		t.Fatalf("listeners = (%v,%v,%v), want all non-nil", jailLn, adminLn, dirLn)
	}
	// The UDS was created at the directive-sock path and chmod'd to 0600.
	fi, err := os.Stat(state.DirectiveSock())
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

	state := StateDir(t.TempDir())
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, err := bindListeners(app, state)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer closeListeners(jailLn, adminLn, dirLn)

	if dirLn != nil {
		t.Error("dirLn is non-nil with directives disabled, want nil (no channel)")
	}
	if _, err := os.Stat(state.DirectiveSock()); !errors.Is(err, fs.ErrNotExist) {
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

	state := StateDir(t.TempDir())
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	jailLn, adminLn, dirLn, err := bindListeners(app, state)
	if err == nil {
		closeListeners(jailLn, adminLn, dirLn)
		t.Fatal("bindListeners succeeded, want admin-bind failure on the shared port")
	}
	if jailLn != nil || adminLn != nil || dirLn != nil {
		t.Fatalf("failure returned non-nil listeners: (%v,%v,%v)", jailLn, adminLn, dirLn)
	}
	// The jail listener must have been closed on the failure path — proven by
	// the port being re-bindable now.
	reln, rerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if rerr != nil {
		t.Fatalf("jail port %d still bound after failure — jail listener leaked: %v", port, rerr)
	}
	_ = reln.Close()
}
