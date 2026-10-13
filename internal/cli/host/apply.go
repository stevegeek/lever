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
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
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

// hubProjectKey is the hub's name for an instance's project. It must match the
// key ensureControllerPAT uses for `hub token create`.
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
	cfgHash   string // brokerctl.RemoteConfigHash(app), for the reuse stamp
	// startTimeout/startInterval bound the waits for bind and pid file; zero
	// means remoteProxyStartTimeout/Interval.
	startTimeout  time.Duration
	startInterval time.Duration
	log           logFunc // apply's user-facing line sink (applyWiring.log)
}

func newRemoteController(app *config.App, st state.State, configPath, selfExe string, log logFunc) *remoteController {
	return &remoteController{
		state:      st,
		configPath: configPath,
		selfExe:    selfExe,
		port:       app.EffectiveRemotePort(),
		probeHost:  remoteProbeHost(app),
		version:    cli.VersionString(),
		cfgHash:    brokerctl.RemoteConfigHash(app),
		log:        log,
	}
}

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
	if _, found, alive := rc.state.RemoteStatus(); found && alive {
		if rc.state.RemoteStampMatches(rc.version, rc.cfgHash) && tcpDial(rc.addr()) == nil {
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
	// Stamp only once the proxy is proven listening. Best-effort: a write failure
	// costs the next apply a redundant restart, never a stale process kept alive.
	if err := rc.recordStamp(ctx, cmd.Process.Pid); err != nil {
		rc.log.printf("lever: could not record the remote proxy stamp (%v) — the next apply will restart it", err)
	}
	return nil
}

// recordStamp waits until remote.pid names pid: the proxy binds its port
// before it writes that file.
func (rc *remoteController) recordStamp(ctx context.Context, pid int) error {
	timeout := cmp.Or(rc.startTimeout, remoteProxyStartTimeout)
	interval := cmp.Or(rc.startInterval, remoteProxyStartInterval)
	var last error
	err := retry.Until(ctx, int(timeout/interval)+1, interval, func() (bool, error) {
		got, err := state.ReadPID(rc.state.RemotePID())
		switch {
		case err != nil:
			last = err
		case got != pid:
			last = fmt.Errorf("%s names pid %d, not the new proxy %d", rc.state.RemotePID(), got, pid)
		}
		return err == nil && got == pid, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return fmt.Errorf("after %s: %w", timeout, last)
	}
	if err != nil {
		return err
	}
	return rc.state.WriteRemoteStamp(rc.version, rc.cfgHash)
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

// removeJailFileScript removes only a regular file at $1 (a directory there is
// a stray in-repo git-mode project, not a stale marker) and is a no-op when $1
// is absent.
const removeJailFileScript = `if [ ! -d "$1" ] && [ -e "$1" ]; then rm -f -- "$1"; fi`

func removeJailFile(ctx context.Context, jr proc.Runner, jailPath string) error {
	if _, err := jr.Run(ctx, nil, "sh", "-c", removeJailFileScript, "_", jailPath); err != nil {
		return fmt.Errorf("removing jail file %s: %w", jailPath, err)
	}
	return nil
}

// throwawayHubPort is the Hub API port of ensureControllerPAT's throwaway
// dev-auth hub, distinct from the real hub (8080).
//
// The throwaway runs with the web frontend off (scion.ServerOpts.DisableWeb).
// With it on, scion's devAuthMiddleware signs any session-less request in as
// the dev super-admin, and an agent container reaches jail-loopback ports
// through pasta --map-host-loopback. With it off, the Hub API runs its own
// server on --port and every non-public route needs the Bearer dev token;
// --web-port is then inert.
//
// Web off is not enough: a dev-auth hub upgrades an agent's token to the full
// role on refresh, signed with the key the live hub also uses. So the window
// also refuses to open while any container runs (see ensureControllerPAT).
const throwawayHubPort = 48080

// throwawayHubURL is the endpoint the window's scion CLI and admin calls use.
// The scion CLI falls back to ~/.scion/dev-token by itself; the admin calls
// read it with readDevToken.
var throwawayHubURL = fmt.Sprintf("http://127.0.0.1:%d", throwawayHubPort)

// windowCleanupTimeout bounds the window's cleanup, which runs on a context
// detached from the apply's so an interrupted apply still cleans up.
const windowCleanupTimeout = 2 * time.Minute

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

// remotePATScopes is the exact scope set of the remote-access PAT: attach,
// message, read and list, nothing that creates, deletes or reconfigures. The
// proxy does not send this token, but the lever-remote web role is derived
// from it (remoteRolePermissions), so it must never carry agent:manage,
// agent:create/delete, project:update or any secret scope.
func remotePATScopes() []string {
	return []string{"agent:read", "agent:list", "project:read", "agent:attach", "agent:message"}
}

// patExpires is the lifetime asked for at mint: just under scion's one-year
// maximum, clear of its "no later than now+1y" check on either clock. scion's
// default is 90 days, and an expired token breaks every hub call.
const patExpires = "360d"

// patRenewWindow is how long before expiry apply re-mints.
const patRenewWindow = 30 * 24 * time.Hour

// patMintOpts are ensureControllerPAT's seams; zero values mean time.Now,
// stderr and the real throwaway hub.
type patMintOpts struct {
	Now  func() time.Time
	Warn func(format string, args ...any)
	// AdminHub replaces the throwaway hub transport for the remote web role grant.
	// Tests only.
	AdminHub hubapi.Doer
	// RestartHub starts the live hub with its normal options; a failed window
	// calls it because the window stopped the live hub. nil: the error says the
	// hub is down and how to recover.
	RestartHub func(ctx context.Context) error
	// ScopeKnown asks the jail's scion whether it knows a UAT scope. nil: the
	// recorded variant is kept.
	ScopeKnown scopeKnownFunc
}

func (o patMintOpts) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o patMintOpts) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
		return
	}
	fmt.Fprintln(os.Stderr, termsafe.Sanitize(fmt.Sprintf("lever: warning: "+format, args...)))
}

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

// ensureControllerPAT backs the "bootstrap-token" apply step. In one
// throwaway dev-auth window it mints the controller PAT the real hub is driven
// with and, when remote access is on, the remote-access PAT; with remote
// access on, every window also runs the remote web role and ceiling grant
// (ensureRemoteWebRole), since role admin is hub-admin only and a reset hub
// database makes remote-role.json stale. That record alone opens a window
// when it does not cover every allowed user with the current permissions and
// a ceiling. With no token missing and the grant
// complete, no window opens.
//
// The window never runs beside a running container: an agent reaches
// jail-loopback ports, and a dev-auth hub refreshes an agent token to a full
// role the live hub accepts (see throwawayHubPort). When any container runs,
// or the list fails, a window that is only needed for optional work (the role
// grant, the remote PAT, a renewal) is skipped with a warning; one the
// instance cannot run without fails the apply and names the repair (`lever
// stop`, then `lever up`).
//
// The window stops the live hub, starts a dev-auth-on, web-off hub on
// throwawayHubPort, runs `scion init` and `hub link` (idempotent), mints the
// missing PATs with exactly the wanted scopes, persists each 0600, runs the
// role grant, stops the throwaway and deletes scion's residual dev-token so it
// does not linger as an admin credential. Both hubs share the jail's ~/.scion
// database. The cleanup runs on a context detached from ctx
// (windowCleanupTimeout) so an interrupted apply still closes the window;
// `lever doctor` reports one left open (checkDevAuthWindow).
func ensureControllerPAT(ctx context.Context, jr proc.Runner, st state.State, tree, jailMount string, remote remoteAccess, o patMintOpts) (err error) {
	now := o.now()
	ctok, _ := st.LoadControllerPAT()
	crec, cfound, err := st.LoadControllerPATRecord()
	if err != nil {
		return fmt.Errorf("bootstrap-token: %w", err)
	}
	// The jail's scion answers this with the hub down, so a pin upgrade followed
	// by `lever up` re-mints while no container runs.
	clifecycle := controllerLifecycle(ctx, o.ScopeKnown, crec)
	controllerScopes := controllerPATScopes(clifecycle, controllerInbox(ctx, o.ScopeKnown, crec))
	controllerReason := patMintReason(ctok, crec, cfound, controllerScopes, now)
	urgent := patUrgent(ctok, crec, cfound, controllerScopes, now)
	remoteReason := ""
	var rrec state.PATRecord
	roleReason := ""
	lifecycle := false
	if remote.Enabled {
		rtok, _ := st.LoadRemotePAT()
		var rfound bool
		rrec, rfound, err = st.LoadRemotePATRecord()
		if err != nil {
			return fmt.Errorf("bootstrap-token: %w", err)
		}
		remoteReason = patMintReason(rtok, rrec, rfound, remotePATScopes(), now)
		rolerec, rolefound, err := st.LoadRemoteRoleRecord()
		if err != nil {
			return fmt.Errorf("bootstrap-token: %w", err)
		}
		// The jail's scion answers this with the hub down, so a pin upgrade followed
		// by `lever up` re-grants the role while no container runs.
		lifecycle = remoteRoleLifecycle(ctx, o.ScopeKnown, rolerec)
		roleReason = remoteRoleReason(rolerec, rolefound, remote.Emails, remote.Contacts, remoteRolePermissions(lifecycle))
	}
	exp, err := checkHubExperimentsForApply(ctx, &hubapi.Client{T: hubJailTransport(jr, st)}, st, o.ScopeKnown)
	if err != nil {
		return fmt.Errorf("bootstrap-token: %w", err)
	}
	if controllerReason == "" && remoteReason == "" && roleReason == "" && exp.reason == "" {
		return nil // nothing to mint or grant; no dev-auth window
	}
	if blocked := windowBlocked(ctx, jr); blocked != "" {
		why := strings.Join(nonEmpty(controllerReason, remoteReason, roleReason, exp.reason), "; ")
		if exp.urgent {
			return experimentsWindowBlocked(exp.reason, blocked)
		}
		if urgent {
			return fmt.Errorf("bootstrap-token: the controller token must be minted (%s), but the dev-auth window cannot open: %s. "+
				"Agents can reach the jail loopback, and the window's dev-auth hub would hand them admin-grade tokens. "+
				"Run `lever stop`, then `lever up` (the manager conversation is kept)", controllerReason, blocked)
		}
		o.warn("bootstrap-token: skipped the dev-auth window (%s): %s; apply goes on without it. "+
			"To run it, `lever stop`, then `lever up`", why, blocked)
		return nil
	}

	tw := scion.New(jr, scion.Options{HubEndpoint: throwawayHubURL, SettingsWarnLog: brokerctl.StateLogAppender(st)})
	started := false
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowCleanupTimeout)
		defer cancel()
		err = closeDevAuthWindow(cctx, jr, tw, started, err, o)
	}()
	// The live hub (or a throwaway left behind) is stopped first: scion keeps one
	// server pid file per jail home, so the throwaway's start would be refused and
	// its stop would stop the live hub. ServerStop waits for the daemon to exit so
	// the throwaway's port check does not race it. A failure from here on restarts
	// the live hub in the cleanup.
	if err := tw.ServerStop(ctx); err != nil {
		return fmt.Errorf("bootstrap-token: stopping the hub for the dev-auth window: %w", err)
	}
	if err := tw.ServerStart(ctx, scion.ServerOpts{HubPort: throwawayHubPort, DisableWeb: true, DisableRuntimeBroker: true, DevAuth: true, Exclusive: true}); err != nil {
		// A start that launched the daemon but timed out on readiness still owns the
		// pid file; stop it unless scion refused outright (the pid file then names
		// another daemon).
		started = !scion.AlreadyRunning(err)
		return fmt.Errorf("bootstrap-token: throwaway server: %w", err)
	}
	started = true
	if exp.has {
		if err := pinHubExperimentsOff(ctx, jr, st, now, o); err != nil {
			return err
		}
	}

	jp := apply.JailPath(tree, tree, jailMount)
	if err := tw.InitProject(ctx, jp); err != nil {
		return fmt.Errorf("bootstrap-token: init project: %w", err)
	}
	if err := tw.HubLink(ctx, jp); err != nil {
		return fmt.Errorf("bootstrap-token: hub link: %w", err)
	}
	// scion's `hub token create` requires --project and --name; the project name
	// is jp's basename and each PAT's label is fixed.
	//
	// Each mint persists the token, then its record, and only then revokes the
	// token it replaced, so a failure at any point leaves a working token on disk.
	// The revoke is best-effort but reported: a valid token nobody holds is still
	// a valid token.
	mint := func(name string, scopes []string, old state.PATRecord, reason string,
		save func(string) error, saveRec func(state.PATRecord) error) error {
		// A first mint is silent; replacing a token an operator may hold elsewhere is
		// worth a line.
		if reason != patReasonAbsent {
			o.warn("bootstrap-token: re-minting %s: %s", name, reason)
		}
		tok, err := tw.HubTokenCreate(ctx, jp, filepath.Base(jp), name, scopes, patExpires)
		if err != nil {
			return fmt.Errorf("bootstrap-token: hub token create %s: %w", name, err)
		}
		if err := save(tok.Token); err != nil {
			return fmt.Errorf("bootstrap-token: persisting %s: %w", name, err)
		}
		if err := saveRec(state.PATRecord{
			ID: tok.ID, Requested: scopes, Granted: tok.Scopes, MintedAt: now, ExpiresAt: tok.ExpiresAt,
		}); err != nil {
			return fmt.Errorf("bootstrap-token: persisting %s record: %w", name, err)
		}
		if old.ID != "" {
			if err := tw.HubTokenRevoke(ctx, jp, old.ID); err != nil {
				o.warn("bootstrap-token: could not revoke the superseded %s token %s (it stays valid in the hub until %s): %v",
					name, old.ID, expiryText(old.ExpiresAt), err)
			}
		}
		return nil
	}
	if controllerReason != "" || remoteReason != "" {
		grantControllerRole(ctx, jr, filepath.Base(jp), clifecycle, o)
	}
	if controllerReason != "" {
		if err := mint("lever-controller", controllerScopes, crec, controllerReason,
			st.SaveControllerPAT, st.SaveControllerPATRecord); err != nil {
			return err
		}
	}
	if remoteReason != "" {
		if err := mint("lever-remote", remotePATScopes(), rrec, remoteReason,
			st.SaveRemotePAT, st.SaveRemotePATRecord); err != nil {
			return err
		}
	}
	if remote.Enabled {
		if err := grantRemoteWebRole(ctx, jr, st, filepath.Base(jp), remote, lifecycle, now, o); err != nil {
			return err
		}
	}
	return nil
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// windowBlocked says why the dev-auth window must not open now, or "" when it
// may. Any running container blocks it, not only scion's agents (any container
// with the loopback map can reach the throwaway hub), and so does a failed
// list: the window cannot then prove it runs agent-free.
func windowBlocked(ctx context.Context, jr proc.Runner) string {
	res, err := jr.Run(ctx, nil, "podman", "ps", "--format", "{{.Names}}")
	if err != nil {
		return "lever cannot list the jail's running containers (" + firstLine(err.Error()) + ")"
	}
	names := strings.Fields(res.Stdout)
	if len(names) == 0 {
		return ""
	}
	return "containers are running in the jail (" + strings.Join(names, ", ") + ")"
}

// closeDevAuthWindow is the window's cleanup; its return value replaces err.
// It stops the throwaway (when this window started it), deletes the residual
// dev token, and restarts the live hub when the window failed.
//
// A throwaway that will not stop is an error even on a successful window: a
// running dev-auth hub is an open admin surface.
//
// scion writes the dev token to ~/.scion/dev-token of the jail user (not
// /home/scion, the agent-container user); that home is resolved in-jail.
// scion reuses an existing file on the next dev-auth start, so a file this
// misses becomes the next window's token; `lever doctor` reports it.
func closeDevAuthWindow(ctx context.Context, jr proc.Runner, tw *scion.Client, started bool, err error, o patMintOpts) error {
	stopFailed := false
	if started {
		// No container ran when the window opened. One running now started during it
		// and may hold a full-role token from the dev-auth hub: fail loudly.
		if b := windowBlocked(ctx, jr); b != "" {
			err = errors.Join(err, fmt.Errorf("bootstrap-token: a container started during the dev-auth window (%s); "+
				"it may hold a full-role agent token. Run `lever stop`, then check the hub's agents before `lever up`", b))
		}
		if serr := tw.ServerStop(ctx); serr != nil {
			stopFailed = true
			err = errors.Join(err, fmt.Errorf("bootstrap-token: the throwaway dev-auth hub on 127.0.0.1:%d did not stop: %w; "+
				"stop it with `scion server stop` in the jail, then run `lever apply`", throwawayHubPort, serr))
		}
	}
	if home, herr := jr.Run(ctx, nil, "sh", "-c", `printf %s "$HOME"`); herr == nil {
		if h := strings.TrimSpace(home.Stdout); h != "" {
			_ = removeJailFile(ctx, jr, h+"/"+layout.DevTokenRel)
		}
	}
	if err == nil {
		return nil
	}
	if o.RestartHub == nil || stopFailed {
		// With the throwaway still up, a restart would be answered "already running"
		// about the throwaway and wrongly reported as done.
		return fmt.Errorf("%w; the live hub is stopped (the dev-auth window stopped it): run `lever apply` again to start it", err)
	}
	if rerr := o.RestartHub(ctx); rerr != nil {
		return fmt.Errorf("%w; the live hub is stopped (the dev-auth window stopped it) and restarting it failed: %v. "+
			"Run `lever apply` again, or `lever stop`, then `lever up`", err, rerr)
	}
	return fmt.Errorf("%w (the live hub was started again)", err)
}

// grantRemoteWebRole runs ensureRemoteWebRole against the throwaway hub and
// persists its record. Most failures only warn: the PATs are minted, the
// instance works without the web UI, and the missing record makes the next
// apply retry. errCeilingRemoved fails the apply: a user's ceiling was deleted
// and not rewritten, so that user can create projects until the next apply.
func grantRemoteWebRole(ctx context.Context, jr proc.Runner, st state.State, projectKey string, remote remoteAccess, lifecycle bool, now time.Time, o patMintOpts) error {
	fail := func(err error) {
		o.warn("bootstrap-token: remote web role not granted, so the web UI may answer 403; the next `lever apply` retries: %v", err)
	}
	hub, err := windowAdminHub(ctx, jr, o)
	if err != nil {
		fail(err)
		return nil
	}
	rec, err := ensureRemoteWebRole(ctx, &hubapi.Client{T: hub}, projectKey, remote.Emails, remote.Contacts, lifecycle, now, o.warn)
	if errors.Is(err, errCeilingRemoved) {
		return fmt.Errorf("bootstrap-token: remote web role: %w", err)
	}
	if err != nil {
		fail(err)
		return nil
	}
	if err := st.SaveRemoteRoleRecord(rec); err != nil {
		fail(fmt.Errorf("persisting remote-role.json: %w", err))
	}
	return nil
}

// expiryText: zero means scion did not say.
func expiryText(t time.Time) string {
	if t.IsZero() {
		return "its expiry (unknown)"
	}
	return t.Format(time.RFC3339)
}

// applySignalContext returns a context that SIGINT or SIGTERM cancels, so the
// step in flight fails and its cleanup runs. Without it a signal kills lever
// at once and can leave a dev-auth hub listening, its dev token on disk and the
// live hub stopped. The first signal restores the default action, so a second
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
	var dryRun bool
	var progressMode string
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
			run, pctx, err := startBringup(cmd, app, stateFor(path), "apply", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			printRemoteWarnings(cmd, app)
			if err := checkContactGate(app, stateFor(path)); err != nil {
				return err
			}
			printContactSessionWarnings(cmd, app, stateFor(path))
			ctx, stop := applySignalContext(pctx)
			defer stop()
			run.rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: run.rep})
			if err != nil {
				return err
			}
			if err := apply.Run(ctx, app, w.deps, apply.PlanOpts{}); err != nil {
				return err
			}
			run.finish(nil)
			cmd.Printf("application %q is up.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the bring-up plan and exit")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
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
	// brokerHost is what agents and the guest login forwarder dial the host by:
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
	// call time, so verbs pick it up once ensureControllerPAT persists it mid-apply.
	st := stateFor(configPath)
	sc := brokerctl.HostScionClient(jr, st, app.Scion.AgentRole)

	// The hub's session signing key is generated once and never rotated by lever
	// (see EnsureSessionSecret).
	sessionSecret, err := st.EnsureSessionSecret()
	if err != nil {
		return nil, err
	}

	w := &applyWiring{b: b, sc: sc, app: app, jr: jr, state: st, cmd: opts.Cmd, rep: opts.Rep}
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
	return w, nil
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
		Scion:            sc,
		JailMount:        b.MountDest(),
		HubSessionSecret: sessionSecret,

		// RemoveJailFile goes through the jail runner so the removal shares the
		// jail's filesystem view with the `scion init` that follows (a VirtioFS
		// unlink/init race; see the register-project case in internal/apply/run.go).
		RemoveJailFile: w.removeJailFile,

		EnsureControllerPAT:     w.ensureControllerPAT,
		EnsureHubExperimentsOff: w.ensureHubExperimentsOff,

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
		WaitBrokerReady: sc.RuntimeBrokerReady,

		RearmBootstrap: bc.Rearm,

		// StopRemoteProxy needs no controller: teardown is by pid file.
		StartRemoteProxy: rc.Start,
		StopRemoteProxy:  func(context.Context) error { return brokerctl.StopRemoteProxy(st) },
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
		AgentRevoked: agentRevoked(w.app, st),
		// GuestBoot lets start-manager and up recover a manager record a hard VM kill
		// left "running".
		GuestBoot: jailGuestBoot{jr: b.JailRunner(), st: st},

		EnsureHubLogin:       w.ensureHubLogin,
		DisableHubLogin:      b.DisableHubLogin,
		EnsureScionTelemetry: w.ensureScionTelemetry,
		EnsureAgentTemplate:  w.ensureAgentTemplate,
		Log:                  w.log,
		StepTail:             w.stepTail,
	}
}

func (w *applyWiring) removeJailFile(ctx context.Context, jailPath string) error {
	return removeJailFile(ctx, w.jr, jailPath)
}

// RestartHub starts the live hub exactly as the scion-server step would, so a
// window that failed after stopping it does not leave the instance without one.
func (w *applyWiring) ensureControllerPAT(ctx context.Context) error {
	return ensureControllerPAT(ctx, w.jr, w.state, w.app.Tree, w.b.MountDest(), remoteAccessFor(w.app), patMintOpts{
		RestartHub: func(ctx context.Context) error {
			opts := apply.HubServerOpts(w.app, w.deps.HubSessionSecret)
			opts.Progress = w.deps.Log
			return w.sc.ServerStart(ctx, opts)
		},
		ScopeKnown: w.sc.KnowsUATScope,
		// Through the wiring's log: sanitized, and above the progress line.
		Warn: func(format string, args ...any) { w.log("lever: warning: "+format, args...) },
	})
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
// the real hub after a re-mint linked it to the throwaway.
func (w *applyWiring) repairScionHubEndpoint(ctx context.Context, wp string) error {
	return w.b.RepairScionHubEndpoint(ctx, wp, scion.DefaultHubEndpoint)
}

// verifyAgentRole refuses to keep an agent whose hub record predates scion's
// roles: that agent holds full hub authority (see apply.Deps.VerifyAgentRole).
// It fails closed on any question it cannot answer.
func (w *applyWiring) verifyAgentRole(ctx context.Context, projectName, agentName string) error {
	return hubapi.VerifyAgentRole(ctx, w.sc.RolesSupported, w.hub(), projectName, agentName)
}

// ensureHubLogin is a no-op with remote access off: no provider runs then, so a
// forwarder would point at nothing and an oidc_login block would advertise a
// login that cannot complete.
func (w *applyWiring) ensureHubLogin(ctx context.Context) (bool, error) {
	if !w.app.RemoteEnabled() {
		return false, nil
	}
	return w.b.EnsureHubLogin(ctx, types.HubLogin{
		IssuerPort:  config.GuestLoginIssuerPort,
		HostPort:    w.app.EffectiveRemoteLoginPort(),
		HostAddress: w.brokerHost,
		ClientID:    remoteproxy.LoginClientID,
	})
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
