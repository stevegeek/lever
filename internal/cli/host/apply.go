package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wire"
)

// scionScratchpadSharedDir is the shared directory scion stamps on every new
// project. It is mounted read-write into every agent of the project, so lever
// removes it (see Deps.StripProjectSharedDirs).
const scionScratchpadSharedDir = "scratchpad"

// hubJailTransport is curl inside the jail carrying the controller PAT, so
// callers address the same hub the in-jail scion CLI does.
func hubJailTransport(jr proc.Runner, state state.State) *hubapi.JailCurl {
	return &hubapi.JailCurl{
		Runner:  jr,
		BaseURL: scion.DefaultHubEndpoint,
		Token:   func() string { t, _ := state.LoadControllerPAT(); return t },
	}
}

// hubProjectKey is the hub's name for an instance's project: the jail mount's
// basename, the name the project is registered and looked up under.
func hubProjectKey(jailMount string) string { return filepath.Base(jailMount) }

// detachedSelfCmd re-execs this binary in its own session (Setsid, no
// controlling TTY), with stdout and stderr appended to outLog so a bind failure
// or panic stays inspectable.
//
// The log's parent is created here: on a fresh apply the state dir is only
// made by EnsureKeys inside the child, too late for this open.
//
// The child runs in configPath's directory: the broker's supervised tools
// inherit it, and config load checks a relative tool path against the
// instance root.
func detachedSelfCmd(self, outLog, configPath string, env []string, args ...string) (*exec.Cmd, *os.File, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: config path: %w", args[0], err)
	}
	if err := os.MkdirAll(filepath.Dir(outLog), 0o700); err != nil {
		return nil, nil, fmt.Errorf("%s: log dir: %w", args[0], err)
	}
	f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: out log: %w", args[0], err)
	}
	cmd := exec.Command(self, append(args, abs)...)
	cmd.Dir = filepath.Dir(abs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd, f, nil
}

func brokerServeCmd(self, configPath, outLog, aliasV4, runUser, runUID string) (*exec.Cmd, *os.File, error) {
	return detachedSelfCmd(self, outLog, configPath, []string{
		"LEVER_HOST_ALIAS_IP=" + aliasV4,
		"LEVER_JAIL_USER=" + runUser,
		"LEVER_JAIL_UID=" + runUID,
	}, "broker", "serve")
}

// remoteServeCmd adds no env: the proxy resolves its jail identity on its
// first dial (jailPrefixFn in remote.go).
func remoteServeCmd(self, configPath, outLog string) (*exec.Cmd, *os.File, error) {
	return detachedSelfCmd(self, outLog, configPath, nil, "remote", "serve")
}

// logFunc is the sink for apply's user-facing lines. A nil logFunc prints to
// stderr.
type logFunc func(format string, args ...any)

func (f logFunc) printf(format string, args ...any) {
	if f == nil {
		logLine(os.Stderr, format, args...)
		return
	}
	f(format, args...)
}

// logLine sanitizes every line (termsafe.Sanitize): the arguments wrap jail
// and hub errors such as scion's stderr. Lines are serialized because the
// manager start logs its progress from another goroutine.
func logLine(w io.Writer, format string, args ...any) {
	line := termsafe.Sanitize(fmt.Sprintf(format, args...))
	logLineMu.Lock()
	defer logLineMu.Unlock()
	fmt.Fprintln(w, line)
}

var logLineMu sync.Mutex

// remoteController owns the apply-time remote-proxy lifecycle. The spawn is
// fire-and-forget, but Start waits until the proxy has bound its listener, so
// a startup failure (a non-orbstack backend, a port already taken) fails the
// apply with the last line of state.RemoteLog() quoted.
type remoteController struct {
	state      state.State
	configPath string
	selfExe    string // the binary re-exec'd as `remote serve`; see applyOpts.SelfExe
	port       int    // app.EffectiveRemotePort()
	// probeHost is the host of app.RemoteProbeAddr(); "" means 127.0.0.1.
	probeHost string
	version   string // this binary, for the reuse stamp
	// cfgHash is brokerctl.RemoteConfigHash, for the reuse stamp. It is read at
	// Start: hub-identity rewrites the record it covers earlier in the run.
	cfgHash func() string
	// startTimeout/startInterval bound the waits for bind and pid file; zero
	// means remoteProxyStartTimeout/Interval.
	startTimeout  time.Duration
	startInterval time.Duration
	log           logFunc // apply's user-facing line sink (applyWiring.log)
	// stateInTree reports whether the state directory, which holds the stamp
	// and remote.pid, is inside the tree. nil reads as inside.
	stateInTree func() bool
}

func newRemoteController(app *config.App, st state.State, configPath, selfExe string, log logFunc) *remoteController {
	return &remoteController{
		state:       st,
		configPath:  configPath,
		selfExe:     selfExe,
		port:        app.EffectiveRemotePort(),
		probeHost:   remoteProbeHost(app),
		version:     cli.VersionString(),
		cfgHash:     func() string { return brokerctl.RemoteConfigHash(app, st) },
		log:         log,
		stateInTree: func() bool { return brokerctl.StateInsideTree(app, st) },
	}
}

func (rc *remoteController) inTree() bool { return rc.stateInTree == nil || rc.stateInTree() }

func (rc *remoteController) addr() string {
	return net.JoinHostPort(cmp.Or(rc.probeHost, "127.0.0.1"), strconv.Itoa(rc.port))
}

// Start spawns `lever remote serve <config>` as a daemonized child.
//
// A proxy that is alive, listening on the configured port, this instance's
// `lever remote serve` (state.State.RemoteStatus), and stamped with this
// binary and remote config is reused; a duplicate would fail to bind and
// clobber remote.pid with a dead pid.
//
// The stamp check is required: the proxy caches its whole config at startup
// (ServeHost, the allowed-user set, the ports), so without it a security
// change such as enabling allowed_users would silently not take effect. The
// proxy must not grow an /epoch-style endpoint to compare against, since any
// listener of its own is reachable by whatever reaches the proxy; hence a
// host-side stamp file.
func (rc *remoteController) Start(ctx context.Context) error {
	hash := rc.cfgHash()
	if _, found, alive := rc.state.RemoteStatus(); found && alive {
		// With the state directory inside the tree the stamp is the agents' to
		// forge, so it vouches for nothing and the proxy is always replaced.
		if !rc.inTree() && rc.state.RemoteStampMatches(rc.version, hash) && tcpDial(rc.addr()) == nil {
			progress.From(ctx).Note("reused")
			return nil // already serving: this binary, this config, this port
		}
		// A live proxy that is not the one this config asks for is stopped first, or
		// the replacement cannot bind. This must stay outside the tcpDial check: a
		// changed remote.port makes the dial fail, and skipping the stop then would
		// leak the old proxy on the old port with remote.pid overwritten.
		rc.log.printf("lever: the remote proxy predates this binary or its remote config changed — restarting it")
		if err := brokerctl.StopRemoteProxy(rc.state); err != nil {
			return fmt.Errorf("restarting the remote proxy: %w", err)
		}
	}
	cmd, logf, err := remoteServeCmd(rc.selfExe, rc.configPath, rc.state.RemoteLog())
	if err != nil {
		return err
	}
	defer logf.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lever remote serve: %w", err)
	}
	if err := rc.awaitListening(ctx, cmd); err != nil {
		return err
	}
	// A proxy with no matching stamp costs the next bring-up a restart, never
	// a stale process kept alive, so only a warning. A child that never wrote
	// remote.pid never bound the port: what answered the dial is another
	// process, which may enforce another config. With the state directory
	// inside the tree remote.pid is the agents' to write, so it decides
	// nothing there.
	if err := rc.awaitStamp(ctx, cmd.Process.Pid, hash); err != nil {
		if errors.Is(err, errRemoteProxyNotRecorded) && !rc.inTree() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return fmt.Errorf("lever remote serve: %w; another process may hold %s (see `lever doctor`)", err, rc.addr())
		}
		rc.log.printf("lever: the remote proxy runs, but it did not stamp this config (%v) — the next bring-up restarts it", err)
	}
	return nil
}

var errRemoteProxyNotRecorded = errors.New("the new proxy never recorded itself in remote.pid, so it is not the one serving the port")

// awaitStamp waits until remote.pid names pid and the proxy has stamped the
// config apply converged. The proxy is the stamp's only writer: it records
// the config it loaded, which an edit made since this apply hashed it would
// make another.
func (rc *remoteController) awaitStamp(ctx context.Context, pid int, hash string) error {
	timeout := cmp.Or(rc.startTimeout, remoteProxyStartTimeout)
	interval := cmp.Or(rc.startInterval, remoteProxyStartInterval)
	var last error
	recorded := false
	err := retry.Until(ctx, int(timeout/interval)+1, interval, func() (bool, error) {
		got, err := state.ReadPID(rc.state.RemotePID())
		switch {
		case err != nil:
			last = err
		case got != pid:
			last = fmt.Errorf("%s names pid %d, not the new proxy %d", rc.state.RemotePID(), got, pid)
		case !rc.state.RemoteStampMatches(rc.version, hash):
			recorded = true
			last = errors.New("the proxy has not stamped the remote config this apply converged")
		default:
			return true, nil
		}
		return false, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		if !recorded {
			return fmt.Errorf("after %s: %w: %w", timeout, errRemoteProxyNotRecorded, last)
		}
		return fmt.Errorf("after %s: %w", timeout, last)
	}
	return err
}

// remoteProxyStartTimeout/Interval are the default bounds on the waits for the
// spawned proxy to bind and to write its pid file. Both are host loopback
// operations that happen before the proxy touches the jail.
const (
	remoteProxyStartTimeout  = 5 * time.Second
	remoteProxyStartInterval = 50 * time.Millisecond
)

var errChildGone = errors.New("child exited")

// awaitListening fails the apply when the spawned proxy does not bind (a port
// already taken), quoting the log's tail: the child owns that file and this
// process never sees its stderr. A listener alone is not enough: child must
// still be alive, since another process (a leaked older proxy) may hold the
// port, and stamping then would record a config nothing enforces.
func (rc *remoteController) awaitListening(ctx context.Context, child *exec.Cmd) error {
	timeout := cmp.Or(rc.startTimeout, remoteProxyStartTimeout)
	interval := cmp.Or(rc.startInterval, remoteProxyStartInterval)
	attempts := int(timeout/interval) + 1
	err := retry.Until(ctx, attempts, interval, func() (bool, error) {
		if err := child.Process.Signal(syscall.Signal(0)); err != nil {
			return false, errChildGone
		}
		return tcpDial(rc.addr()) == nil, nil
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, errChildGone) && !errors.Is(err, retry.ErrExhausted) {
		return err // the apply's own ctx ended
	}
	if tail := lastLogLine(rc.state.RemoteLog()); tail != "" {
		return fmt.Errorf("%w on %s: %s (see %s)",
			errRemoteProxyNotListening, rc.addr(), tail, rc.state.RemoteLog())
	}
	return fmt.Errorf("%w on %s — see %s",
		errRemoteProxyNotListening, rc.addr(), rc.state.RemoteLog())
}

func lastLogLine(path string) string { return lastFileLine(path, 4<<10) }

// removeJailFileScript removes a file or any symlink at $1 (a directory there
// is a stray in-repo git-mode project, not a stale marker) and is a no-op when
// $1 is absent.
//
// A dangling symlink must go too: -e is false for one, and scion init writes
// the marker with a plain open that follows it, so an agent's link in the tree
// would make the jail user create a file wherever the link points.
const removeJailFileScript = `if [ -L "$1" ] || { [ ! -d "$1" ] && [ -e "$1" ]; }; then rm -f -- "$1"; fi`

func removeJailFile(ctx context.Context, jr proc.Runner, jailPath string) error {
	if _, err := jr.Run(ctx, nil, "sh", "-c", removeJailFileScript, "_", jailPath); err != nil {
		return fmt.Errorf("removing jail file %s: %w", jailPath, err)
	}
	return nil
}

// controllerPATScopes is the exact scope set the controller PAT is minted
// with. agent:attach gates scion's start/stop/suspend/restart for a user
// caller; agent:message gates message sends on newer scions (attach does not
// expand to it); project:update is needed for the shared-dir strip and the
// agent-role ceiling on the project settings route (agent:manage does not
// expand to it). Changing this set makes the next apply re-mint.
//
// The skill, template and harness_config reads let the token create a
// baseline agent: a newer hub caps a token-created agent's role at what the
// token's scopes cover, and baseline's project:read covers those reads. They
// are read-only and every supported scion knows them.
//
// lifecycle adds agent:lifecycle, which a hub that has it requires for start,
// stop, suspend and resume of an existing agent by a user token; a refused
// resume answers 409 "already exists". inbox adds inbox:read, which a hub that
// has it requires to list notifications (the broker inbox and the manager
// watch). An older hub rejects an unknown scope, so each is added only when
// the jail's scion knows it.
func controllerPATScopes(lifecycle, inbox bool) []string {
	s := []string{"agent:manage", "agent:attach", "agent:message", "project:read", "project:update",
		"skill:read", "skill:list", "template:read", "template:list", "harness_config:read", "harness_config:list"}
	if lifecycle {
		s = append(s, lifecycleScope)
	}
	if inbox {
		s = append(s, inboxReadScope)
	}
	return s
}

// inboxReadScope is the UAT scope that lists the caller's notifications.
const inboxReadScope = "inbox:read"

// controllerInbox decides whether the controller PAT carries inbox:read, as
// controllerLifecycle does for agent:lifecycle: what the jail's scion says or,
// when it cannot be asked, what the token on disk was minted with. No alias
// expands to it.
func controllerInbox(ctx context.Context, known scopeKnownFunc, rec state.PATRecord) bool {
	if known != nil {
		if has, err := known(ctx, inboxReadScope); err == nil {
			return has
		}
	}
	return slices.Contains(rec.Requested, inboxReadScope)
}

// controllerLifecycle decides whether the controller PAT carries
// agent:lifecycle: what the jail's scion says or, when it cannot be asked,
// what the token on disk was minted with, so an unanswered probe never forces
// a re-mint. A token whose Granted set already holds the scope (agent:manage
// expands to it at mint) keeps its requested set.
func controllerLifecycle(ctx context.Context, known scopeKnownFunc, rec state.PATRecord) bool {
	if slices.Contains(rec.Granted, lifecycleScope) && !slices.Contains(rec.Requested, lifecycleScope) {
		return false
	}
	if known != nil {
		if has, err := known(ctx, lifecycleScope); err == nil {
			return has
		}
	}
	return slices.Contains(rec.Requested, lifecycleScope)
}

// remotePATScopes is the exact scope set of the remote proxy's PAT: the proxy
// only lists the project's agents with it (the chat page and the contact
// fence), so it reads and nothing else. It is the controller's token, so
// anything sent with it would wear lever's sender label.
func remotePATScopes() []string {
	return []string{"agent:read", "agent:list", "project:read"}
}

// patRenewWindow is how long before expiry apply re-mints.
const patRenewWindow = 30 * 24 * time.Hour

const patReasonAbsent = "no token on disk"

// patMintReason says why a token must be (re)minted, or "" when the stored one
// is still wanted. A token without a record is re-minted: it may lack a scope
// and carries scion's 90-day default expiry. An unknown expiry on a record is
// not a reason, or a scion that prints none would force a mint on every apply.
func patMintReason(tok string, rec state.PATRecord, found bool, want []string, now time.Time) string {
	if tok == "" {
		return patReasonAbsent
	}
	if !found {
		return "the token has no record of its scopes or expiry (minted by an older lever)"
	}
	if !sameScopeSet(rec.Requested, want) {
		return fmt.Sprintf("the token was minted with scopes %s; this lever needs %s",
			strings.Join(rec.Requested, ","), strings.Join(want, ","))
	}
	if !rec.ExpiresAt.IsZero() {
		if !rec.ExpiresAt.After(now) {
			return fmt.Sprintf("the token expired on %s", rec.ExpiresAt.Format(time.RFC3339))
		}
		if rec.ExpiresAt.Before(now.Add(patRenewWindow)) {
			return fmt.Sprintf("the token expires on %s (within %s)", rec.ExpiresAt.Format(time.RFC3339), patRenewWindow)
		}
	}
	return ""
}

// patUrgent reports whether a token must be minted before apply can go on:
// every mint reason except the renew window, where the token still works.
func patUrgent(tok string, rec state.PATRecord, found bool, want []string, now time.Time) bool {
	if patMintReason(tok, rec, found, want, now) == "" {
		return false
	}
	renewOnly := tok != "" && found && sameScopeSet(rec.Requested, want) &&
		(rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(now))
	return !renewOnly
}

func sameScopeSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func expiryText(t time.Time) string {
	if t.IsZero() {
		return "its expiry (unknown)"
	}
	return t.Format(time.RFC3339)
}

// resetHubIdentityUsage: a hub whose database was reset loses lever's role,
// users and tokens; lever re-bootstraps on its own only with positive evidence
// (hubIdentity.markerGone), and this flag is the operator's explicit say-so.
const resetHubIdentityUsage = "forget lever's recorded hub identities and tokens and bootstrap the hub as new (after a deliberate hub database reset)"

// applySignalContext returns a context that SIGINT or SIGTERM cancels, so the
// step in flight fails and its cleanup runs. Without it a signal kills lever
// at once, mid-step. The first signal restores the default action, so a second
// Ctrl-C still kills lever. stop must be called.
func applySignalContext(parent context.Context) (ctx context.Context, stop func()) {
	ctx, stop = signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func newApplyCmd(bf BackendFactory) *cobra.Command {
	var dryRun, resetHubIdentity bool
	var progressMode string
	var lockWait time.Duration
	c := &cobra.Command{
		Use:   "apply [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an agent-manager application up from a config",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			if dryRun {
				if p, ok := registry.ProfileFor(app.Backend); ok {
					cmd.Printf("backend: %s\n", p.Summary())
				}
				printRemoteWarnings(cmd, app)
				if err := checkContactGate(app, stateFor(path)); err != nil {
					return err
				}
				printContactSessionWarnings(cmd, app, stateFor(path))
				for _, s := range apply.Plan(app, apply.PlanOpts{}) {
					if s.TarPath != "" {
						cmd.Printf("  %-16s %s (from %s)\n", s.Kind, s.Target, s.TarPath)
						continue
					}
					cmd.Printf("  %-16s %s\n", s.Kind, s.Target)
				}
				return nil
			}
			unlock, err := lockLifecycle(cmd.Context(), app, stateFor(path), "lever apply", lockWait, lockNotice(cmd))
			if err != nil {
				return err
			}
			defer unlock()
			run, pctx, err := startBringup(cmd, app, stateFor(path), "apply", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			printRemoteWarnings(cmd, app)
			if err := fenceNow(app, stateFor(path)); err != nil {
				return err
			}
			if err := checkContactGate(app, stateFor(path)); err != nil {
				return err
			}
			printContactSessionWarnings(cmd, app, stateFor(path))
			ctx, stop := applySignalContext(pctx)
			defer stop()
			run.rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: run.rep, ResetHubIdentity: resetHubIdentity, Locked: true})
			if err != nil {
				return err
			}
			if err := apply.Run(ctx, app, w.deps, apply.PlanOpts{}); err != nil {
				return err
			}
			recordConverge(app, stateFor(path), "apply", w.log)
			run.finish(nil)
			cmd.Printf("application %q is up.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the bring-up plan and exit")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	c.Flags().BoolVar(&resetHubIdentity, "reset-hub-identity", false, resetHubIdentityUsage)
	addLockWaitFlag(c, &lockWait, 0)
	return c
}

// brokerReusable reports whether a running broker's /epoch identity matches
// this binary and broker config. A broker that reports empty identity fields
// mismatches, so it is restarted rather than trusted.
func brokerReusable(got wire.EpochResponse, wantVersion, wantHash string) bool {
	return got.Version == wantVersion && got.ConfigHash == wantHash
}

// brokerController owns the apply-time broker lifecycle: spawning `lever
// broker serve`, polling it healthy, and minting the manager's one-time
// bootstrap ticket. Its methods back the like-named Deps funcs, and Rearm
// combines them for the re-arm-on-create path. Built after EnsureUp and
// HostAliasV4 have resolved the run user and host alias.
type brokerController struct {
	app        *config.App // EffectiveJailPort, ManagerCN, ConfigHash, Tree (Rearm)
	state      state.State // pid file, out log, CA cert, StopBroker
	configPath string      // passed to `lever broker serve`
	adminURL   string      // http://127.0.0.1:<admin port>
	aliasV4    string      // resolved host-alias IP (LEVER_HOST_ALIAS_IP for the child)
	brokerHost string      // host agents dial the broker by (IP if resolved, else hostname)
	runUser    string      // in-machine run user for the broker child
	runUID     string      // in-machine run uid for the broker child
	selfExe    string      // the binary re-exec'd as `broker serve`; see applyOpts.SelfExe
	log        logFunc     // apply's user-facing line sink (applyWiring.log)
	// admin's timeout bounds every /epoch and /bootstrap call, so a wedged broker
	// cannot hang an apply step whose ctx has no deadline.
	admin *http.Client
}

const brokerAdminTimeout = 5 * time.Second

// Start spawns `lever broker serve <config>` as a daemonized child.
//
// A broker already serving is reused only when it is this instance's and
// matches this binary and broker config; otherwise it is stopped and a new one
// spawned, since a duplicate would fail to bind and clobber broker.pid with a
// dead pid. Another instance's broker on the same ports is never stopped or
// used: apply fails and names the ports to change (judgeRunningBroker).
func (bc *brokerController) Start(ctx context.Context) error {
	ours, err := bc.state.EnsureInstanceID()
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// Anything but a decodable epoch 200 means no lever broker is serving. Something
	// else holding a port makes the child's bind fail, which Healthy reports.
	if er, err := readBrokerIdentity(probeCtx, bc.admin, bc.adminURL); err == nil {
		// alive: broker.pid names a live `lever broker serve` of this instance, not a
		// pid the OS reused.
		_, _, alive := bc.state.BrokerStatus()
		switch judgeRunningBroker(er, ours, cli.VersionString(), brokerctl.ConfigHash(bc.app), alive) {
		case brokerReuse:
			progress.From(ctx).Note("reused")
			return nil // same instance, binary and broker config; keep the process + PID
		case brokerRefuse:
			if er.InstanceID == "" {
				return fmt.Errorf("%w (this instance's %s names no live process)", foreignBrokerError(bc.app.EffectiveAdminPort(), er, ours), stateRel(bc.state, bc.state.PID()))
			}
			return foreignBrokerError(bc.app.EffectiveAdminPort(), er, ours)
		}
		bc.log.printf("lever: broker predates this binary or its tool config changed — restarting it (was %q)", er.Version)
		if err := brokerctl.StopBroker(bc.state); err != nil {
			return fmt.Errorf("stopping the stale broker before restart: %w", err)
		}
		// StopBroker stops only the process broker.pid names. A broker still answering
		// once the port had time to fall silent was not that process; the health check
		// must not adopt it, so refuse.
		still, ok := bc.stillAnswering(ctx, brokerQuietWait)
		if err := ctx.Err(); err != nil {
			return err // a cancelled probe proves nothing: spawn nothing
		}
		if ok {
			if still.InstanceID == ours {
				return fmt.Errorf("this instance's broker still answers on 127.0.0.1:%d after the stop: %s names another process or none; stop the broker by hand (find it with lsof -nP -iTCP:%d -sTCP:LISTEN), then run this again",
					bc.app.EffectiveAdminPort(), stateRel(bc.state, bc.state.PID()), bc.app.EffectiveAdminPort())
			}
			return fmt.Errorf("a broker still answers on 127.0.0.1:%d after this instance's broker was stopped: %w",
				bc.app.EffectiveAdminPort(), foreignBrokerError(bc.app.EffectiveAdminPort(), still, ours))
		}
	}
	cmd, logf, err := brokerServeCmd(bc.selfExe, bc.configPath, bc.state.OutLog(), bc.aliasV4, bc.runUser, bc.runUID)
	if err != nil {
		return err
	}
	defer logf.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lever broker serve: %w", err)
	}
	return nil
}

// brokerHealthTimeout bounds Healthy's poll on the wall clock, since each probe
// can itself take up to brokerAdminTimeout.
const (
	brokerHealthTimeout  = 10 * time.Second
	brokerHealthInterval = 200 * time.Millisecond
)

// Healthy polls GET /epoch until this instance's broker, with this binary's
// version and this config's hash, answers or brokerHealthTimeout elapses. Any
// other broker answering fails at once rather than being adopted: minting from
// another instance's broker would hand this manager the wrong broker.
func (bc *brokerController) Healthy(ctx context.Context) error {
	ours, err := bc.state.ReadInstanceID()
	if err != nil {
		return fmt.Errorf("broker health: %w", err)
	}
	pollCtx, cancel := context.WithTimeout(ctx, brokerHealthTimeout)
	defer cancel()
	wantV, wantH := cli.VersionString(), brokerctl.ConfigHash(bc.app)
	var last error
	err = retry.Until(pollCtx, 0, brokerHealthInterval, func() (bool, error) {
		got, gerr := readBrokerIdentity(pollCtx, bc.admin, bc.adminURL)
		if gerr != nil {
			last = gerr
			return false, nil
		}
		if err := heldBrokerErr(got, ours, wantV, wantH, bc.app.EffectiveAdminPort()); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil && ctx.Err() == nil && pollCtx.Err() != nil {
		return fmt.Errorf("broker did not become healthy within %s (last err: %v); if another process holds 127.0.0.1:%d or 127.0.0.1:%d, the broker could not bind it: see %s",
			brokerHealthTimeout, last, bc.app.EffectiveJailPort(), bc.app.EffectiveAdminPort(), stateRel(bc.state, bc.state.OutLog()))
	}
	return err
}

// Mint obtains the one-time manager enrolment ticket from /bootstrap plus the
// CA PEM. A 403 means the single-use latch was already consumed and is returned
// as apply.ErrBootstrapLatched, which a re-apply tolerates.
func (bc *brokerController) Mint(ctx context.Context) (apply.BootstrapMaterial, error) {
	var result wire.BootstrapResponse
	switch err := httpjson.Post(ctx, bc.admin, bc.adminURL+wire.PathBootstrap, nil, &result); {
	case err == nil:
	case httpjson.Status(err) == http.StatusForbidden:
		return apply.BootstrapMaterial{}, apply.ErrBootstrapLatched
	default:
		return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: %w", err)
	}
	caPEM, err := os.ReadFile(bc.state.CACert())
	if err != nil {
		return apply.BootstrapMaterial{}, fmt.Errorf("reading broker CA cert: %w", err)
	}
	return apply.BootstrapMaterial{
		Ticket:    result.Ticket,
		BrokerCA:  string(caPEM),
		BrokerURL: fmt.Sprintf("https://%s:%d", bc.brokerHost, bc.app.EffectiveJailPort()),
		AgentCN:   bc.app.ManagerCN(),
		Claude:    bc.app.ManagerClaude(),
	}, nil
}

// Rearm backs Deps.RearmBootstrap: a freshly created scion agent has no home
// to reuse and always re-enrols, which a spent latch would refuse. It restarts
// the broker to re-arm the single-use latch (the CA and signing keys are on
// disk, so existing certs and tokens keep working), waits for health, mints,
// and stages the result into app.Tree/.lever/bootstrap.json. Staging happens
// here because only this controller has app.Tree in scope.
func (bc *brokerController) Rearm(ctx context.Context) error {
	if err := brokerctl.StopBroker(bc.state); err != nil {
		return fmt.Errorf("stopping the broker to re-arm its bootstrap latch: %w", err)
	}
	if err := bc.Start(ctx); err != nil {
		return fmt.Errorf("restarting the broker to re-arm its bootstrap latch: %w", err)
	}
	if err := bc.Healthy(ctx); err != nil {
		return fmt.Errorf("waiting for the re-armed broker to become healthy: %w", err)
	}
	m, err := bc.Mint(ctx)
	if err != nil {
		return fmt.Errorf("minting bootstrap material from the re-armed broker: %w", err)
	}
	if err := apply.StageBootstrapMaterial(bc.app.Tree, m); err != nil {
		return fmt.Errorf("staging re-armed bootstrap material: %w", err)
	}
	return nil
}

// leverMayClaimTemplate reports whether lever may point default_template at
// its overlay. Only scion's stock "default" is lever's to take, and an empty
// value (unset, so scion falls back to "default"). Anything else is already
// lever's or the operator's deliberate choice, which lever must not override.
func leverMayClaimTemplate(current string) bool {
	switch strings.TrimSpace(current) {
	case "default", "":
		return true
	default:
		return false
	}
}

// applyWiring is what buildApplyDeps resolved for one apply. Its methods are
// the Deps closures that need more than a one-line forward.
type applyWiring struct {
	deps  apply.Deps
	b     backend.Backend
	sc    *scion.Client
	app   *config.App
	jr    proc.Runner
	state state.State
	// hubKey signs every assertion; instanceID names the hub in iss and aud;
	// clock is the guest's, read once per run.
	hubKey     *hubauth.Key
	instanceID string
	clock      hubauth.Clock
	// runStart is clock.Now() when the run began.
	runStart         time.Time
	signer           *hubauth.Signer
	resetHubIdentity bool
	// ssh is this apply's `lever ssh` decision.
	ssh sshGate
	// brokerHost is what agents dial the host by:
	// the host-alias IP when the backend has one. Under closed-internet egress DNS
	// is dropped; the IP is allowlisted and is a SAN on the broker cert.
	brokerHost string
	cmd        *cobra.Command // for Log; nil ⇒ stderr
	// rep, when set, takes the Log lines so they print above its live line.
	rep *progress.Reporter
}

type applyOpts struct {
	// Cmd wires Deps.Log; nil means stderr.
	Cmd *cobra.Command
	// Rep is the bring-up's progress list; nil prints Log lines directly.
	Rep *progress.Reporter
	// ResetHubIdentity forgets the recorded hub identities and bootstraps the
	// hub as new (`--reset-hub-identity`).
	ResetHubIdentity bool
	// Locked: the command holds the lifecycle lock for its whole run, so
	// Deps.ManagerLock is apply.HeldManagerLock. Otherwise it is nil and
	// apply.EnsureManager refuses.
	Locked bool
	// SelfExe is the executable re-exec'd as `broker serve` / `remote serve`;
	// empty means os.Args[0]. Tests must set it: under `go test` os.Args[0] is the
	// test binary, and the Setsid child outlives the run with nothing to reap it.
	SelfExe string
}

// buildApplyDeps wires the live dependencies for apply.Run. It calls EnsureUp
// first so the run user and uid are known before the jail runner and scion
// client are built; that is why the plan has no jail-up step.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory, opts applyOpts) (*applyWiring, error) {
	b, err := bringUpBackend(ctx, app, bf, stateFor(configPath), (&applyWiring{cmd: opts.Cmd, rep: opts.Rep}).log)
	if err != nil {
		return nil, err
	}
	jr := b.JailRunner()

	// state must exist before sc: sc's HubTokenSource reads the controller PAT at
	// call time, so verbs pick it up once hub-identity mints it mid-apply.
	st := stateFor(configPath)
	sc := brokerctl.HostScionClient(jr, st, app.Scion.AgentRole)

	// The hub's session signing key is generated once and never rotated by lever
	// (see EnsureSessionSecret).
	sessionSecret, err := st.EnsureSessionSecret()
	if err != nil {
		return nil, err
	}

	w := &applyWiring{b: b, sc: sc, app: app, jr: jr, state: st, cmd: opts.Cmd, rep: opts.Rep, resetHubIdentity: opts.ResetHubIdentity}
	if err := w.initHubAuth(ctx); err != nil {
		return nil, err
	}
	w.ssh = newSSHGate(ctx, app, jailConduit{jr}, jr)
	if w.ssh.why != "" {
		w.log("lever: warning: %s", w.ssh.why)
	}
	aliasV4 := b.HostAliasV4()
	w.brokerHost = cmp.Or(aliasV4, b.HostToolAlias())
	selfExe := cmp.Or(opts.SelfExe, os.Args[0])

	bc := &brokerController{
		app:        app,
		state:      st,
		configPath: configPath,
		adminURL:   fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort()),
		aliasV4:    aliasV4,
		brokerHost: w.brokerHost,
		runUser:    b.RunUser(),
		runUID:     b.RunUID(),
		selfExe:    selfExe,
		log:        w.log,
		admin:      &http.Client{Timeout: brokerAdminTimeout},
	}

	rc := newRemoteController(app, st, configPath, selfExe, w.log)

	w.deps = w.newDeps(bc, rc, sessionSecret)
	if opts.Locked {
		w.deps.ManagerLock = apply.HeldManagerLock
	}
	return w, nil
}

// initHubAuth loads (or creates) the host's assertion key and reads the guest
// clock once: assertions are signed relative to the clock the hub checks them
// against.
func (w *applyWiring) initHubAuth(ctx context.Context) error {
	k, created, err := hubauth.EnsureKey(w.state.HubAuthKey())
	if err != nil {
		return fmt.Errorf("hub auth key: %w", err)
	}
	if created {
		w.log("lever: created the hub assertion key %s (%s)", w.state.HubAuthKey(), k.Fingerprint())
	}
	id, err := w.state.EnsureInstanceID()
	if err != nil {
		return err
	}
	w.hubKey, w.instanceID = k, id
	w.clock = readGuestClock(ctx, w.jr, w.log)
	w.signer = hubauth.NewSigner(k, id, w.clock)
	w.runStart = w.clock.Now()
	return nil
}

// smallGuestOutputLimit caps a jail command whose real output is a line or
// a small file (a clock read, a marker file, a probe's exit status).
const smallGuestOutputLimit = 64 << 10

// readGuestClock reads `date +%s` in the jail. A failed read signs on the
// host clock: the jail is then unreachable for the assertions too, and doctor
// reports a large skew.
func readGuestClock(ctx context.Context, jr proc.Runner, log func(string, ...any)) hubauth.Clock {
	at := time.Now()
	res, err := jr.Run(proc.WithOutputLimit(ctx, smallGuestOutputLimit), nil, "date", "+%s")
	if err == nil {
		if epoch, perr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64); perr == nil {
			return hubauth.ClockFromGuestEpoch(epoch, at)
		}
	}
	log("lever: warning: could not read the jail's clock (%v); signing hub assertions on the host clock", err)
	return hubauth.Clock{}
}

// bringUpBackend brings the jail up so the run user and host alias are known
// before anything is built over the jail runner.
func bringUpBackend(ctx context.Context, app *config.App, bf BackendFactory, st state.State, warn logFunc) (backend.Backend, error) {
	machine := machineName(app.Name)
	b, err := bf(app.Backend, machine)
	if err != nil {
		return nil, err
	}
	cfg := backendConfigFor(app, machine)
	cfg.Warn = warn
	if st.Dir != "" {
		cfg.HWEFailStamp = st.HWEFailStamp()
		// An agent could read, forge or delete them there.
		if !brokerctl.StateInsideTree(app, st) {
			cfg.GuestLogDir = st.GuestLogs()
		} else {
			warn("jail: warning: the state directory (%s) is inside the tree the agents mount, so lever saves no copy of the VM's console logs", state.DirName)
		}
	}
	// The host's scion build runs while the VM boots; the scion step takes it once
	// the guest confirms the architecture.
	cfg.ScionPrebuild = startScionPrebuild(ctx, b, cfg)
	defer cfg.ScionPrebuild.Stop()
	if err := b.EnsureUp(ctx, cfg); err != nil {
		return nil, err
	}
	return b, nil
}

// startScionPrebuild returns nil when the backend has no host runner or there
// is nothing to build.
func startScionPrebuild(ctx context.Context, b backend.Backend, cfg backend.Config) *scionbin.Prebuild {
	hr, ok := b.(interface{ Runner() proc.Runner })
	if !ok || !cfg.HasScion() {
		return nil
	}
	return scionbin.StartPrebuild(ctx, hr.Runner(), scionbin.Spec{
		Binary:  cfg.ScionBinary,
		Source:  cfg.ScionSource,
		Version: cfg.ScionVersion,
	}, runtime.GOARCH, cfg.MachineName)
}

func backendConfigFor(app *config.App, machine string) backend.Config {
	return backend.Config{
		MachineName:    machine,
		ProjectTree:    app.Tree,
		AllowedPorts:   app.EffectiveAllowedPorts(),
		ScionSource:    app.Scion.Source,
		ScionVersion:   app.Scion.Version,
		ScionBinary:    app.Scion.Binary,
		ScionWebUI:     app.ScionWebAssets(),
		ClosedInternet: app.ClosedInternetEgress(),
		Disk:           app.Disk,
		CPUs:           app.CPUs,
		Memory:         app.Memory,
		NestedVirt:     app.NestedVirt,
		HostSwap:       app.HostSwap,
		GuestKernel:    app.EffectiveGuestKernel(),
		GuestProfile:   app.EffectiveGuestProfile(),
	}
}

func (w *applyWiring) newDeps(bc *brokerController, rc *remoteController, sessionSecret string) apply.Deps {
	b, sc, st := w.b, w.sc, w.state
	return apply.Deps{
		LoadImage: b.LoadImage,
		// ImageLoaded skips a re-import when the jail holds the same image ID.
		// Fail-open in the backend, so a check failure just loads.
		ImageLoaded:    b.ImageLoaded,
		LoadImageTar:   b.LoadImageTar,
		ImageLoadedTar: b.ImageLoadedTar,
		// Every tag in an image_tar archive is held to the registry allowlist, not
		// only the configured ref.
		ImageTagPolicy: w.app.Security.ImageTagPolicy(),
		// Best-effort: the step logs and never fails on error.
		PruneImages: b.PruneJailImages,
		// WarmImage builds podman's id-mapped layer copy before scion's agent start
		// needs it. Best-effort.
		WarmImage:        b.WarmJailImage,
		Fingerprints:     stepFingerprints(w.app, st),
		Scion:            sc,
		JailMount:        b.MountDest(),
		HubSessionSecret: sessionSecret,

		// RemoveJailFile goes through the jail runner so the removal shares the
		// jail's filesystem view with the `scion init` that follows (a VirtioFS
		// unlink/init race; see the register-project case in internal/apply/run.go).
		RemoveJailFile: w.removeJailFile,

		EnsureHubIdentity: w.ensureHubIdentity,
		EnsureHubAuth:     w.ensureHubAuth,
		BeforeHubRestart:  w.pinExperimentsBeforeRestart,
		HubAuthEnv:        apply.HubEnv(w.instanceID, w.hubKey.Fingerprint(), w.app, w.ssh.controls),
		HubConduitEnv: func(ctx context.Context) (map[string]string, error) {
			live, err := conduit.ReadLiveHub(ctx, w.jr)
			return live.Env, err
		},
		ProjectLinked:         w.projectLinked,
		EnsureProjectProvider: w.ensureProjectProvider,

		// RemoveScionProjectConfigs clears stale ~/.scion/project-configs entries
		// before re-init, so apply does not add a duplicate registration every run.
		RemoveScionProjectConfigs: b.RemoveScionProjectConfigs,

		// ScionProjectRegistered gates the destructive clean+init, so a re-apply does
		// not orphan a resumable scion agent record.
		ScionProjectRegistered: b.ScionProjectRegistered,

		StripProjectSharedDirs: w.stripProjectSharedDirs,
		EnsureAgentRoleCeiling: w.ensureAgentRoleCeiling,
		RepairScionHubEndpoint: w.repairScionHubEndpoint,
		VerifyAgentRole:        w.verifyAgentRole,

		StartBroker:          bc.Start,
		BrokerHealthy:        bc.Healthy,
		MintManagerBootstrap: bc.Mint,

		// The runtime broker registers asynchronously after the Hub API, so a first
		// create/resume would race it. Fail-soft: it never fails bring-up.
		WaitBrokerReady: w.waitBrokerReady,
		BrokerOnline:    w.brokerOnline,

		RearmBootstrap: bc.Rearm,

		// StopRemoteProxy needs no controller: teardown is by pid file.
		StartRemoteProxy: rc.Start,
		StopRemoteProxy:  func(context.Context) error { return brokerctl.StopRemoteProxy(st) },
		FenceRemoteLogins: func(context.Context) error {
			return fenceRemoteLogins(w.app, st, func() error { return brokerctl.StopRemoteProxy(st) })
		},
		// BeginSession records the manager's fresh starts for the contact gate (see
		// sessionrec and remoteContactSession).
		BeginSession: func(agent string) func() error {
			return brokerctl.BeginSession(w.app, st, cli.VersionString(), agent)
		},
		// BeginUnattendedSession records a fresh start lever makes on its own, only
		// over an unchanged skill.
		BeginUnattendedSession: func(agent string) (apply.UnattendedSession, error) {
			return brokerctl.BeginUnattendedSession(w.app, st, cli.VersionString(), agent)
		},
		// InspectContainerMounts lets apply warn when a kept or resumed manager lacks
		// the manager.read_only mounts (create-time only), and refuse a resume whose
		// record mounts a directory that is gone.
		InspectContainerMounts: func(ctx context.Context, ref string) ([]jail.Mount, error) {
			return jail.ContainerMounts(ctx, b.JailRunner(), ref)
		},
		CheckLiveMounts: func(ctx context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			return jail.ContainerLiveMounts(ctx, b.JailRunner(), ref, want)
		},
		ProbeContainerDevice: func(ctx context.Context, ref, path string) (bool, error) {
			return jail.ContainerHasCharDevice(ctx, b.JailRunner(), ref, path)
		},
		ReadClaudeSettings: jail.AgentProbe{R: b.JailRunner()}.ClaudeSettings,
		BackgroundSessions: jail.AgentProbe{R: b.JailRunner()}.BackgroundSessions,
		HarnessLogTail:     jail.AgentProbe{R: b.JailRunner()}.LogTail,
		RecordVolumes:      w.recordVolumes,
		AgentSession:       jail.AgentProbe{R: b.JailRunner()},
		// ContainerRunning asks podman itself, not the hub's heartbeat.
		ContainerRunning: func(ctx context.Context, ref string) (bool, error) {
			return jail.ContainerRunning(ctx, b.JailRunner(), ref)
		},
		// ContainerStartedBeforeBoot is the guest-restart recovery's evidence when no
		// boot id was kept.
		ContainerStartedBeforeBoot: func(ctx context.Context, ref string) (bool, error) {
			return jail.ContainerStartedBeforeBoot(ctx, b.JailRunner(), ref)
		},
		// StaleContainer lets a manager resume clear a hub stop claim that only
		// removing the manager's stopped container releases.
		StaleContainer: jail.ContainerJanitor{R: b.JailRunner()},
		AgentRevoked:   agentRevoked(w.app, st),
		// GuestBoot lets start-manager and up recover a manager record a hard VM kill
		// left "running".
		GuestBoot: brokerctl.GuestBootFor(w.app, st, b.JailRunner()),

		EnsureScionTelemetry: w.ensureScionTelemetry,
		EnsureAgentTemplate:  w.ensureAgentTemplate,
		Log:                  w.log,
		StepTail:             w.stepTail,
	}
}

// stepFingerprints is the lifecycle record's step fingerprints, or nil (a
// converge skips nothing) with the state directory inside the tree, where an
// agent could forge them.
func stepFingerprints(app *config.App, st state.State) apply.StepFingerprints {
	if brokerctl.StateInsideTree(app, st) {
		return nil
	}
	return lifecycleSteps{st}
}

type lifecycleSteps struct{ st state.State }

func (l lifecycleSteps) Matches(step, inputs string) bool { return l.st.StepMatches(step, inputs) }
func (l lifecycleSteps) Record(step, inputs string) error {
	return l.st.RecordStep(step, inputs, time.Now())
}
func (l lifecycleSteps) Forget(step string) error { return l.st.ForgetStep(step) }

// convergeDigest is the config a bring-up converges on, as lever loaded it.
func convergeDigest(app *config.App) string { return state.HashJSON(app) }

// convergeNotice says, before up, what changed since the last bring-up that
// ran the whole plan: up converges on the config as it is now, so an edit
// takes effect without an apply, and this keeps that visible. "" when
// nothing changed, nothing is recorded, or the state directory is inside
// the tree (where an agent could write the record).
func convergeNotice(app *config.App, st state.State) string {
	if brokerctl.StateInsideTree(app, st) {
		return ""
	}
	l, err := st.LoadLifecycle()
	if err != nil || l.LastConverge == nil {
		return ""
	}
	last := l.LastConverge
	var changed []string
	if last.Config != convergeDigest(app) {
		changed = append(changed, "the config")
	}
	if last.Version != cli.VersionString() {
		changed = append(changed, "the lever binary")
	}
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("lever: %s changed since the last bring-up (`lever %s`, %s); this up applies it",
		strings.Join(changed, " and "), termsafe.Sanitize(last.By), last.At.Local().Format("2006-01-02 15:04 MST"))
}

// recordConverge notes a bring-up that ran the whole plan, for the next
// one's convergeNotice. A failure only warns: the record informs, it
// guards nothing.
func recordConverge(app *config.App, st state.State, by string, log func(string, ...any)) {
	if brokerctl.StateInsideTree(app, st) {
		return
	}
	if err := st.RecordConverge(by, convergeDigest(app), cli.VersionString(), time.Now()); err != nil {
		log("lever: warning: could not record this bring-up in %s: %v", st.LifecycleFile(), err)
	}
}

func (w *applyWiring) removeJailFile(ctx context.Context, jailPath string) error {
	return removeJailFile(ctx, w.jr, jailPath)
}

func (w *applyWiring) recordVolumes(ctx context.Context, projectName, agentName string) ([]jail.Mount, error) {
	return hubRecordVolumes(ctx, w.hub(), projectName, agentName)
}

// hubRecordVolumes is agentName's record volumes from the hub's agent listing,
// as read-only jail mounts, for when the manager has no container to inspect.
func hubRecordVolumes(ctx context.Context, hc *hubapi.Client, projectName, agentName string) ([]jail.Mount, error) {
	agents, err := hc.Agents(ctx, projectName, scion.DefaultHubEndpoint)
	if err != nil {
		return nil, err
	}
	return recordMounts(agents, agentName)
}

func recordMounts(agents []hubapi.Agent, agentName string) ([]jail.Mount, error) {
	for _, a := range agents {
		if a.Slug != agentName {
			continue
		}
		out := make([]jail.Mount, 0, len(a.Volumes))
		for _, v := range a.Volumes {
			out = append(out, jail.Mount{Source: v.Source, Destination: v.Target, RW: !v.ReadOnly})
		}
		return out, nil
	}
	return nil, fmt.Errorf("no hub record for agent %q", agentName)
}

// hub is the Hub REST client, run in the jail with the controller PAT: the hub
// binds the jail's loopback and the Lima template suppresses guest-to-host
// forwards, so a host-side call could not reach it. The PAT is read per call so
// a re-mint is picked up.
func (w *applyWiring) hub() *hubapi.Client {
	return &hubapi.Client{T: hubJailTransport(w.jr, w.state)}
}

// stripProjectSharedDirs removes scion's default cross-agent `scratchpad` mount
// through the hub (see apply.Deps.StripProjectSharedDirs).
func (w *applyWiring) stripProjectSharedDirs(ctx context.Context, projectName string) error {
	return w.hub().StripSharedDir(ctx, projectName, scion.DefaultHubEndpoint, scionScratchpadSharedDir)
}

// The role is the one the `scion start` stamp uses, so the two cannot drift.
func (w *applyWiring) ensureAgentRoleCeiling(ctx context.Context, projectName string) error {
	return ensureAgentRoleCeiling(ctx, w.sc.RolesSupported, w.hub(), projectName, scion.EffectiveAgentRole(w.app.Scion.AgentRole))
}

// ensureAgentRoleCeiling asks the hub to cap every agent create in the project
// at role, on a scion that has roles. The ceiling setting shipped with
// `--role`, so the flag probe is an exact gate. A probe that cannot answer
// fails closed, as the --role stamp does in scion.Client.Start.
func ensureAgentRoleCeiling(ctx context.Context, rolesSupported func(context.Context) (bool, error), hub *hubapi.Client, projectName, role string) error {
	supported, err := rolesSupported(ctx)
	if err != nil {
		return fmt.Errorf("determining scion agent-role support before writing the project's role ceiling: %w", err)
	}
	if !supported {
		return nil
	}
	_, err = hub.EnsureAgentRoleCeiling(ctx, projectName, scion.DefaultHubEndpoint, role)
	return err
}

// repairScionHubEndpoint points the project's recorded hub endpoint back at
// the real hub when the registration names another port.
func (w *applyWiring) repairScionHubEndpoint(ctx context.Context, wp string) error {
	return w.b.RepairScionHubEndpoint(ctx, wp, scion.DefaultHubEndpoint)
}

// verifyAgentRole refuses to keep an agent whose hub record predates scion's
// roles: that agent holds full hub authority (see apply.Deps.VerifyAgentRole).
// It fails closed on any question it cannot answer.
func (w *applyWiring) verifyAgentRole(ctx context.Context, projectName, agentName string) error {
	return roleCheckErr(agentName, hubapi.VerifyAgentRole(ctx, w.sc.RolesSupported, w.hub(), projectName, agentName))
}

// roleCheckErr gives an agent list page over the reply cap the same recovery
// as scion's oversized list: the role check reads every record in the project.
func roleCheckErr(agentName string, err error) error {
	if tooLarge(err) {
		return fmt.Errorf("checking %q's stored role: %w", agentName, scion.ErrAgentListTooLarge)
	}
	return err
}

// ensureHubAuth installs the assertion public key and the hub's auth block
// in the guest (see guest.EnsureHubAuth).
func (w *applyWiring) ensureHubAuth(ctx context.Context) (string, error) {
	return w.b.EnsureHubAuth(ctx, types.HubAuth{
		PublicKeyPEM: w.hubKey.PublicPEM(),
		Issuer:       hubauth.Issuer(w.instanceID),
		Audience:     hubauth.Audience(w.instanceID),
		AdminEmail:   hubauth.AdminEmail,
		Header:       hubauth.Header,
		RemoteOn:     w.app.RemoteEnabled(),
	})
}

// assertHub is the hub REST client in the jail authenticated as one of
// lever's identities by a fresh assertion per request.
func (w *applyWiring) assertHub(sign func() (string, error)) *hubapi.Client {
	return &hubapi.Client{T: &hubapi.AssertCurl{Runner: w.jr, BaseURL: scion.DefaultHubEndpoint, Sign: sign}}
}

// ensureHubIdentity runs the hub-identity step (hubIdentity.run).
func (w *applyWiring) ensureHubIdentity(ctx context.Context) error {
	h := &hubIdentity{
		app: w.app, st: w.state, jr: w.jr, sc: w.sc,
		admin:     w.assertHub(w.signer.Admin),
		ctl:       w.assertHub(w.signer.Controller),
		pat:       w.hub(),
		jailMount: w.b.MountDest(),
		keyFP:     w.hubKey.Fingerprint(),
		guestNow:  w.clock.Now,
		runStart:  w.runStart,
		reset:     w.resetHubIdentity,
		ssh:       w.ssh,
		restartHub: func(ctx context.Context) error {
			return apply.RestartHub(ctx, w.app, w.deps, "")
		},
		linkBroker: w.ensureProjectProvider,
		log:        w.log,
		warn:       func(format string, args ...any) { w.log("lever: warning: "+format, args...) },
	}
	return h.run(ctx)
}

// pinExperimentsBeforeRestart pins scion's experiments on the hub the
// scion-server step is about to restart (prePinExperiments).
func (w *applyWiring) pinExperimentsBeforeRestart(ctx context.Context) error {
	return prePinExperiments(ctx, experimentsPin{
		admin: w.assertHub(w.signer.Admin), st: w.state, known: w.sc.KnowsUATScope, now: w.clock.Now,
		conduitOn: w.ssh.on, conduitPolicyLive: w.ssh.policyLive,
		log: w.log, warn: func(format string, args ...any) { w.log("lever: warning: "+format, args...) },
	})
}

// projectLinked is the register-project soundness predicate's link half
// (treeLinkedTo) for the project hub-identity recorded.
func (w *applyWiring) projectLinked(ctx context.Context, jp string) (bool, error) {
	rec, found, err := w.state.LoadHubIdentity()
	if err != nil || !found || rec.ProjectID == "" {
		return false, err
	}
	return treeLinkedTo(ctx, w.jr, w.sc.ConfigGetProject, jp, rec.ProjectID)
}

// treeLinkedTo reports whether the jail's scion says the tree's project is
// linked, and to hub project want: by the marker scion init wrote or, after
// register-project's re-init gave the tree a new local ID, by the
// hub.projectId pinHubProject wrote into the jail user's scion config, out of
// the agents' reach.
func treeLinkedTo(ctx context.Context, jr proc.Runner, get func(ctx context.Context, dir, key string) (string, error), jp, want string) (bool, error) {
	linked, err := get(ctx, jp, "hub.linked")
	if err != nil {
		return false, err
	}
	if linked != "true" {
		return false, nil
	}
	id, _, err := readProjectMarker(ctx, jr, jp)
	if err != nil {
		return false, err
	}
	if id == want {
		return true, nil
	}
	hubID, err := get(ctx, jp, "hub.projectId")
	if err != nil {
		return false, err
	}
	return hubID == want, nil
}

func (w *applyWiring) ensureScionTelemetry(ctx context.Context) (bool, error) {
	return w.b.EnsureScionTelemetry(ctx, w.app.EffectiveScionTelemetry() == config.ScionTelemetryOff)
}

// ensureAgentTemplate puts lever's overlay template in front of scion's stock
// `default` (see guest.EnsureLeverTemplate).
//
// The file is written before the setting: a default_template naming a missing
// template would fail every provision, while an unselected template is inert.
// The setting is read first so an operator's own template is kept, and so a
// re-apply stays quiet (`config set` cannot report a change).
func (w *applyWiring) ensureAgentTemplate(ctx context.Context, projectDir string) (bool, error) {
	wrote, err := w.b.EnsureLeverTemplate(ctx)
	if err != nil {
		return false, err
	}
	cur, err := w.sc.ConfigGetProject(ctx, projectDir, "default_template")
	if err != nil {
		return false, fmt.Errorf("read default_template: %w", err)
	}
	if !leverMayClaimTemplate(cur) {
		return wrote, nil
	}
	if err := w.sc.ConfigSetProject(ctx, projectDir, "default_template", guest.LeverTemplateName); err != nil {
		return false, fmt.Errorf("set default_template: %w", err)
	}
	return true, nil
}

// log prints user-facing lines on the command's stderr, or os.Stderr when cmd
// is nil.
func (w *applyWiring) log(format string, args ...any) {
	if w.rep != nil {
		w.rep.Log(fmt.Sprintf(format, args...))
		return
	}
	if w.cmd != nil {
		logLine(w.cmd.ErrOrStderr(), format, args...)
		return
	}
	logLine(os.Stderr, format, args...)
}

// agentRevoked reads the broker's revocation list at each call, since a `lever
// revoke` may land between two heals. The list holds cert CNs. A list that
// cannot be read answers revoked.
func agentRevoked(app *config.App, st state.State) func(agent string) bool {
	return func(agent string) bool {
		rs, err := brokerctl.LoadRevocation(st)
		if err != nil {
			return true
		}
		cn := agent
		if agent == app.Name {
			cn = app.ManagerCN()
		}
		return slices.Contains(rs.Revoked, cn)
	}
}
