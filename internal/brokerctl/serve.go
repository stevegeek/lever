package brokerctl

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/daemon"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// ServeEnv is what `lever apply` hands the detached `broker serve` child
// through the environment (see internal/cli/apply.go detachedSelfCmd).
// Without it (a manual `lever broker serve` with no prior apply) the
// worker-dispatch runtime stays unset and the server cert carries no IP SAN.
type ServeEnv struct {
	// JailUser and JailUID are the resolved run-user inside the jail
	// ($LEVER_JAIL_USER / $LEVER_JAIL_UID); both must be set for the broker
	// to build its host-side worker-dispatch runtime.
	JailUser, JailUID string
	// HostAliasIP is the jail's resolved host-alias IP ($LEVER_HOST_ALIAS_IP),
	// added to the server cert as an IP SAN and used as the broker URL host
	// agents dial under closed-internet egress.
	HostAliasIP string
}

// ServeEnvFromOS reads ServeEnv from the process environment.
func ServeEnvFromOS() ServeEnv {
	return ServeEnv{
		JailUser:    os.Getenv("LEVER_JAIL_USER"),
		JailUID:     os.Getenv("LEVER_JAIL_UID"),
		HostAliasIP: os.Getenv("LEVER_HOST_ALIAS_IP"),
	}
}

// machineName is the jail's machine name for app.
func machineName(app *config.App) string { return "lever-" + app.Name }

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
// workerLiveSettle is DispatchConfig.LiveSettle in production (see there).
const workerLiveSettle = 10 * time.Second

func HostScionClient(jr proc.Runner, st state.State, agentRole string) *scion.Client {
	return scion.New(jr, scion.Options{
		HubEndpoint:    scion.DefaultHubEndpoint,
		HubTokenSource: controllerPAT(st),
		AgentRole:      agentRole,
	})
}

// controllerPAT is a lazy reader of the controller PAT in st, so a PAT minted
// mid-apply is picked up live; a read failure reads as "no token".
func controllerPAT(st state.State) func() string {
	return func() string { t, _ := st.LoadControllerPAT(); return t }
}

// Serve runs the host-side broker for app until ctx is cancelled: ensure keys +
// revocation state, build the broker, pre-bind both loopback listeners (learning
// OS-assigned ports), issue the server cert, supervise the first-party tools, and
// serve. The supervisor is torn down on shutdown. version is this binary's
// version string, reported by /epoch alongside ConfigHash(app) so apply's
// broker-reuse shortcut can detect a stale broker (#19). env is what apply
// passed through the environment (ServeEnvFromOS).
func Serve(ctx context.Context, app *config.App, st state.State, version string, env ServeEnv) error {
	kp, caInst, err := EnsureKeys(st)
	if err != nil {
		return err
	}

	// app.Backend was validated selectable at config.Load, so this cannot pick a
	// planned backend; routing through the registry keeps the mount dest coming
	// from the SELECTED backend rather than a hardwired one.
	be, err := registry.Select(app.Backend, proc.RealRunner{}, machineName(app))
	if err != nil {
		return err
	}

	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		return err
	}
	if err := decorateConfig(&cfg, app, st, be, version, env); err != nil {
		return err
	}

	// Persist the broker's audit decisions (provision/enrol/request/revoke …) to
	// the state-dir log. Without this the broker defaults to a discard logger, so
	// every allow/deny — the first thing you need when a worker can't enrol — is
	// lost. Opened before broker.New so cfg.Log is set; reused by the supervisor.
	logf, err := os.OpenFile(st.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cfg.Log = slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Per-boot broker-to-tool shared secret: the gateway presents it to every
	// first-party tool, the supervisor hands it to each tool it spawns, and a
	// tool refuses any request without it (a direct dial past the broker).
	toolSecret, err := NewToolSecret()
	if err != nil {
		return err
	}
	cfg.ToolSecret = toolSecret

	b := broker.New(cfg)

	jailLn, adminLn, dirLn, opLn, err := bindListeners(app, st)
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
			daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)
		}
	}()
	adminURL := "http://" + adminLn.Addr().String()

	// The serve process owns its pid file: written now that both listeners are
	// bound (a pid on disk ⇒ a broker actually serving, not a failed-bind ghost),
	// removed when we stop. This also makes a manual `lever broker serve`
	// doctor-visible, which a parent-written pid never was.
	if err := daemon.WritePIDFile(st.PID()); err != nil {
		return err
	}
	defer daemon.RemovePIDFile(st.PID())

	// Broker server cert: a self-rotating SOURCE, not a one-shot mint — leaf
	// certs live certTTL (24h), and a broker that outlives its serving cert
	// fails every gateway handshake, including the agents' own /renew calls,
	// so the whole fleet's certs decay behind it. SANs: always the selected
	// backend's host alias (be.HostToolAlias(), e.g. host.orb.internal) as DNS;
	// additionally the jail's resolved host-alias IP (passed by `lever apply`
	// via $LEVER_HOST_ALIAS_IP) so agents under closed-internet egress can dial
	// the broker by IP — DNS/53 is dropped in that posture, so they cannot
	// resolve the hostname and instead connect to the already-allowlisted alias
	// IP, which TLS validates against this IP SAN. Absent (e.g. a direct
	// `lever broker serve`), fall back to the hostname-only cert.
	serverName := be.HostToolAlias()
	certSrc, err := caInst.NewServerCertSource(serverName, []string{serverName}, []string{env.HostAliasIP})
	if err != nil {
		return err
	}

	sup := NewSupervisor(ToolSpecs(app), adminURL, st.ToolLogDir(), toolSecret)
	if err := sup.Start(ctx); err != nil {
		return err
	}
	defer sup.Stop()

	served = true
	return b.ServeListeners(ctx, jailLn, adminLn, dirLn, opLn, certSrc)
}

// decorateConfig fills the host-side groups of a broker.Config that BuildBroker
// leaves unset — Persistence (state-dir revocation/directive state and their
// write-through closures), Directives (the operator-directive channel, when
// enabled), Dispatch (the worker-dispatch runtime, worker specs, instance
// project and URLs) — plus the /epoch Version/ConfigHash pair. It is the exact
// wiring Serve applies between BuildBroker and broker.New;
// single_project_dispatch_test drives this same function so the two can never
// drift.
func decorateConfig(cfg *broker.Config, app *config.App, st state.State, be backend.Backend, version string, env ServeEnv) error {
	pe, err := persistenceConfig(st)
	if err != nil {
		return err
	}
	d, err := dispatchConfig(app, st, be, env)
	if err != nil {
		return err
	}
	cfg.Persistence = pe
	cfg.Dispatch = d
	if app.DirectivesEnabled() {
		cfg.Directives = broker.DirectiveConfig{
			Verifier:   &opsig.Verifier{AllowedSigners: app.OperatorAllowedSignersPath(), Principal: app.OperatorPrincipal()},
			InstanceID: app.Name,
			AuditPath:  st.DirectiveAudit(),
			ExpiryMax:  app.EffectiveDirectiveExpiryMax(),
		}
	}
	// The host records agents verify their messages against: the chat
	// ledger (web posts, only with verified chat on) and the sent ledger
	// (lever's own sends), plus the record of which were verified. None of
	// them is kept inside the tree, where an agent could write it.
	cfg.Chat = broker.ChatConfig{Configured: ChatConfigured(app), LedgerPath: ChatLedgerPath(app, st), WebSenders: WebSenders(app)}
	if !StateInsideTree(app, st) {
		cfg.Chat.UsedPath = st.ChatVerified()
		cfg.Chat.SentLedgerDir = st.SentLedger()
	}
	// remote.agent_messages: the contacts each agent may message, the
	// limits, and the agent ledger (off inside the tree, like the others).
	cfg.AgentMessages = AgentMessages(app, st)
	// remote.files: the file exchange (share_file and its ledger).
	cfg.Files = Files(app, st)
	cfg.Dispatch.BeginSession = func(agent string) func() error { return BeginSession(app, st, version, agent) }
	cfg.Version = version
	cfg.ConfigHash = ConfigHash(app)
	return nil
}

// persistenceConfig loads the persisted revocation + directive state from st
// and wires their write-through closures.
func persistenceConfig(st state.State) (broker.PersistenceConfig, error) {
	rev, err := LoadRevocation(st)
	if err != nil {
		return broker.PersistenceConfig{}, err
	}
	dirs, err := LoadDirectives(st)
	if err != nil {
		return broker.PersistenceConfig{}, err
	}
	return broker.PersistenceConfig{
		Revocation:        rev,
		PersistRevocation: func(rs broker.RevocationState) error { return SaveRevocation(st, rs) },
		Directives:        dirs,
		PersistDirectives: func(ds broker.DirectiveState) error { return SaveDirectives(st, ds) },
	}, nil
}

// dispatchConfig builds the host-side worker-dispatch group: the worker specs
// and instance project from the selected backend's mount dest, the scion
// runtime + hub lookups when apply passed the jail run-user through env, and
// the CA/URL every staged bootstrap carries.
func dispatchConfig(app *config.App, st state.State, be backend.Backend, env ServeEnv) (broker.DispatchConfig, error) {
	jailMount := be.MountDest()
	d := broker.DispatchConfig{
		Workers:         WorkerSpecs(app, jailMount, env.JailUID),
		InstanceProject: jailMount,
		WorkerToWorker:  app.WorkerToWorkerMessaging(),
		// Natural-lapse auto-re-enrol (#22): mode from config; the manager's
		// bootstrap.json lives at <tree>/.lever (mint-manager-bootstrap stages it
		// there; a worker's goes to the guest through Tickets, see below).
		AutoReenrol:         string(app.EffectiveAutoReenrol()),
		ManagerBootstrapDir: filepath.Join(app.Tree, ".lever"),
		// Confinement anchor for every bootstrap.json the broker stages (see
		// broker.DispatchConfig.Tree): the mount point, which no agent can replace.
		Tree: app.Tree,
		// manager.read_only: worker workspaces may not be created through a
		// symlink, nor over a protected directory (see the field doc).
		ReadOnlyDirs: app.Manager.ReadOnly,
		// A dispatched worker must hold live for this long before the manager
		// hears "running" (lever#31): scion reports the record running before
		// the harness runs a line, and every observed harness death landed
		// within three seconds of that. Same value as apply's manager gate.
		LiveSettle: workerLiveSettle,
		// Agents under closed-internet egress dial the host-alias IP; otherwise
		// the backend's host alias hostname (the server cert's DNS SAN).
		BrokerURL: workerBrokerURL(cmp.Or(env.HostAliasIP, be.HostToolAlias()), app.EffectiveJailPort()),
	}
	if caPEM, err := os.ReadFile(st.CACert()); err == nil {
		d.BrokerCAPEM = string(caPEM)
	} else {
		daemon.Warnf("broker CA read: %v", err)
	}

	// Worker dispatch runs host-side with operator identity (jail runner). apply
	// passes the resolved run-user/uid via env (ServeEnv). Without the env
	// (manual `broker serve` with no prior apply) Runtime stays nil; the worker
	// handlers detect this via runtimeReady and return 502 — they do not panic.
	// apply is the real path.
	if env.JailUser == "" || env.JailUID == "" {
		return d, nil
	}
	jr, err := registry.JailRunner(app.Backend, proc.RealRunner{}, machineName(app), env.JailUser, env.JailUID)
	if err != nil {
		return broker.DispatchConfig{}, err
	}
	// HostScionClient lets the broker's own worker-dispatch scion client
	// (host-side, operator identity) authenticate against the real,
	// dev-auth-off hub with the controller PAT minted by `lever apply`'s
	// bootstrap-token step (see internal/cli/apply.go's ensureControllerPAT).
	sc := HostScionClient(jr, st, app.Scion.AgentRole)
	d.Runtime = sc
	// Worker enrolment tickets go to the guest runtime dir over the same jail
	// runner, never into the tree the manager mounts (jail.StageWorkerTicket;
	// the spec's TicketDir names the directory the worker container mounts).
	d.Tickets = jailTicketStager{jr}
	// Expired agent hub tokens are read in each agent's container and reset
	// with `scion reset-auth` under the controller PAT (broker tokenwatch.go).
	d.HubTokens = hubTokenHealer{probe: jail.AgentProbe{R: jr}, sc: sc, project: jailMount}
	// Worker resume meets the same pre-role record hazard as the manager's
	// (see broker.DispatchConfig.VerifyAgentRole). The hub read rides the same
	// jail runner and controller PAT as every other lever hub call.
	hc := &hubapi.Client{T: &hubapi.JailCurl{
		Runner:  jr,
		BaseURL: scion.DefaultHubEndpoint,
		Token:   controllerPAT(st),
	}}
	projectKey := filepath.Base(jailMount)
	d.VerifyAgentRole = func(ctx context.Context, agent string) error {
		return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, projectKey, agent)
	}
	// /msg/list cuts the controller's fleet-wide notification feed down to
	// one agent, and needs the hub's agent id to attribute events (see
	// broker.DispatchConfig.ResolveAgentID).
	d.ResolveAgentID = func(ctx context.Context, agentSlug string) (string, error) {
		return hc.AgentID(ctx, projectKey, scion.DefaultHubEndpoint, agentSlug)
	}
	// Every message lever sends wears the controller PAT's hub user as its
	// sender; /message/verify learns that label from the hub, the way the hub
	// stamps it (the email, else the user id), rather than assuming the dev
	// user's default address.
	d.ResolveControllerSender = controllerSenderFrom(hc)
	return d, nil
}

// controllerSenderFrom resolves the envelope sender of the controller PAT's
// hub user from auth/me: "user:" + the email, else "user:" + the user id,
// the way scion stamps a user's sends (handlers_agent_messaging.go).
func controllerSenderFrom(hc *hubapi.Client) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		u, err := hc.Me(ctx)
		if err != nil {
			return "", err
		}
		if u.Email != "" {
			return "user:" + u.Email, nil
		}
		return "user:" + u.ID, nil
	}
}

// bindListeners pre-binds the broker's loopback listeners so Serve learns the
// OS-assigned ports before serving: the jail-facing TCP listener, the admin TCP
// listener, the directive UDS (only when operator directives are enabled) and
// the operator note UDS (only when the state directory is outside the tree).
// Both sockets are 0600, gated by filesystem permissions rather than network
// origin, and a stale socket from an unclean shutdown is removed first. dirLn
// or opLn is nil when its channel is off — ServeListeners treats nil as "no
// channel".
//
// The operator note socket stays unbound when the state directory is inside
// the tree: agents mount the tree, and on a backend where a socket in a shared
// mount is connectable from a container, the manager could post notes that
// record as the operator's. `lever msg send` then reports that the broker has
// no operator socket. An operator socket that cannot be bound (a state path too
// long for a socket) is reported and skipped, never fatal: nothing else
// depends on it, and doctor fails its row. On any other bind/chmod failure
// every already-bound listener is closed so no port leaks, and nil listeners
// are returned with the error.
func bindListeners(app *config.App, st state.State) (jailLn, adminLn, dirLn, opLn net.Listener, err error) {
	jailLn, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveJailPort()))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("brokerctl: bind jail listener: %w", err)
	}
	adminLn, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveAdminPort()))
	if err != nil {
		daemon.CloseListeners(jailLn)
		return nil, nil, nil, nil, fmt.Errorf("brokerctl: bind admin listener: %w", err)
	}
	if app.DirectivesEnabled() {
		ul, lerr := bindSocket(st.DirectiveSock())
		if lerr != nil {
			daemon.CloseListeners(jailLn, adminLn)
			return nil, nil, nil, nil, fmt.Errorf("brokerctl: directive socket: %w", lerr)
		}
		dirLn = ul
	}
	if !StateInsideTree(app, st) {
		ul, lerr := bindSocket(st.OperatorSock())
		if lerr != nil {
			daemon.Warnf("operator socket: %v; `lever msg send` is unavailable", lerr)
		} else {
			opLn = ul
		}
	}
	return jailLn, adminLn, dirLn, opLn, nil
}

// maxSocketPath is the longest UNIX socket path every host platform binds
// (macOS's sun_path holds 104 bytes with the terminating NUL; Linux's 108).
const maxSocketPath = 103

// bindSocket binds a UNIX socket at sock with mode 0600, removing a stale
// one first.
func bindSocket(sock string) (net.Listener, error) {
	if len(sock) > maxSocketPath {
		return nil, fmt.Errorf("%s is %d bytes, longer than a UNIX socket path may be (%d); move the state directory to a shorter path",
			sock, len(sock), maxSocketPath)
	}
	_ = os.Remove(sock) // stale socket from an unclean shutdown
	ul, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		daemon.CloseListeners(ul)
		return nil, fmt.Errorf("chmod: %w", err)
	}
	return ul, nil
}

// jailTicketStager is broker.TicketStager over the jail runner.
type jailTicketStager struct{ r proc.Runner }

func (s jailTicketStager) StageWorkerTicket(ctx context.Context, worker string, payload []byte) error {
	return jail.StageWorkerTicket(ctx, s.r, worker, payload)
}

// hubTokenHealer is broker.HubTokenHealer over the jail: the expiry is read
// in the agent's container by scion's container name, and the reset is the
// host scion client's `reset-auth`, which mints a token of the agent's own
// stored role.
type hubTokenHealer struct {
	probe   jail.AgentProbe
	sc      *scion.Client
	project string
}

func (h hubTokenHealer) TokenExpired(ctx context.Context, agent string) (bool, error) {
	t, err := h.probe.HubToken(ctx, jail.ContainerName(filepath.Base(h.project), agent))
	if err != nil {
		return false, err
	}
	return t.Expired(), nil
}

func (h hubTokenHealer) ResetAuth(ctx context.Context, agent string) error {
	return h.sc.ResetAuth(ctx, agent, h.project)
}
