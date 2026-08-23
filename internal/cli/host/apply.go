package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// scionScratchpadSharedDir is the shared directory scion stamps on every new
// project (scion#925). It is mounted read-write into every agent of the
// project, so lever removes it — see Deps.StripProjectSharedDirs.
const scionScratchpadSharedDir = "scratchpad"

// hubJailTransport builds the Hub REST transport: curl, inside the jail,
// carrying the controller PAT. Shared by apply's shared-dir strip and doctor's
// shared-dir check so both address the same hub the in-jail scion CLI does.
func hubJailTransport(jr proc.Runner, state state.State) *hubapi.JailCurl {
	return &hubapi.JailCurl{
		Runner:  jr,
		BaseURL: scion.DefaultHubEndpoint,
		Token:   func() string { t, _ := state.LoadControllerPAT(); return t },
	}
}

// hubProjectKey is the name the hub knows a lever instance's project by: the
// basename of its in-jail mount. ensureControllerPAT derives the same key for
// `hub token create`, so the two stay consistent by construction.
func hubProjectKey(jailMount string) string { return filepath.Base(jailMount) }

// detachedSelfCmd builds a detached re-exec of this binary (`self args...`):
// its OWN session (Setsid — survives the parent terminal/session, no
// controlling TTY), stdout+stderr appended to outLog (so a bind failure or
// panic is inspectable, not discarded), and the parent's environment plus
// env. Used for `lever broker serve` and `lever remote serve`; each serve
// process writes its own pid file, not this.
//
// On a fresh apply the state dir (.lever-state) does not exist yet — it's
// created by EnsureKeys inside the spawned child, too late for this open —
// so the log's parent is created here, or the whole bring-up hard-fails
// before the daemon is ever spawned.
func detachedSelfCmd(self, outLog string, env []string, args ...string) (*exec.Cmd, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(outLog), 0o700); err != nil {
		return nil, nil, fmt.Errorf("%s: log dir: %w", args[0], err)
	}
	f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: out log: %w", args[0], err)
	}
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd, f, nil
}

// brokerServeCmd builds the detached `lever broker serve` command with the env
// the broker needs to issue its cert + reach the jail.
func brokerServeCmd(self, configPath, outLog, aliasV4, runUser, runUID string) (*exec.Cmd, *os.File, error) {
	return detachedSelfCmd(self, outLog, []string{
		"LEVER_HOST_ALIAS_IP=" + aliasV4,
		"LEVER_JAIL_USER=" + runUser,
		"LEVER_JAIL_UID=" + runUID,
	}, "broker", "serve", configPath)
}

// remoteServeCmd builds the detached `lever remote serve <config>` command. No
// env of its own: the proxy resolves its jail identity lazily, on its first
// dial (jailPrefixFn in remote.go), so it needs nothing beyond the parent's
// environment — which is where the jail transport binary is found on PATH.
func remoteServeCmd(self, configPath, outLog string) (*exec.Cmd, *os.File, error) {
	return detachedSelfCmd(self, outLog, nil, "remote", "serve", configPath)
}

// logFunc is the sink for apply's loud, user-facing lines (Deps.Log and the
// controllers' restart notices). A nil logFunc prints to stderr, so a
// controller built directly (tests) never loses a line.
type logFunc func(format string, args ...any)

func (f logFunc) printf(format string, args ...any) {
	if f == nil {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return
	}
	f(format, args...)
}

// remoteController owns the apply-time remote-proxy lifecycle: spawning the
// detached `lever remote serve` child, exactly like brokerController.Start
// spawns `lever broker serve` (same Setsid pattern via remoteServeCmd).
//
// The SPAWN is fire-and-forget — the proxy exposes no HTTP identity endpoint
// like the broker's /epoch to health-poll against — but Start does not return
// until the proxy has bound its listener, or the wait has run out. So a
// startup failure (the backend gate in `remote serve` refusing a non-orbstack
// config; a port already taken) fails the apply step synchronously, with the
// last line of state.RemoteLog() quoted into the error. See awaitListening for
// why that check exists: without it, a proxy that died on a deterministic bind
// error left `lever apply` reporting success.
type remoteController struct {
	state      state.State
	configPath string
	selfExe    string // the binary re-exec'd as `remote serve`; see applyOpts.SelfExe
	port       int    // app.EffectiveRemotePort()
	version    string // this binary, for the reuse stamp
	cfgHash    string // state.RemoteConfigHash(app), for the reuse stamp
	// startTimeout/startInterval bound awaitListening's wait for the spawned
	// proxy to bind; zero means remoteProxyStartTimeout/Interval. Set short by
	// tests whose stand-in never binds.
	startTimeout  time.Duration
	startInterval time.Duration
	log           logFunc // apply's user-facing line sink (applyWiring.log)
}

func (rc *remoteController) addr() string { return fmt.Sprintf("127.0.0.1:%d", rc.port) }

// Start spawns `lever remote serve <config>` as a daemonized child so it
// outlives the apply invocation.
//
// Idempotent: if a proxy is already alive, actually listening on the configured
// port, AND running this binary with this remote config, reuse it rather than
// spawning a duplicate — a duplicate would fail to bind and die, clobbering
// remote.pid with a dead pid, the same failure mode brokerController.Start's
// #19 reuse shortcut guards against for the broker. Liveness is the same
// pidfile+TCP-dial check `lever remote status` uses (state.PIDStatus +
// tcpDial): a live-but-not-yet-listening pid (a startup race) or a
// stale/absent pid both fall through to a fresh spawn.
//
// The stamp check is the third condition, and it is NOT optional. The proxy
// reads its config once and caches all of it in the handler it builds at
// startup — ServeHost from base_url, the allowed-user set by value, the bound
// ports — so a running proxy goes on enforcing the config it was born with.
// Without this, editing `remote:` and re-applying reported success while
// changing nothing: enabling `allowed_users` on the live instance left the old
// process serving, and identity-free requests kept returning 200. A
// security-relevant config change that is silently ignored is worse than one
// that fails loudly. brokerController.Start has always compared version+hash
// this way (via the broker's /epoch); the proxy has no such endpoint, and must
// not grow one — it fronts the hub, so any listener of its own would be
// reachable by whatever reaches the proxy. Hence a host-side stamp file.
func (rc *remoteController) Start(ctx context.Context) error {
	if _, found, alive := state.PIDStatus(rc.state.RemotePID()); found && alive {
		if rc.state.RemoteStampMatches(rc.version, rc.cfgHash) && tcpDial(rc.addr()) == nil {
			return nil // already serving: this binary, this config, this port
		}
		// A live proxy that is NOT the one this config asks for. Stop it before
		// spawning, or the replacement cannot bind and dies into remote.log.
		//
		// This decision is deliberately OUTSIDE the tcpDial check. Nesting it
		// there — as the first version of this fix did — meant the ONE change
		// that makes the dial fail, a changed `remote.port`, skipped the stop
		// entirely: the old proxy kept serving the OLD port with the OLD
		// config, `tailscale serve` still pointed at it, and the fresh spawn
		// then OVERWROTE remote.pid, so no `lever` verb could ever stop the
		// leaked process again. An independent review caught it.
		rc.log.printf("lever: the remote proxy predates this binary or its remote config changed — restarting it")
		if err := brokerctl.StopRemoteProxy(rc.state); err != nil {
			return fmt.Errorf("restarting the remote proxy: %w", err)
		}
	}
	cmd, logf, err := remoteServeCmd(rc.selfExe, rc.configPath, rc.state.RemoteLog())
	if err != nil {
		return err
	}
	// Keep the log fd owned by the child; close our copy after Start.
	defer logf.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lever remote serve: %w", err)
	}
	if err := rc.awaitListening(ctx, cmd); err != nil {
		return err
	}
	// Stamp only once the proxy is proven listening, so a stamp never claims a
	// process that never served. Best-effort: a write failure costs the next
	// apply a redundant restart, never a stale process kept alive.
	if err := rc.state.WriteRemoteStamp(rc.version, rc.cfgHash); err != nil {
		rc.log.printf("lever: could not record the remote proxy stamp (%v) — the next apply will restart it", err)
	}
	return nil
}

// remoteProxyStartTimeout/Interval are the default bounds on the wait for the
// spawned proxy to bind (remoteController.startTimeout/startInterval).
// Generous against a loaded host, and still far below anything a human would
// notice: both listeners are host loopback binds that happen before the proxy
// touches the jail.
const (
	remoteProxyStartTimeout  = 5 * time.Second
	remoteProxyStartInterval = 50 * time.Millisecond
)

// errChildGone is awaitListening's signal that the spawned child exited.
var errChildGone = errors.New("child exited")

// awaitListening waits for the spawned proxy to actually bind, and reports
// what the log says when it does not.
//
// The spawn itself is fire-and-forget, like the broker's — but "started" and
// "serving" are not the same thing, and the difference was invisible: a proxy
// that died on a deterministic bind error (a port already taken) left `lever
// apply` printing "is up" and exiting 0, with the operator's next signal a 502
// in a browser. A failure that reproduces on every apply must fail the apply.
//
// The log's tail is quoted into the error because the cause is always there
// and nowhere else — the child owns that file, and this process never sees its
// stderr.
// child is the (started) process just spawned: a listener alone does not
// prove OUR proxy is serving — some other process (including a leaked older
// proxy) may hold the port, and concluding "listening" then would stamp a
// config nothing is enforcing.
func (rc *remoteController) awaitListening(ctx context.Context, child *exec.Cmd) error {
	timeout := cmp.Or(rc.startTimeout, remoteProxyStartTimeout)
	interval := cmp.Or(rc.startInterval, remoteProxyStartInterval)
	attempts := int(timeout/interval) + 1
	err := retry.Until(ctx, attempts, interval, func() (bool, error) {
		if err := child.Process.Signal(syscall.Signal(0)); err != nil {
			// The child is gone; whatever may be listening is not it.
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
		return fmt.Errorf("the remote proxy started but is not listening on %s: %s (see %s)",
			rc.addr(), tail, rc.state.RemoteLog())
	}
	return fmt.Errorf("the remote proxy started but is not listening on %s — see %s",
		rc.addr(), rc.state.RemoteLog())
}

func lastLogLine(path string) string { return lastFileLine(path, 4<<10) }

// removeJailFileScript guards a jail-side rm: it only removes a REGULAR file
// at $1 (a directory at $1 is left untouched — a stray in-repo git-mode
// project, not a stale marker) and is a no-op if $1 is already absent.
// Shared by Deps.RemoveJailFile (register-project's stale-marker cleanup, see
// the comment at that call site in buildApplyDeps) and ensureControllerPAT's
// residual dev-token cleanup below, so the guard text lives in exactly one
// place.
const removeJailFileScript = `if [ ! -d "$1" ] && [ -e "$1" ]; then rm -f -- "$1"; fi`

// removeJailFile runs removeJailFileScript through jr against a jail-absolute
// path. Best-effort by convention at call sites that don't want a missing (or
// already-removed) target to fail the caller.
func removeJailFile(ctx context.Context, jr proc.Runner, jailPath string) error {
	if _, err := jr.Run(ctx, nil, "sh", "-c", removeJailFileScript, "_", jailPath); err != nil {
		return fmt.Errorf("removing jail file %s: %w", jailPath, err)
	}
	return nil
}

// throwawayHubPort is the port ensureControllerPAT's throwaway dev-auth hub is
// reached on — a distinct port from the real hub (8080). lever runs scion in
// workstation (combined) mode, where the Hub API rides the web server's port and
// the standalone --port flag is IGNORED (verified live: `--port 48080` binds
// :8080); --web-port is what actually binds it (verified: `--web-port 48080`
// binds :48080). ServerStart emits --web-port, so the throwaway lands here,
// physically isolated from the real dev-auth-OFF hub the scion-server apply step
// starts on 8080 right after. The throwaway's dev-auth window is agent-free +
// jail-loopback only (the "agent-free window").
const throwawayHubPort = 48080

// controllerPATScopes is the EXACT scope set the controller PAT is minted
// with. agent:message is deliberately omitted — the scion authz review found
// every interactive verb, message included, gates on agent:attach.
// project:update is required for the post-register scratchpad-shared-dir strip
// (scion#925): the shared-dirs REST endpoint gates on project ActionUpdate,
// and agent:manage does NOT expand to project:update.
func controllerPATScopes() []string {
	return []string{"agent:manage", "agent:attach", "project:read", "project:update"}
}

// remotePATScopes is the EXACT scope set the remote-access PAT is minted
// with: interactive (attach gates every interactive verb, message included)
// plus read/list — and nothing that can create, delete, or reconfigure.
// The remote proxy injects this token; it must never carry agent:manage,
// agent:create/delete, project:update, or any secret scope.
func remotePATScopes() []string {
	return []string{"agent:read", "agent:list", "project:read", "agent:attach"}
}

// ensureControllerPAT backs Deps.EnsureControllerPAT, the "bootstrap-token"
// apply step (internal/apply/run.go). It owns TWO mints that both need the
// same throwaway dev-auth window: the controller PAT that the real hub —
// started dev-auth-OFF by the scion-server step right after this one — is
// driven with, and (when remote access is configured on) the narrower
// remote-access PAT the remote proxy injects on the operator's behalf.
//
// Idempotent per token: a PAT already persisted in state short-circuits its
// own mint (survives `down`→`up`; clearStagedRuntimeState only wipes
// tree/.lever/*). If NEITHER token is
// missing this is a complete no-op — no window opens at all.
//
// Why one window: the dev-auth mint window is the sensitive part (a
// throwaway hub with auth off, reachable from the jail loopback only, but
// still an open admin surface while it's up). Minting both tokens in the
// SAME window on a fresh bootstrap — the common case, remote.enabled set
// from the start — preserves the "agent-free window, opened once" property
// instead of opening it twice. If the controller PAT already exists and
// remote is enabled later (instance upgraded, remote.enabled flipped on
// after first bring-up), a second window opens for the remote mint alone;
// that is the same brief, jail-loopback-only repair shape already documented
// for a controller-PAT re-mint (delete state + `lever apply`).
//
// This opens the window by: start a throwaway dev-auth-ON hub on
// throwawayHubPort, `scion init` + `hub link` the project tree (idempotent —
// they already run on every controller re-mint against an existing project;
// the same tolerance covers a remote-only mint), mint whichever PAT(s) are
// missing scoped to exactly controllerPATScopes / remotePATScopes, persist
// each 0600, stop the throwaway hub, and best-effort delete scion's residual
// dev-token file so it doesn't linger as an open admin credential once the
// real hub takes over. The throwaway and real hub share the same jail
// ~/.scion DB BY CONSTRUCTION (both `scion server start` invocations run in
// the same jail home) — no data-dir control point is needed for the minted
// project + PAT(s) to carry over.
//
// jr/tree/jailMount are passed explicitly (rather than closing over
// app/b) purely so this function is unit-testable with fakes; jr is the same
// jail exec.Runner buildApplyDeps already has (this function needs no other
// backend access). remoteEnabled is App.RemoteEnabled() — plumbed as a bool
// rather than closing over *config.App for the same testability reason.
//
// Live-validated against scion 37a54a8e: `scion server start` runs workstation
// (combined) mode where --port is inert and --web-port binds the Hub API
// (ServerStart emits --web-port); `scion server stop`, `hub token create
// --scopes`, and the scopes agent:manage/agent:attach/project:read all exist;
// the residual dev-token is at the jail user's ~/.scion/dev-token (resolved
// in-jail below, not assumed).
func ensureControllerPAT(ctx context.Context, jr proc.Runner, state state.State, tree, jailMount string, remoteEnabled bool) error {
	ctok, _ := state.LoadControllerPAT()
	rtok, _ := state.LoadRemotePAT()
	needController := ctok == ""
	needRemote := remoteEnabled && rtok == ""
	if !needController && !needRemote {
		return nil // nothing to mint; no dev-auth window
	}
	tw := scion.New(jr, scion.Options{HubEndpoint: fmt.Sprintf("http://127.0.0.1:%d", throwawayHubPort)})
	// Register the kill BEFORE ServerStart so a partial start — e.g. a throwaway
	// dev-auth server left running from a prior failed invocation, whose
	// readiness poll then times out — is still stopped rather than leaked as a
	// dev-auth-on admin server. ServerStop tolerates a not-running server; a
	// live run against a scion build without `server stop` needs a
	// jail-pid-kill fallback instead (see ServerStop's doc comment) — a
	// live-validation item, not implemented here.
	//
	// Then a best-effort delete of scion's residual dev-token so it doesn't
	// linger as an open admin credential once the real dev-auth-OFF hub takes
	// over. scion writes it to <scionDir>/dev-token, default ~/.scion/dev-token
	// (pkg/apiclient/devauth.go), where ~ is the JAIL USER's home (here
	// /home/stephen — NOT /home/scion, which is the agent-container user).
	// Resolve that home in-jail rather than hardcode it, then remove through
	// the guarded removeJailFile helper.
	defer func() {
		_ = tw.ServerStop(ctx)
		if home, herr := jr.Run(ctx, nil, "sh", "-c", `printf %s "$HOME"`); herr == nil {
			if h := strings.TrimSpace(home.Stdout); h != "" {
				_ = removeJailFile(ctx, jr, h+"/"+layout.DevTokenRel)
			}
		}
	}()
	if err := tw.ServerStart(ctx, scion.ServerOpts{WebPort: throwawayHubPort, DevAuth: true}); err != nil {
		return fmt.Errorf("bootstrap-token: throwaway server: %w", err)
	}

	jp := apply.JailPath(tree, tree, jailMount)
	if err := tw.InitProject(ctx, jp); err != nil {
		return fmt.Errorf("bootstrap-token: init project: %w", err)
	}
	if err := tw.HubLink(ctx, jp); err != nil {
		return fmt.Errorf("bootstrap-token: hub link: %w", err)
	}
	// scion's `hub token create` requires --project (name or ID) and --name.
	// The project is registered from jp, so its scion project name is jp's
	// basename (jailMount is a constant mount root, so this is stable). Each
	// PAT's label is fixed — one controller PAT and (when enabled) one remote
	// PAT per instance.
	if needController {
		pat, err := tw.HubTokenCreate(ctx, jp, filepath.Base(jp), "lever-controller", controllerPATScopes())
		if err != nil {
			return fmt.Errorf("bootstrap-token: hub token create: %w", err)
		}
		if err := state.SaveControllerPAT(pat); err != nil {
			return fmt.Errorf("bootstrap-token: persisting controller PAT: %w", err)
		}
	}
	if needRemote {
		pat, err := tw.HubTokenCreate(ctx, jp, filepath.Base(jp), "lever-remote", remotePATScopes())
		if err != nil {
			return fmt.Errorf("bootstrap-token: remote token create: %w", err)
		}
		if err := state.SaveRemotePAT(pat); err != nil {
			return fmt.Errorf("bootstrap-token: persisting remote PAT: %w", err)
		}
	}
	return nil
}

func newApplyCmd(bf BackendFactory) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "apply [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an agent-manager application up from a config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			// State the containment posture every bring-up runs under, so the
			// selected backend's guarantees are visible, not assumed.
			if p, ok := backend.ProfileFor(app.Backend); ok {
				cmd.Printf("backend: %s\n", p.Summary())
			}
			if dryRun {
				for _, s := range apply.Plan(app, apply.PlanOpts{}) {
					cmd.Printf("  %-16s %s\n", s.Kind, s.Target)
				}
				return nil
			}
			w, err := buildApplyDeps(cmd.Context(), app, path, bf, applyOpts{Cmd: cmd})
			if err != nil {
				return err
			}
			if err := apply.Run(cmd.Context(), app, w.deps, apply.PlanOpts{}); err != nil {
				return err
			}
			cmd.Printf("application %q is up.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the bring-up plan and exit")
	return c
}

// brokerReusable reports whether a running broker's /epoch identity matches
// this binary + this broker config, i.e. whether apply's broker-reuse shortcut may keep
// it (#19). A broker predating the identity fields reports them empty —
// mismatch — so old brokers are always restarted rather than trusted.
func brokerReusable(got wire.EpochResponse, wantVersion, wantHash string) bool {
	return got.Version == wantVersion && got.ConfigHash == wantHash
}

// brokerController owns the apply-time broker lifecycle: spawning the detached
// `lever broker serve` child, polling it healthy, and minting the manager's
// one-time bootstrap ticket from it. Its Start/Healthy/Mint methods back the
// like-named Deps funcs, and Rearm recombines them for the re-arm-on-create
// path — replacing what used to be a cluster of mutually-referencing closures
// inside buildApplyDeps (so Rearm can reuse the broker-start/health/mint logic
// verbatim without duplicating it). Built once, after EnsureUp/HostAliasV4 have
// resolved the run-user/uid and host alias.
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
	// admin is the loopback client for the broker's admin API. Its timeout
	// bounds every /epoch and /bootstrap call on its own, so a wedged broker
	// cannot hang an apply step whose ctx carries no deadline.
	admin *http.Client
}

// brokerAdminTimeout bounds one admin-API round trip (loopback; the broker
// answers /epoch and /bootstrap from memory).
const brokerAdminTimeout = 5 * time.Second

// Start spawns `lever broker serve <config>` as a daemonized child (its own
// session, via brokerServeCmd) so it outlives the apply invocation.
//
// Idempotent (M2): if a broker is already serving (re-apply), don't spawn a
// duplicate — it would fail to bind the ports, die, and clobber broker.pid with
// a dead PID. A fast single-shot probe (no listener => instant
// connection-refused, so no penalty on a fresh apply). But only reuse a broker
// that matches THIS binary and THIS broker config (#19): a broker started by an
// older binary (no healer, stale routes) or with an older tool set would
// otherwise keep serving while apply reports success — so on identity mismatch,
// stop it and fall through to spawn.
func (bc *brokerController) Start(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var er wire.EpochResponse
	// Anything but a decodable 200 — connection refused, a non-200, a body that
	// is not the epoch JSON — means no broker of ours is serving: fall through
	// to the spawn.
	if err := httpjson.Get(probeCtx, bc.admin, bc.adminURL+wire.PathEpoch, &er); err == nil {
		if brokerReusable(er, cli.VersionString(), brokerctl.ConfigHash(bc.app)) {
			return nil // same binary + same broker config; keep the process + PID
		}
		bc.log.printf("lever: broker predates this binary or its tool config changed — restarting it (was %q)", er.Version)
		if err := brokerctl.StopBroker(bc.state); err != nil {
			return fmt.Errorf("stopping the stale broker before restart: %w", err)
		}
	}
	cmd, logf, err := brokerServeCmd(bc.selfExe, bc.configPath, bc.state.OutLog(), bc.aliasV4, bc.runUser, bc.runUID)
	if err != nil {
		return err
	}
	// Keep the log fd owned by the child; close our copy after Start.
	defer logf.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lever broker serve: %w", err)
	}
	return nil
}

// brokerHealthTimeout bounds Healthy's poll on the wall clock; the interval
// paces the attempts within it. A wall-clock bound rather than an attempt
// count because each probe can itself take up to brokerAdminTimeout.
const (
	brokerHealthTimeout  = 10 * time.Second
	brokerHealthInterval = 200 * time.Millisecond
)

// Healthy polls GET /epoch until 200 or brokerHealthTimeout elapses.
func (bc *brokerController) Healthy(ctx context.Context) error {
	pollCtx, cancel := context.WithTimeout(ctx, brokerHealthTimeout)
	defer cancel()
	var last error
	err := retry.Until(pollCtx, 0, brokerHealthInterval, func() (bool, error) {
		last = httpjson.Get(pollCtx, bc.admin, bc.adminURL+wire.PathEpoch, nil)
		return last == nil, nil
	})
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("broker did not become healthy within %s (last err: %v)", brokerHealthTimeout, last)
	}
	return err
}

// Mint POSTs /bootstrap to obtain the one-time manager enrolment ticket, reads
// the CA PEM from the state dir, and returns the full BootstrapMaterial. A 403
// means the single-use latch was already consumed — surfaced as
// apply.ErrBootstrapLatched so the mint step tolerates it on an idempotent
// re-apply against the same broker process.
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
	}, nil
}

// Rearm backs Deps.RearmBootstrap (see its doc in internal/apply/run.go):
// start-manager's create path calls this when no fresh bootstrap material was
// minted this apply (Mint tolerated a spent latch), because a freshly-created
// scion agent record has no agent home to reuse and so ALWAYS re-enrols —
// against a spent latch that would 403.
//
// Reuses the exact same broker-start/health/mint logic as the broker-up and
// mint-manager-bootstrap steps (no duplicated broker-start logic): stop the
// (possibly still-running) broker so its next start re-arms the single-use
// latch — the CA and signing keys live on disk in the state dir and are
// untouched by a process restart, so existing agent certs and capability tokens
// keep working — then start it fresh, wait for it to become healthy, mint, and
// stage the result into app.Tree/.lever/bootstrap.json via the same
// StageBootstrapMaterial helper the mint-manager-bootstrap step itself uses (one
// staging code path). Staging happens HERE (not in apply/run.go) because
// start-manager's Step.Target is the manager's slug, not the tree dir — this
// controller is the only place that has app.Tree in scope.
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

// leverMayClaimTemplate reports whether lever may point default_template at its
// own overlay, given the value currently in effect.
//
// Only scion's stock "default" is lever's to take. Anything else is either
// already lever's (idempotence — `config set` cannot report whether it changed
// anything, so this is also what keeps a re-apply quiet) or a template the
// operator chose deliberately, which lever must not silently override.
//
// An EMPTY value is not a claim: it means the key is unset, and scion's own
// fallback is "default" (pkg/agent/provision.go), so leaving it alone would
// leave the placeholder in place.
func leverMayClaimTemplate(current string) bool {
	switch strings.TrimSpace(current) {
	case "default", "":
		return true
	default:
		return false
	}
}

// applyWiring is what buildApplyDeps resolved for one apply: the backend
// (already up), the jail runner and state dir, the in-jail scion client, the
// two host daemon controllers, and the apply.Deps built over them. Its methods
// are the Deps closures that need more than a one-line forward.
type applyWiring struct {
	deps  apply.Deps
	b     backend.Backend
	sc    *scion.Client
	app   *config.App
	jr    proc.Runner
	state state.State
	// brokerHost is what agents (and the guest login forwarder) dial the host
	// by: the resolved host-alias IP when the backend has one, else the alias
	// hostname. Under closed-internet egress DNS/53 is dropped, so the
	// hostname cannot be resolved; the IP is already allowlisted and the
	// broker cert carries it as a SAN.
	brokerHost string
	cmd        *cobra.Command // for Log; nil ⇒ stderr
}

// applyOpts carries the per-invocation knobs of buildApplyDeps that are not
// derived from the config.
type applyOpts struct {
	// Cmd is the invoking cobra command, used only to wire Deps.Log (a loud,
	// user-facing progress line — see apply.Deps.Log); may be nil (e.g. tests
	// that never exercise a Log-emitting path), in which case Log falls back
	// to stderr.
	Cmd *cobra.Command
	// SelfExe is the executable the broker and remote controllers re-exec as
	// `broker serve` / `remote serve`. Empty means os.Args[0].
	//
	// This seam is load-bearing, not cosmetic. In production os.Args[0] is
	// the lever binary and re-execing it is exactly right. Under `go test`
	// os.Args[0] is the TEST BINARY, so a spawn becomes `<pkg>.test broker
	// serve …` — and brokerServeCmd detaches the child with Setsid precisely
	// so it outlives its parent. Nothing then reaps it: the processes survive
	// the run, accumulate across runs, and re-spawn each other. A full suite
	// run left 724 of them behind before this seam existed. Tests point it at
	// an inert command.
	SelfExe string
}

// buildApplyDeps wires the live dependencies for apply.Run.
// It eagerly calls EnsureUp so the backend resolves the in-machine
// run-user and UID before the JailRunner and scion.Client are constructed —
// which is why the plan has no jail-up step: the jail is confirmed up and
// the user/uid known before Run can be called at all.
// configPath is the resolved config file path; it is passed to `lever broker
// serve` and used to locate the broker state dir.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory, opts applyOpts) (*applyWiring, error) {
	b, err := bringUpBackend(ctx, app, bf)
	if err != nil {
		return nil, err
	}
	jr := b.JailRunner()

	// state must be built before sc: sc's HubTokenSource closes over it so
	// every verb issued through sc picks up the controller PAT live, once
	// ensureControllerPAT (wired into Deps.EnsureControllerPAT below) persists
	// it mid-apply (see scion.Options.HubTokenSource's doc: lazy, read at
	// call time, wins over a static HubToken).
	st := stateFor(configPath)
	sc := brokerctl.HostScionClient(jr, st, app.Scion.AgentRole)

	// The hub's session-cookie signing key: generated once, adopted verbatim
	// on every later apply, never rotated by lever (see EnsureSessionSecret).
	// Threaded into Deps so every hub start signs sessions with the same key.
	sessionSecret, err := st.EnsureSessionSecret()
	if err != nil {
		return nil, err
	}

	w := &applyWiring{b: b, sc: sc, app: app, jr: jr, state: st, cmd: opts.Cmd}
	aliasV4 := b.HostAliasV4()
	w.brokerHost = cmp.Or(aliasV4, b.HostToolAlias())
	selfExe := cmp.Or(opts.SelfExe, os.Args[0])

	// bc owns the broker lifecycle (start/health/mint) and the re-arm
	// recombination — see brokerController's doc. Built here, after
	// EnsureUp/HostAliasV4, so runUser/runUID/aliasV4 are already resolved.
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

	// rc owns the remote-proxy lifecycle (see remoteController's doc). It
	// only ever runs when app.RemoteEnabled() (the remote-proxy apply step
	// — see internal/apply/plan.go) or, in the disabled direction, when
	// Run's own converge-off reconciliation calls StopRemoteProxy — see
	// internal/apply/run.go.
	rc := &remoteController{
		state:      st,
		configPath: configPath,
		selfExe:    selfExe,
		port:       app.EffectiveRemotePort(),
		version:    cli.VersionString(),
		cfgHash:    state.RemoteConfigHash(app),
		log:        w.log,
	}

	w.deps = w.newDeps(bc, rc, sessionSecret)
	return w, nil
}

// bringUpBackend constructs the backend for app's machine and brings the jail
// up, so the run-user/uid and host alias are resolved before anything is
// built over the jail runner.
func bringUpBackend(ctx context.Context, app *config.App, bf BackendFactory) (backend.Backend, error) {
	machine := machineName(app.Name)
	b, err := bf(app.Backend, machine)
	if err != nil {
		return nil, err
	}
	if err := b.EnsureUp(ctx, backendConfigFor(app, machine)); err != nil {
		return nil, err
	}
	return b, nil
}

// backendConfigFor is the backend.Config apply brings the jail up with.
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
	}
}

// newDeps assembles the apply.Deps over the wiring and the two host daemon
// controllers.
func (w *applyWiring) newDeps(bc *brokerController, rc *remoteController, sessionSecret string) apply.Deps {
	b, sc, st := w.b, w.sc, w.state
	return apply.Deps{
		LoadImage: b.LoadImage,
		// ImageLoaded skips a redundant image re-import when the jail already
		// holds the exact bytes (same image ID as the host) — see the Deps field
		// doc. Fail-open in the backend, so a check failure just loads.
		ImageLoaded: b.ImageLoaded,
		// PruneImages reclaims the dangling image a rebuilt tag orphans, after a
		// load. Best-effort (the apply step logs, never fails, on error).
		PruneImages:      b.PruneJailImages,
		Scion:            sc,
		JailMount:        b.MountDest(),
		HubSessionSecret: sessionSecret,

		// RemoveJailFile removes a regular file at a jail-absolute path THROUGH
		// the jail runner, so the removal shares the jail's own filesystem view
		// with the `scion init` that follows it in the register step (see the
		// comment at the register-project case in
		// internal/apply/run.go for the VirtioFS unlink/init race this closes).
		// The guard leaves directories untouched and is a no-op if the path is
		// already absent.
		RemoveJailFile: w.removeJailFile,

		// EnsureControllerPAT backs the "bootstrap-token" apply step (see
		// ensureControllerPAT's doc above): mint the controller PAT the real,
		// dev-auth-off hub is driven with, and (when remote access is
		// configured on) the narrower remote-access PAT, in one agent-free
		// window.
		EnsureControllerPAT: w.ensureControllerPAT,

		// RemoveScionProjectConfigs clears any stale ~/.scion/project-configs
		// registration(s) for a workspace path before the register step re-inits
		// (see internal/apply/run.go's register-project case) —
		// keeps apply from accumulating a duplicate registration every run.
		RemoveScionProjectConfigs: b.RemoveScionProjectConfigs,

		// ScionProjectRegistered observes whether the register-project
		// apply step (internal/apply/run.go) even needs to run its
		// destructive clean+init path — see RemoveScionProjectConfigs's comment
		// above for why that path exists; this is the idempotency gate that
		// decides whether to run it at all, so a re-apply stops orphaning a
		// resumable scion agent record.
		ScionProjectRegistered: b.ScionProjectRegistered,

		StripProjectSharedDirs: w.stripProjectSharedDirs,
		RepairScionHubEndpoint: w.repairScionHubEndpoint,
		VerifyAgentRole:        w.verifyAgentRole,

		StartBroker:          bc.Start,
		BrokerHealthy:        bc.Healthy,
		MintManagerBootstrap: bc.Mint,

		// WaitBrokerReady gates start-manager on the scion runtime broker being
		// registered + online (see the Deps field doc): the broker registers
		// asynchronously after the Hub API, so the first create/resume would
		// otherwise race it. Fail-soft in the client, so it never fails bring-up.
		WaitBrokerReady: sc.WaitRuntimeBrokerReady,

		// RearmBootstrap backs Deps.RearmBootstrap — see brokerController.Rearm
		// for the full rationale (re-arm the single-use latch on the create path,
		// staging the result because only the controller has app.Tree in scope).
		RearmBootstrap: bc.Rearm,

		// StartRemoteProxy/StopRemoteProxy back the remote-proxy apply step
		// (present only when app.RemoteEnabled() — see plan.go) and Run's
		// own converge-to-off reconciliation when it's false (see run.go).
		// StopRemoteProxy goes straight to state.State — no controller
		// method needed, since teardown-by-pidfile carries no lifecycle
		// state of its own (unlike Start's reuse probe).
		StartRemoteProxy: rc.Start,
		StopRemoteProxy:  func(context.Context) error { return brokerctl.StopRemoteProxy(st) },

		EnsureHubLogin: w.ensureHubLogin,
		// DisableHubLogin removes the guest-side bridge when remote access is
		// off — see apply.Deps.DisableHubLogin for why leaving it running is
		// the part that matters.
		DisableHubLogin:     b.DisableHubLogin,
		EnsureAgentTemplate: w.ensureAgentTemplate,
		Log:                 w.log,
	}
}

// removeJailFile backs Deps.RemoveJailFile through the jail runner.
func (w *applyWiring) removeJailFile(ctx context.Context, jailPath string) error {
	return removeJailFile(ctx, w.jr, jailPath)
}

// ensureControllerPAT backs Deps.EnsureControllerPAT — see the free function.
func (w *applyWiring) ensureControllerPAT(ctx context.Context) error {
	return ensureControllerPAT(ctx, w.jr, w.state, w.app.Tree, w.b.MountDest(), w.app.RemoteEnabled())
}

// hub is the Hub REST client, over curl in the jail with the controller PAT.
// The request runs IN THE JAIL, like every other scion interaction: the hub
// binds the jail's loopback, and the Lima template suppresses every
// guest→host port forward on purpose, so a host-side call could not reach it
// there at all. The PAT is read per call so a re-mint is picked up.
func (w *applyWiring) hub() *hubapi.Client {
	return &hubapi.Client{T: hubJailTransport(w.jr, w.state)}
}

// stripProjectSharedDirs declines scion's default cross-agent `scratchpad`
// mount for this project — see apply.Deps.StripProjectSharedDirs for why it
// exists and why the removal must go through the hub.
func (w *applyWiring) stripProjectSharedDirs(ctx context.Context, projectName string) error {
	return w.hub().StripSharedDir(ctx, projectName, scion.DefaultHubEndpoint, scionScratchpadSharedDir)
}

// repairScionHubEndpoint puts the project's recorded hub endpoint back to the
// real hub after a controller-PAT re-mint linked it at the throwaway one — see
// apply.Deps.RepairScionHubEndpoint.
func (w *applyWiring) repairScionHubEndpoint(ctx context.Context, wp string) error {
	return w.b.RepairScionHubEndpoint(ctx, wp, scion.DefaultHubEndpoint)
}

// verifyAgentRole refuses to keep an agent whose hub record predates scion's
// roles — see apply.Deps.VerifyAgentRole for why that is a promotion to full
// hub authority rather than a cosmetic gap. It fails CLOSED on every question
// it cannot answer: not knowing whether the installed scion has roles, or not
// being able to read the record, is exactly the state in which guessing hands
// out authority.
func (w *applyWiring) verifyAgentRole(ctx context.Context, projectName, agentName string) error {
	return hubapi.VerifyAgentRole(ctx, w.sc.RolesSupported, w.hub(), projectName, agentName)
}

// ensureHubLogin provisions the guest half of the remote login path (see
// apply.Deps.EnsureHubLogin). It is a no-op with remote access off: no
// provider runs host-side then, so a forwarder would point at nothing and an
// oidc_login block would advertise a login that cannot complete. The guest
// reaches the host at the same alias the agents already use for the broker.
func (w *applyWiring) ensureHubLogin(ctx context.Context) (bool, error) {
	if !w.app.RemoteEnabled() {
		return false, nil
	}
	return w.b.EnsureHubLogin(ctx, backend.HubLogin{
		IssuerPort:  config.GuestLoginIssuerPort,
		HostPort:    w.app.EffectiveRemoteLoginPort(),
		HostAddress: w.brokerHost,
		ClientID:    remoteproxy.LoginClientID,
	})
}

// ensureAgentTemplate puts lever's overlay template in front of scion's stock
// `default` — see apply.Deps.EnsureAgentTemplate, and guest.EnsureLeverTemplate
// for WHY an empty system prompt is the fix.
//
// The two halves are ordered file-then-setting deliberately. Pointing
// default_template at a template that does not exist yet would fail every
// provision in between; the reverse order is inert, because a template
// nothing selects is just an unused directory. So a failure after the first
// half leaves the guest in a working state, and the next apply converges it.
//
// The setting is read before it is written so an operator who has
// deliberately chosen their own template keeps it: lever only claims
// default_template while it is still scion's own default. Reading it also
// keeps a re-apply quiet, since `config set` cannot report whether it changed
// anything.
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

// log surfaces start-manager's loud resume-failed recovery notice (see
// apply.Deps.Log) on the invoking command's stderr, mirroring how other
// user-facing warnings already surface (cmd.PrintErrf; see cli/stop.go,
// cli/down.go). A nil cmd (defence in depth for any caller that doesn't have
// one, e.g. a direct test) falls back to os.Stderr so the line is never
// silently lost.
func (w *applyWiring) log(format string, args ...any) {
	if w.cmd != nil {
		w.cmd.PrintErrf(format+"\n", args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
