package brokerctl

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/scion"
)

// HostScionClient builds the host-side scion client used by every operator-side
// verb (apply's start-manager, stop's suspend, attach, msg send, worker purge,
// and the broker's own worker-dispatch runtime). It threads the loopback hub
// endpoint (DefaultHubEndpoint is load-bearing: an empty HubEndpoint would omit
// SCION_HUB_ENDPOINT and defer to the scion binary's default) and a lazy
// HubTokenSource that reads the controller PAT from state at call time — so a
// PAT minted mid-apply is picked up live (see scion.Options.HubTokenSource).
//
// agentRole is the instance's configured scion.agent_role; empty omits the
// --role flag (see config.ScionConfig.AgentRole). It is threaded in rather than
// read from config here so package scion stays a thin, config-free wrapper.
func HostScionClient(jr leverexec.Runner, st State, agentRole string) *scion.Client {
	return scion.New(jr, scion.Options{
		HubEndpoint:    scion.DefaultHubEndpoint,
		HubTokenSource: func() string { t, _ := st.LoadControllerPAT(); return t },
		AgentRole:      agentRole,
	})
}

// writePIDFile records the running broker's pid at state.PID() (0600), after
// its listeners have bound — so a broker.pid on disk means a broker is (or was)
// actually serving, never a failed-bind ghost. Returns an error: a pid file we
// cannot write is a doctor blind spot, so callers treat it as fatal.
func writePIDFile(state State) error {
	pidFile := state.PID()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o700); err != nil {
		return fmt.Errorf("brokerctl: pid dir: %w", err)
	}
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("brokerctl: write pid: %w", err)
	}
	return nil
}

// removePIDFile deletes the pid file on shutdown. A removal failure is a
// warning, not fatal (the process is exiting anyway); an already-absent file
// is fine.
func removePIDFile(state State) {
	if err := os.Remove(state.PID()); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "lever: warning: could not remove %s: %v\n", state.PID(), err)
	}
}

// Serve runs the host-side broker for app until ctx is cancelled: ensure keys +
// revocation state, build the broker, pre-bind both loopback listeners (learning
// OS-assigned ports), issue the server cert, supervise the first-party tools, and
// serve. The supervisor is torn down on shutdown. version is this binary's
// version string, reported by /epoch alongside ConfigHash(app) so apply's
// broker-reuse shortcut can detect a stale broker (#19).
func Serve(ctx context.Context, app *config.App, state State, version string) error {
	kp, caInst, err := state.EnsureKeys()
	if err != nil {
		return err
	}

	machine := "lever-" + app.Name
	// app.Backend was validated selectable at config.Load, so this cannot pick a
	// planned backend; routing through the registry keeps the mount dest coming
	// from the SELECTED backend rather than a hardwired one.
	be, err := registry.Select(app.Backend, leverexec.RealRunner{}, machine)
	if err != nil {
		return err
	}

	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		return err
	}
	if err := decorateConfig(&cfg, app, state, be, version); err != nil {
		return err
	}

	// Persist the broker's audit decisions (provision/enrol/request/revoke …) to
	// the state-dir log. Without this the broker defaults to a discard logger, so
	// every allow/deny — the first thing you need when a worker can't enrol — is
	// lost. Opened before broker.New so cfg.Log is set; reused by the supervisor.
	logf, err := os.OpenFile(state.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cfg.Log = slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b := broker.New(cfg)

	jailLn, adminLn, dirLn, err := bindListeners(app, state)
	if err != nil {
		return err
	}
	// Own the listeners until ServeListeners takes over: any early return below
	// (pid file, cert source, supervisor) must not leak a bound port. Once we
	// hand off to ServeListeners the flag flips — it closes all three on its own
	// fail-closed paths, and a double-close would be benign anyway (ErrClosed).
	served := false
	defer func() {
		if !served {
			closeListeners(jailLn, adminLn, dirLn)
		}
	}()
	adminURL := "http://" + adminLn.Addr().String()

	// The serve process owns its pid file: written now that both listeners are
	// bound (a pid on disk ⇒ a broker actually serving, not a failed-bind ghost),
	// removed when we stop. This also makes a manual `lever broker serve`
	// doctor-visible, which a parent-written pid never was.
	if err := writePIDFile(state); err != nil {
		return err
	}
	defer removePIDFile(state)

	// Broker server cert: a self-rotating SOURCE, not a one-shot mint — leaf
	// certs live certTTL (24h), and a broker that outlives its serving cert
	// fails every gateway handshake, including the agents' own /renew calls,
	// so the whole fleet's certs decay behind it. SANs: always the selected
	// backend's host alias (cfg.ServerName, e.g. host.orb.internal) as DNS;
	// additionally the jail's resolved host-alias IP (passed by `lever apply`
	// via $LEVER_HOST_ALIAS_IP) so agents under closed-internet egress can dial
	// the broker by IP — DNS/53 is dropped in that posture, so they cannot
	// resolve the hostname and instead connect to the already-allowlisted alias
	// IP, which TLS validates against this IP SAN. Absent (e.g. a direct
	// `lever broker serve`), fall back to the hostname-only cert.
	certSrc, err := caInst.NewServerCertSource(cfg.ServerName, []string{cfg.ServerName}, []string{os.Getenv("LEVER_HOST_ALIAS_IP")})
	if err != nil {
		return err
	}

	sup := NewSupervisor(app.Broker.Tools, adminURL, state.ToolLogDir())
	if err := sup.Start(ctx); err != nil {
		return err
	}
	defer sup.Stop()

	served = true
	return b.ServeListeners(ctx, jailLn, adminLn, dirLn, certSrc)
}

// decorateConfig fills the host-side, config-derived fields of a broker.Config
// that BuildBroker leaves unset: the persisted revocation/directive state and
// their write-through closures, the operator-directive verifier, the selected
// backend's server name + mount dest, the worker-dispatch runtime, the worker
// specs + instance project, and the identity/URL fields. It is the exact wiring
// Serve applies between BuildBroker and broker.New; single_project_dispatch_test
// drives this same function so the two can never drift.
func decorateConfig(cfg *broker.Config, app *config.App, state State, be backend.Backend, version string) error {
	rev, err := state.LoadRevocation()
	if err != nil {
		return err
	}
	dirs, err := state.LoadDirectives()
	if err != nil {
		return err
	}
	cfg.RevocationState = rev
	cfg.PersistRevocation = state.SaveRevocation
	cfg.DirectiveState = dirs
	cfg.PersistDirectives = state.SaveDirectives
	if app.DirectivesEnabled() {
		cfg.DirectiveVerifier = &opsig.Verifier{AllowedSigners: app.OperatorAllowedSignersPath(), Principal: app.OperatorPrincipal()}
		cfg.InstanceID = app.Name
		cfg.DirectiveAuditPath = filepath.Join(state.Dir, "directives.log")
		cfg.DirectiveExpiryMax = app.EffectiveDirectiveExpiryMax()
	}
	cfg.ServerName = be.HostToolAlias()

	jailMount := be.MountDest()
	// Worker dispatch runs host-side with operator identity (jail runner). apply
	// passes the resolved run-user/uid via env (LEVER_JAIL_USER/UID). Without the
	// env (manual `broker serve` with no prior apply) cfg.Runtime stays nil; the
	// worker handlers detect this via runtimeReady and return 502 — they do not
	// panic. apply is the real path.
	if u, id := os.Getenv("LEVER_JAIL_USER"), os.Getenv("LEVER_JAIL_UID"); u != "" && id != "" {
		jr, jerr := registry.JailRunner(app.Backend, leverexec.RealRunner{}, "lever-"+app.Name, u, id)
		if jerr != nil {
			return jerr
		}
		// HostScionClient lets the broker's own worker-dispatch scion client
		// (host-side, operator identity) authenticate against the real,
		// dev-auth-off hub with the controller PAT minted by `lever apply`'s
		// bootstrap-token step (see internal/cli/apply.go's ensureControllerPAT).
		sc := HostScionClient(jr, state, app.Scion.AgentRole)
		cfg.Runtime = sc
		// Worker resume meets the same pre-role record hazard as the manager's
		// (see broker.Config.VerifyAgentRole). The hub read rides the same jail
		// runner and controller PAT as every other lever hub call.
		hc := &hubapi.Client{T: &hubapi.JailCurl{
			Runner:  jr,
			BaseURL: scion.DefaultHubEndpoint,
			Token:   func() string { t, _ := state.LoadControllerPAT(); return t },
		}}
		projectKey := filepath.Base(jailMount)
		cfg.VerifyAgentRole = func(ctx context.Context, agent string) error {
			return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, projectKey, agent)
		}
		// /msg/list cuts the controller's fleet-wide notification feed down to
		// one agent, and needs the hub's agent id to attribute events (see
		// broker.Config.ResolveAgentID).
		cfg.ResolveAgentID = func(ctx context.Context, agentSlug string) (string, error) {
			return hc.AgentID(ctx, projectKey, scion.DefaultHubEndpoint, agentSlug)
		}
	}
	cfg.Workers = WorkerSpecs(app, jailMount)
	cfg.InstanceProject = jailMount
	// The manager's scion agent slug is the APP NAME (apply's start-manager
	// dispatches the manager as Worker: app.Name), not the manager cert CN.
	cfg.ManagerSlug = app.Name
	// Natural-lapse auto-re-enrol (#22): mode from config; the manager's
	// bootstrap.json lives at <tree>/.lever (mint-manager-bootstrap stages it
	// there; workers carry their dir in WorkerSpec.BootstrapDir).
	cfg.AutoReenrol = string(app.EffectiveAutoReenrol())
	cfg.ManagerBootstrapDir = filepath.Join(app.Tree, ".lever")
	// Confinement anchor for every bootstrap.json the broker stages (see
	// broker.Config.Tree): the mount point, which no agent can replace.
	cfg.Tree = app.Tree
	cfg.Version = version
	cfg.ConfigHash = ConfigHash(app)
	cfg.WorkerToWorker = app.WorkerToWorkerMessaging()
	if caPEM, err := os.ReadFile(state.CACert()); err == nil {
		cfg.BrokerCAPEM = string(caPEM)
	} else {
		fmt.Fprintf(os.Stderr, "lever: warning: broker CA read: %v\n", err)
	}
	host := os.Getenv("LEVER_HOST_ALIAS_IP")
	if host == "" {
		host = cfg.ServerName
	}
	cfg.BrokerURL = workerBrokerURL(host, app.EffectiveJailPort())
	return nil
}

// closeListeners closes every non-nil listener, discarding errors: a
// double-close (ServeListeners may already have closed them on a fail-closed
// path) surfaces as ErrClosed, which is benign here.
func closeListeners(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

// bindListeners pre-binds the broker's loopback listeners so Serve learns the
// OS-assigned ports before serving: the jail-facing TCP listener, the admin TCP
// listener, and — only when operator directives are enabled — the directive UDS
// (0600, gated by filesystem permissions rather than network origin; a stale
// socket from an unclean shutdown is removed first). dirLn is nil when
// directives are disabled — ServeListeners treats a nil directiveLn as "no
// channel". On any bind/chmod failure every already-bound listener is closed so
// no port leaks, and (nil, nil, nil, err) is returned.
func bindListeners(app *config.App, state State) (jailLn, adminLn, dirLn net.Listener, err error) {
	jailLn, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveJailPort()))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("brokerctl: bind jail listener: %w", err)
	}
	adminLn, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveAdminPort()))
	if err != nil {
		closeListeners(jailLn)
		return nil, nil, nil, fmt.Errorf("brokerctl: bind admin listener: %w", err)
	}
	if app.DirectivesEnabled() {
		sock := state.DirectiveSock()
		_ = os.Remove(sock) // stale socket from an unclean shutdown
		ul, lerr := net.Listen("unix", sock)
		if lerr != nil {
			closeListeners(jailLn, adminLn)
			return nil, nil, nil, fmt.Errorf("brokerctl: bind directive socket: %w", lerr)
		}
		if cerr := os.Chmod(sock, 0o600); cerr != nil {
			closeListeners(jailLn, adminLn, ul)
			return nil, nil, nil, fmt.Errorf("brokerctl: chmod directive socket: %w", cerr)
		}
		dirLn = ul
	}
	return jailLn, adminLn, dirLn, nil
}
