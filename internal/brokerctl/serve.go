package brokerctl

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/daemon"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// ServeEnv is what `lever apply` hands the detached `broker serve` child
// through the environment (detachedSelfCmd). Without it (a manual `lever
// broker serve`) the worker-dispatch runtime stays unset and the server cert
// carries no IP SAN.
type ServeEnv struct {
	// JailUser and JailUID are the jail's run user; both must be set for the
	// broker to build its worker-dispatch runtime.
	JailUser, JailUID string
	// HostAliasIP is the jail's resolved host-alias IP, added to the server
	// cert as an IP SAN and used as the broker URL host under closed-internet
	// egress.
	HostAliasIP string
}

func ServeEnvFromOS() ServeEnv {
	return ServeEnv{
		JailUser:    os.Getenv("LEVER_JAIL_USER"),
		JailUID:     os.Getenv("LEVER_JAIL_UID"),
		HostAliasIP: os.Getenv("LEVER_HOST_ALIAS_IP"),
	}
}

func machineName(app *config.App) string { return "lever-" + app.Name }

// workerLiveSettle is DispatchConfig.LiveSettle in production.
const workerLiveSettle = 10 * time.Second

// HostScionClient builds the host-side scion client every operator-side verb
// uses. DefaultHubEndpoint is load-bearing: an empty HubEndpoint would defer
// to the scion binary's default. The controller PAT is read at call time, so
// a PAT minted mid-apply is picked up. An empty agentRole omits --role.
func HostScionClient(jr proc.Runner, st state.State, agentRole string) *scion.Client {
	return scion.New(jr, scion.Options{
		HubEndpoint:     scion.DefaultHubEndpoint,
		HubTokenSource:  controllerPAT(st),
		AgentRole:       agentRole,
		SettingsWarnLog: StateLogAppender(st),
	})
}

// StateLogAppender returns a sink that appends one line per call to the
// state directory's broker.log (O_APPEND keeps a record whole beside the
// broker's own writes). It keeps scion lines lever hides from the console in
// the log. A failure to open the file drops the line: it is only a record.
func StateLogAppender(st state.State) func(line string) {
	return func(line string) {
		f, err := os.OpenFile(st.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		logging.New(f, "lever").Info("scion output hidden from the console",
			"reason", "known-benign settings warning (lever#169)", "line", line)
	}
}

// controllerPAT reads the PAT at call time, so a PAT minted mid-apply is
// picked up; a read failure reads as "no token".
func controllerPAT(st state.State) func() string {
	return func() string { t, _ := st.LoadControllerPAT(); return t }
}

// Serve runs the host-side broker for app until ctx is cancelled. version is
// reported by /epoch alongside ConfigHash(app) so apply can detect a stale
// broker. env is what apply passed through the environment. jobs are host
// work the broker runs while it serves; each ends when its context does.
func Serve(ctx context.Context, app *config.App, st state.State, version string, env ServeEnv, jobs ...func(context.Context)) error {
	kp, caInst, err := EnsureKeys(st)
	if err != nil {
		return err
	}

	// Routing through the registry keeps the mount dest coming from the
	// selected backend.
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

	// The broker's audit decisions go to the state-dir log; without it the
	// broker discards them. Reused by the supervisor.
	logf, err := os.OpenFile(st.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cfg.Log = logging.New(logf, "broker")

	// Per-boot broker-to-tool shared secret: the gateway presents it to every
	// first-party tool, and a tool refuses any request without it (a direct
	// dial past the broker).
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
	// Own the listeners until ServeListeners takes over: an early return below
	// must not leak a bound port. ServeListeners closes them on its own
	// fail-closed paths.
	served := false
	defer func() {
		if !served {
			daemon.CloseListeners(jailLn, adminLn, dirLn, opLn)
		}
	}()
	adminURL := "http://" + adminLn.Addr().String()

	// Written once both listeners are bound, so a pid on disk means a broker
	// actually serving; removed when we stop.
	if err := daemon.WritePIDFile(st.PID()); err != nil {
		return err
	}
	defer daemon.RemovePIDFile(st.PID())

	// A self-rotating serving cert: a broker that outlives a one-shot cert
	// fails every gateway handshake, including agents' /renew, and the fleet's
	// certs decay behind it. SANs: the backend's host alias as DNS, plus the
	// jail's host-alias IP when apply passed it, since under closed-internet
	// egress DNS is dropped and agents dial the allowlisted IP.
	serverName := be.HostToolAlias()
	certSrc, err := caInst.NewServerCertSource(serverName, []string{serverName}, []string{env.HostAliasIP})
	if err != nil {
		return err
	}

	specs, err := ToolSpecs(app)
	if err != nil {
		return err
	}
	sup := NewSupervisor(specs, adminURL, st.ToolLogDir(), toolSecret)
	// A tool whose program sits in the tree under manager.read_only starts
	// only once the running manager holds those mounts. Without the jail run
	// user (a manual `broker serve`) there is no way to look, and it stays
	// down.
	if env.JailUser != "" && env.JailUID != "" {
		jr, err := registry.JailRunner(app.Backend, proc.RealRunner{}, machineName(app), env.JailUser, env.JailUID)
		if err != nil {
			return err
		}
		sup.ReadOnlyGuard = readOnlyGuard(app, jr, be.MountDest())
	}
	if err := sup.Start(ctx); err != nil {
		return err
	}
	defer sup.Stop()

	// Started only once this broker holds the instance's ports: a second
	// broker for the same instance fails to bind and runs none.
	jobCtx, cancelJobs := context.WithCancel(ctx)
	var jobsDone sync.WaitGroup
	defer func() {
		cancelJobs()
		jobsDone.Wait()
	}()
	for _, job := range jobs {
		jobsDone.Go(func() { job(jobCtx) })
	}

	served = true
	return b.ServeListeners(ctx, jailLn, adminLn, dirLn, opLn, certSrc)
}

// decorateConfig fills the host-side groups of a broker.Config that
// BuildBroker leaves unset (Persistence, Directives, Dispatch) plus the
// /epoch Version, ConfigHash and InstanceID. Serve and
// single_project_dispatch_test both use it so the wiring cannot drift.
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
	if cfg.ServiceHosts, err = ServiceHostRoutes(app, st); err != nil {
		return err
	}
	if app.DirectivesEnabled() {
		cfg.Directives = broker.DirectiveConfig{
			Verifier:   &opsig.Verifier{AllowedSigners: app.OperatorAllowedSignersPath(), Principal: app.OperatorPrincipal()},
			InstanceID: app.Name,
			AuditPath:  st.DirectiveAudit(),
			ExpiryMax:  app.EffectiveDirectiveExpiryMax(),
		}
	}
	// The records agents verify messages against (chat ledger, sent ledger,
	// verified record) are never kept inside the tree, where an agent could
	// write them.
	cfg.Chat = broker.ChatConfig{Configured: ChatConfigured(app), LedgerPath: ChatLedgerPath(app, st), WebSenders: WebSenders(app)}
	if !StateInsideTree(app, st) {
		cfg.Chat.UsedPath = st.ChatVerified()
		cfg.Chat.SentLedgerDir = st.SentLedger()
	}
	cfg.AgentMessages = AgentMessages(app, st)
	cfg.Files = Files(app, st)
	cfg.Dispatch.BeginSession = func(agent string) func() error { return BeginSession(app, st, version, agent) }
	cfg.Version = version
	cfg.ConfigHash = ConfigHash(app)
	// Made here when apply did not make it first (a manual `lever broker
	// serve`), before any port is bound, so no broker of this instance ever
	// answers without it.
	id, err := st.EnsureInstanceID()
	if err != nil {
		return err
	}
	cfg.InstanceID = id
	return nil
}

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

func dispatchConfig(app *config.App, st state.State, be backend.Backend, env ServeEnv) (broker.DispatchConfig, error) {
	jailMount := be.MountDest()
	d := broker.DispatchConfig{
		Workers:         WorkerSpecs(app, jailMount, env.JailUID),
		InstanceProject: jailMount,
		WorkerToWorker:  app.WorkerToWorkerMessaging(),
		// The manager's bootstrap.json lives at <tree>/.lever; a worker's goes
		// to the guest through Tickets.
		AutoReenrol:         string(app.EffectiveAutoReenrol()),
		ManagerBootstrapDir: filepath.Join(app.Tree, ".lever"),
		// Confinement anchor for every staged bootstrap.json: the mount point,
		// which no agent can replace.
		Tree: app.Tree,
		// Worker workspaces may not be created through a symlink or over a
		// protected directory; shared_folders count too, or a worker workspace
		// over one would write it whatever its writers list says.
		ReadOnlyDirs: app.ProtectedDirs(),
		// scion reports the record running before the harness runs a line, and
		// every observed harness death landed within three seconds of that.
		// Same value as apply's manager gate.
		LiveSettle:        workerLiveSettle,
		MaxRunningWorkers: app.Broker.MaxRunningWorkers,
		// Closed-internet agents dial the host-alias IP; otherwise the alias
		// hostname (the cert's DNS SAN).
		BrokerURL: workerBrokerURL(cmp.Or(env.HostAliasIP, be.HostToolAlias()), app.EffectiveJailPort()),
	}
	if caPEM, err := os.ReadFile(st.CACert()); err == nil {
		d.BrokerCAPEM = string(caPEM)
	} else {
		slog.Warn("could not read the broker CA", "err", err)
	}

	// Without the jail run user from apply (a manual `broker serve`) Runtime
	// stays nil and the worker handlers return 502 (runtimeReady).
	if env.JailUser == "" || env.JailUID == "" {
		return d, nil
	}
	jr, err := registry.JailRunner(app.Backend, proc.RealRunner{}, machineName(app), env.JailUser, env.JailUID)
	if err != nil {
		return broker.DispatchConfig{}, err
	}
	// Authenticates against the dev-auth-off hub with the controller PAT
	// apply mints.
	sc := HostScionClient(patRetryRunner{r: jr, load: controllerPAT(st)}, st, app.Scion.AgentRole)
	d.Runtime = sc
	// Worker tickets go to the guest runtime dir over the jail runner, never
	// into the tree the manager mounts.
	d.Tickets = jailTicketStager{jr}
	// A recycle deletes the record and ticket like `lever worker purge`.
	d.Purge = jailWorkerPurger{sc: sc, r: jr, project: jailMount}
	d.ContainerRunning = func(ctx context.Context, worker string) (bool, error) {
		return jail.ContainerRunning(ctx, jr, jail.ContainerName(filepath.Base(jailMount), worker))
	}
	// Expired agent hub tokens are read in each agent's container and reset
	// with `scion reset-auth` under the controller PAT.
	d.HubTokens = hubTokenHealer{probe: jail.AgentProbe{R: jr}, sc: sc, project: jailMount}
	// Worker resume meets the same pre-role record hazard as the manager's
	// (broker.DispatchConfig.VerifyAgentRole).
	hc := &hubapi.Client{T: newPATRetryJailCurl(jr, scion.DefaultHubEndpoint, controllerPAT(st))}
	projectKey := filepath.Base(jailMount)
	d.VerifyAgentRole = func(ctx context.Context, agent string) error {
		return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, projectKey, agent)
	}
	// /msg/list cuts the controller's fleet-wide feed to one agent and needs
	// the hub's agent id to attribute events.
	d.ResolveAgentID = func(ctx context.Context, agentSlug string) (string, error) {
		return hc.AgentID(ctx, projectKey, scion.DefaultHubEndpoint, agentSlug)
	}
	// Every message lever sends wears the controller PAT's hub user as
	// sender; /message/verify learns that label from the hub, the way the hub
	// stamps it, rather than assuming the dev user's address.
	d.ResolveControllerSender = controllerSenderFrom(hc)
	// No worker that mounts a shared folder starts or resumes while the
	// running manager lacks the pins that keep it from swapping a folder.
	d.SharedGuard = sharedGuard(app, jr, jailMount)
	// A resume is refused when the worker's record holds a shared mount the
	// config does not grant (broker.refuseStaleShares). With a folder
	// configured a failed read refuses the resume; with none, only a record
	// holding a /shared mount is refused and a failed read is ignored.
	readVolumes := func(ctx context.Context, agent string) ([]scion.VolumeMount, error) {
		agents, err := hc.Agents(ctx, projectKey, scion.DefaultHubEndpoint)
		if err != nil {
			return nil, err
		}
		for _, a := range agents {
			if a.Slug != agent {
				continue
			}
			out := make([]scion.VolumeMount, 0, len(a.Volumes))
			for _, v := range a.Volumes {
				out = append(out, scion.VolumeMount{Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
			}
			return out, nil
		}
		return nil, fmt.Errorf("no hub record for worker %q", agent)
	}
	// The broker's changes to the manager go through the manager's one
	// lifecycle, with the same client and runner: the role guard, the tree
	// mount checks around the resume, the stop-claim recovery on the
	// manager's own container, the liveness gate and the guest boot record.
	managerDeps := apply.Deps{
		Scion:       sc,
		Log:         daemon.Warnf,
		JailMount:   jailMount,
		ManagerLock: managerLock(app, st),
		VerifyAgentRole: func(ctx context.Context, project, agent string) error {
			return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, project, agent)
		},
		AgentSession: jail.AgentProbe{R: jr},
		ContainerRunning: func(ctx context.Context, ref string) (bool, error) {
			return jail.ContainerRunning(ctx, jr, ref)
		},
		StaleContainer: jail.ContainerJanitor{R: jr},
		InspectContainerMounts: func(ctx context.Context, ref string) ([]jail.Mount, error) {
			return jail.ContainerMounts(ctx, jr, ref)
		},
		CheckLiveMounts: func(ctx context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return jail.ContainerLiveMounts(ctx, jr, ref, want)
		},
		// The record's mounts when the manager has no container, for the
		// gone-directory check.
		RecordVolumes: func(ctx context.Context, _, agent string) ([]jail.Mount, error) {
			vols, err := readVolumes(ctx, agent)
			if err != nil {
				return nil, err
			}
			out := make([]jail.Mount, 0, len(vols))
			for _, v := range vols {
				out = append(out, jail.Mount{Source: v.Source, Destination: v.Target, RW: !v.ReadOnly})
			}
			return out, nil
		},
	}
	managerDeps.GuestBoot = GuestBootFor(app, st, jr)
	d.EnsureManager = managerLifecycle(app, managerDeps)
	if len(app.SharedFolders) > 0 {
		d.RecordVolumes = readVolumes
	} else {
		d.LeftoverShareVolumes = readVolumes
	}
	return d, nil
}

// controllerSenderFrom resolves the controller PAT's hub user from auth/me as
// scion stamps a user's sends: "user:" + the email, else "user:" + the user
// id.
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
// OS-assigned ports: the jail TCP listener, the admin TCP listener, the
// directive UDS (when directives are enabled) and the operator note UDS
// (when the state directory is outside the tree). Both sockets are 0600,
// gated by filesystem permissions rather than network origin. dirLn or opLn
// is nil when its channel is off.
//
// The operator note socket stays unbound when the state directory is inside
// the tree: agents mount the tree, and where a socket in a shared mount is
// connectable from a container, the manager could post notes recorded as the
// operator's. An operator socket that cannot be bound (path too long) is
// reported and skipped. On any other failure every bound listener is closed
// so no port leaks.
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
			slog.Warn("no operator socket; lever msg send is unavailable", "err", lerr)
		} else {
			opLn = ul
		}
	}
	return jailLn, adminLn, dirLn, opLn, nil
}

// maxSocketPath is the longest UNIX socket path every host platform binds
// (macOS's sun_path holds 104 bytes with the NUL; Linux's 108).
const maxSocketPath = 103

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

type jailTicketStager struct{ r proc.Runner }

func (s jailTicketStager) StageWorkerTicket(ctx context.Context, worker string, payload []byte) error {
	return jail.StageWorkerTicket(ctx, s.r, worker, payload)
}

// hubTokenHealer reads the expiry in the agent's container and resets with
// the host scion client's `reset-auth`, which mints a token of the agent's
// own stored role.
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

// GuestBootFor is the manager's guest boot record for apply and the broker.
// Inside the tree the state directory is the agents' to replace, so lever
// neither trusts nor writes a record there: it reads the current boot only,
// and the guest-restart check falls back to podman's container start time.
func GuestBootFor(app *config.App, st state.State, jr proc.Runner) apply.GuestBoot {
	if StateInsideTree(app, st) {
		return unrecordedGuestBoot{JailGuestBoot{R: jr}}
	}
	return JailGuestBoot{R: jr, State: st}
}

// unrecordedGuestBoot is a JailGuestBoot that keeps no record.
type unrecordedGuestBoot struct{ JailGuestBoot }

func (unrecordedGuestBoot) Recorded() (string, error) { return "", nil }
func (unrecordedGuestBoot) Record(string) error       { return nil }

// managerLifecycle makes the broker's changes to the manager through
// apply.EnsureManager.
func managerLifecycle(app *config.App, d apply.Deps) func(context.Context, broker.ManagerChange) (broker.ManagerOutcome, error) {
	return func(ctx context.Context, c broker.ManagerChange) (broker.ManagerOutcome, error) {
		q := apply.ManagerRequest{Subject: "broker", What: c.What, UnderLock: c.UnderLock, StageTicket: c.StageTicket}
		switch c.Kind {
		case broker.ManagerReenrol:
			q.Intent = apply.ManagerReenrol
		case broker.ManagerTokenReset:
			q.Intent = apply.ManagerTokenReset
		default:
			return broker.ManagerOutcome{}, fmt.Errorf("internal error: unknown change %d to the manager", c.Kind)
		}
		res, err := apply.EnsureManager(ctx, app, d, q)
		if errors.Is(err, apply.ErrManagerRefused) {
			err = refusedChange{err}
		}
		return broker.ManagerOutcome{Verb: res.Verb, MountsUnverified: res.MountsUnverified}, err
	}
}

// refusedChange is an apply.ErrManagerRefused as the broker reads it.
type refusedChange struct{ error }

func (e refusedChange) Unwrap() error        { return e.error }
func (e refusedChange) Is(target error) bool { return target == broker.ErrManagerRefused }

// managerLock is the lifecycle lock as the broker takes it: without
// waiting, naming the broker to the command it turns away. With the state
// directory inside the tree there is no lock file (the agents could hold
// it), and the conversation commands, which suspend or stop the manager on
// purpose, refuse to run.
func managerLock(app *config.App, st state.State) apply.ManagerLock {
	if StateInsideTree(app, st) {
		return func(string) (func(), error) { return func() {}, nil }
	}
	return func(what string) (func(), error) {
		unlock, err := hostledger.TryLockFileAs(st.ConversationLock(), fmt.Sprintf("%s (pid %d)", what, os.Getpid()))
		if errors.Is(err, hostledger.ErrLocked) {
			return nil, fmt.Errorf("%w: %v", broker.ErrManagerBusy, err)
		}
		return unlock, err
	}
}
