package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/scion"
)

// scionScratchpadSharedDir is the shared directory scion stamps on every new
// project (scion#925). It is mounted read-write into every agent of the
// project, so lever removes it — see Deps.StripProjectSharedDirs.
const scionScratchpadSharedDir = "scratchpad"

// hubJailTransport builds the Hub REST transport: curl, inside the jail,
// carrying the controller PAT. Shared by apply's shared-dir strip and doctor's
// shared-dir check so both address the same hub the in-jail scion CLI does.
func hubJailTransport(jr leverexec.Runner, state brokerctl.State) *hubapi.JailCurl {
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

// brokerServeCmd builds the detached `lever broker serve` command: its OWN
// session (Setsid — survives the parent terminal/session, no controlling TTY),
// stdout+stderr appended to outLog (so a bind failure or panic is inspectable,
// not discarded), and the env the broker needs to issue its cert + reach the
// jail. The pid file is written by the serve process itself (Task 1), not here.
func brokerServeCmd(self, configPath, outLog, aliasV4, runUser, runUID string) (*exec.Cmd, *os.File, error) {
	// On a fresh apply the state dir (.lever-state) does not exist yet — it's
	// created by EnsureKeys inside the spawned child, too late for this open —
	// so create the log's parent here or the whole bring-up hard-fails at
	// broker-up before the broker is ever spawned.
	if err := os.MkdirAll(filepath.Dir(outLog), 0o700); err != nil {
		return nil, nil, fmt.Errorf("broker out log dir: %w", err)
	}
	f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("broker out log: %w", err)
	}
	cmd := exec.Command(self, "broker", "serve", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(),
		"LEVER_HOST_ALIAS_IP="+aliasV4,
		"LEVER_JAIL_USER="+runUser,
		"LEVER_JAIL_UID="+runUID,
	)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd, f, nil
}

// remoteServeCmd builds the detached `lever remote serve <config>` command:
// its own session (Setsid — survives the parent terminal/session), stdout+
// stderr appended to outLog. Mirrors brokerServeCmd exactly (see its doc)
// minus the broker's jail-alias/run-user env: the proxy resolves its own jail
// identity lazily, on its first dial (jailPrefixFn in remote.go), so it needs
// nothing here beyond the parent's environment — which exec.Command inherits
// by default when Env is left nil, and which is where the jail transport
// binary is found on PATH.
func remoteServeCmd(self, configPath, outLog string) (*exec.Cmd, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(outLog), 0o700); err != nil {
		return nil, nil, fmt.Errorf("remote proxy out log dir: %w", err)
	}
	f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("remote proxy out log: %w", err)
	}
	cmd := exec.Command(self, "remote", "serve", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd, f, nil
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
	state      brokerctl.State
	configPath string
	port       int    // app.EffectiveRemotePort()
	version    string // this binary, for the reuse stamp
	cfgHash    string // brokerctl.RemoteConfigHash(app), for the reuse stamp
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
// pidfile+TCP-dial check `lever remote status` uses (remotePIDStatus +
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
	if _, found, alive := remotePIDStatus(rc.state.RemotePID()); found && alive {
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
		fmt.Fprintln(os.Stderr, "lever: the remote proxy predates this binary or its remote config changed — restarting it")
		if err := rc.state.StopRemoteProxy(); err != nil {
			return fmt.Errorf("restarting the remote proxy: %w", err)
		}
	}
	cmd, logf, err := remoteServeCmd(brokerSelfExe(), rc.configPath, rc.state.RemoteLog())
	if err != nil {
		return err
	}
	// Keep the log fd owned by the child; close our copy after Start.
	defer logf.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lever remote serve: %w", err)
	}
	if err := rc.awaitListening(cmd); err != nil {
		return err
	}
	// Stamp only once the proxy is proven listening, so a stamp never claims a
	// process that never served. Best-effort: a write failure costs the next
	// apply a redundant restart, never a stale process kept alive.
	if err := rc.state.WriteRemoteStamp(rc.version, rc.cfgHash); err != nil {
		fmt.Fprintf(os.Stderr, "lever: could not record the remote proxy stamp (%v) — the next apply will restart it\n", err)
	}
	return nil
}

// remoteProxyStartTimeout/Interval bound the wait for the spawned proxy to
// bind. Package vars so tests run fast. Generous against a loaded host, and
// still far below anything a human would notice: both listeners are host
// loopback binds that happen before the proxy touches the jail.
var (
	remoteProxyStartTimeout  = 5 * time.Second
	remoteProxyStartInterval = 50 * time.Millisecond
)

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
func (rc *remoteController) awaitListening(child *exec.Cmd) error {
	deadline := time.Now().Add(remoteProxyStartTimeout)
	for {
		if err := child.Process.Signal(syscall.Signal(0)); err != nil {
			// The child is gone; whatever may be listening is not it.
			break
		}
		if err := tcpDial(rc.addr()); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(remoteProxyStartInterval)
	}
	if tail := lastLogLine(rc.state.RemoteLog()); tail != "" {
		return fmt.Errorf("the remote proxy started but is not listening on %s: %s (see %s)",
			rc.addr(), tail, rc.state.RemoteLog())
	}
	return fmt.Errorf("the remote proxy started but is not listening on %s — see %s",
		rc.addr(), rc.state.RemoteLog())
}

// lastLogLine returns the final non-empty line of path, or "". Only the tail is
// read: a long-lived proxy's log can be large, and the failure that matters is
// always the last thing it wrote before exiting.
func lastLogLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	const tailBytes = 4 << 10
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-tailBytes, 0)
	buf := make([]byte, min(fi.Size()-off, tailBytes))
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

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
func removeJailFile(ctx context.Context, jr leverexec.Runner, jailPath string) error {
	if _, err := jr.Run(ctx, nil, "sh", "-c", removeJailFileScript, "_", jailPath); err != nil {
		return fmt.Errorf("removing jail file %s: %w", jailPath, err)
	}
	return nil
}

// jailProjectPath maps tree (the project ROOT) to its in-jail location.
// ensureControllerPAT only ever registers the project root (never a worker
// subtree), so this covers just the hostPath==tree case of
// internal/apply/run.go's jailPath — that helper is unexported in package
// apply and can't be imported here, and the general N-path mapping isn't
// needed for this one caller.
func jailProjectPath(tree, jailMount string) string {
	if jailMount == "" || tree == "" {
		return tree
	}
	return jailMount
}

// throwawayHubPort is the port ensureControllerPAT's throwaway dev-auth hub is
// reached on — a distinct port from the real hub (8080). lever runs scion in
// workstation (combined) mode, where the Hub API rides the web server's port and
// the standalone --port flag is IGNORED (verified live: `--port 48080` binds
// :8080); --web-port is what actually binds it (verified: `--web-port 48080`
// binds :48080). ServerStart emits --web-port, so the throwaway lands here,
// physically isolated from the real dev-auth-OFF hub the scion-server apply step
// starts on 8080 right after. The throwaway's dev-auth window is agent-free +
// jail-loopback only (the "agent-free window" — see the P3 plan).
const throwawayHubPort = 48080

// controllerPATScopes is the EXACT scope set the controller PAT is minted
// with. agent:message is deliberately omitted — the scion authz review found
// every interactive verb, message included, gates on agent:attach.
// project:update is required for the post-register scratchpad-shared-dir strip
// (scion#925): the shared-dirs REST endpoint gates on project ActionUpdate,
// and agent:manage does NOT expand to project:update.
var controllerPATScopes = []string{"agent:manage", "agent:attach", "project:read", "project:update"}

// remotePATScopes is the EXACT scope set the remote-access PAT is minted
// with: interactive (attach gates every interactive verb, message included)
// plus read/list — and nothing that can create, delete, or reconfigure.
// The remote proxy injects this token; it must never carry agent:manage,
// agent:create/delete, project:update, or any secret scope.
var remotePATScopes = []string{"agent:read", "agent:list", "project:read", "agent:attach"}

// ensureControllerPAT backs Deps.EnsureControllerPAT, the "bootstrap-token"
// apply step (internal/apply/run.go). It owns TWO mints that both need the
// same throwaway dev-auth window: the controller PAT that the real hub —
// started dev-auth-OFF by the scion-server step right after this one — is
// driven with, and (when remote access is configured on) the narrower
// remote-access PAT the remote proxy injects on the operator's behalf.
//
// Idempotent per token: a PAT already persisted in state short-circuits its
// own mint (survives `down`→`up`; clearStagedRuntimeState only wipes
// tree/.lever/*, see the P3 plan's Global Constraints). If NEITHER token is
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
func ensureControllerPAT(ctx context.Context, jr leverexec.Runner, state brokerctl.State, tree, jailMount string, remoteEnabled bool) error {
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
	// dev-auth-on admin server. ServerStop tolerates a not-running server.
	defer func() { _ = tw.ServerStop(ctx) }()
	if err := tw.ServerStart(ctx, scion.ServerOpts{WebPort: throwawayHubPort, DevAuth: true}); err != nil {
		return fmt.Errorf("bootstrap-token: throwaway server: %w", err)
	}

	jp := jailProjectPath(tree, jailMount)
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
		pat, err := tw.HubTokenCreate(ctx, jp, filepath.Base(jp), "lever-controller", controllerPATScopes)
		if err != nil {
			return fmt.Errorf("bootstrap-token: hub token create: %w", err)
		}
		if err := state.SaveControllerPAT(pat); err != nil {
			return fmt.Errorf("bootstrap-token: persisting controller PAT: %w", err)
		}
	}
	if needRemote {
		pat, err := tw.HubTokenCreate(ctx, jp, filepath.Base(jp), "lever-remote", remotePATScopes)
		if err != nil {
			return fmt.Errorf("bootstrap-token: remote token create: %w", err)
		}
		if err := state.SaveRemotePAT(pat); err != nil {
			return fmt.Errorf("bootstrap-token: persisting remote PAT: %w", err)
		}
	}
	if err := tw.ServerStop(ctx); err != nil {
		// Best-effort: the deferred ServerStop above retries at return, and a
		// live run against a scion build without `server stop` needs a
		// jail-pid-kill fallback instead (see ServerStop's doc comment) — a P4
		// live-validation item, not implemented here.
		_ = err
	}
	// Best-effort delete of scion's residual dev-token so it doesn't linger as an
	// open admin credential once the real dev-auth-OFF hub takes over. scion writes
	// it to <scionDir>/dev-token, default ~/.scion/dev-token (pkg/apiclient/devauth.go),
	// where ~ is the JAIL USER's home (here /home/stephen — NOT /home/scion, which
	// is the agent-container user). Resolve that home in-jail rather than hardcode
	// it, then remove through the guarded removeJailFile helper.
	if home, herr := jr.Run(ctx, nil, "sh", "-c", `printf %s "$HOME"`); herr == nil {
		if h := strings.TrimSpace(home.Stdout); h != "" {
			_ = removeJailFile(ctx, jr, h+"/.scion/dev-token")
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
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
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
			deps, _, _, err := buildApplyDeps(cmd.Context(), app, path, bf, cmd)
			if err != nil {
				return err
			}
			if err := apply.Run(cmd.Context(), app, deps); err != nil {
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
// this binary + this broker config, i.e. whether apply's M2 shortcut may keep
// it (#19). A broker predating the identity fields reports them empty —
// mismatch — so old brokers are always restarted rather than trusted.
func brokerReusable(got broker.EpochResponse, wantVersion, wantHash string) bool {
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
	app        *config.App     // EffectiveJailPort, ManagerCN, ConfigHash, Tree (Rearm)
	state      brokerctl.State // pid file, out log, CA cert, StopBroker
	configPath string          // passed to `lever broker serve`
	adminURL   string          // http://127.0.0.1:<admin port>
	aliasV4    string          // resolved host-alias IP (LEVER_HOST_ALIAS_IP for the child)
	brokerHost string          // host agents dial the broker by (IP if resolved, else hostname)
	runUser    string          // in-machine run user for the broker child
	runUID     string          // in-machine run uid for the broker child
}

// brokerSelfExe returns the executable Start re-execs as `broker serve`. It is a
// var so tests can point it at an inert command.
//
// This seam is load-bearing, not cosmetic. In production os.Args[0] is the
// lever binary and re-execing it is exactly right. Under `go test` os.Args[0]
// is the TEST BINARY, so a spawn becomes `<pkg>.test broker serve …` — and
// brokerServeCmd detaches the child with Setsid precisely so it outlives its
// parent. Nothing then reaps it: the processes survive the run, accumulate
// across runs, and re-spawn each other. A full suite run left 724 of them
// behind before this seam existed.
var brokerSelfExe = func() string { return os.Args[0] }

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
	if req, err := http.NewRequestWithContext(probeCtx, "GET", bc.adminURL+"/epoch", nil); err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			var er broker.EpochResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&er)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if decodeErr == nil && brokerReusable(er, versionString(), brokerctl.ConfigHash(bc.app)) {
					return nil // same binary + same broker config; keep the process + PID
				}
				fmt.Fprintf(os.Stderr, "lever: broker predates this binary or its tool config changed — restarting it (was %q)\n", er.Version)
				if err := bc.state.StopBroker(); err != nil {
					return fmt.Errorf("stopping the stale broker before restart: %w", err)
				}
			}
		}
	}
	cmd, logf, err := brokerServeCmd(brokerSelfExe(), bc.configPath, bc.state.OutLog(), bc.aliasV4, bc.runUser, bc.runUID)
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

// Healthy polls GET /epoch until 200 or a ~10s timeout.
func (bc *brokerController) Healthy(ctx context.Context) error {
	deadline := time.Now().Add(10 * time.Second)
	epochURL := bc.adminURL + "/epoch"
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", epochURL, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("broker did not become healthy within 10s (last err: %v)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Mint POSTs /bootstrap to obtain the one-time manager enrolment ticket, reads
// the CA PEM from the state dir, and returns the full BootstrapMaterial. A 403
// means the single-use latch was already consumed — surfaced as
// apply.ErrBootstrapLatched so the mint step tolerates it on an idempotent
// re-apply against the same broker process.
func (bc *brokerController) Mint(ctx context.Context) (apply.BootstrapMaterial, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", bc.adminURL+"/bootstrap", bytes.NewReader(nil))
	if err != nil {
		return apply.BootstrapMaterial{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return apply.BootstrapMaterial{}, apply.ErrBootstrapLatched
	}
	if resp.StatusCode != http.StatusOK {
		return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap returned %d", resp.StatusCode)
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap decode: %w", err)
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
	if err := bc.state.StopBroker(); err != nil {
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

// buildApplyDeps wires the live dependencies for apply.Run.
// It eagerly calls EnsureUp so the backend resolves the in-machine
// run-user and UID before the JailRunner and scion.Client are constructed.
// JailUp is therefore a no-op in the returned Deps — the jail is already
// confirmed up and the user/uid are known.
// configPath is the resolved config file path; it is passed to `lever broker
// serve` and used to locate the broker state dir.
// cmd is the invoking cobra command, used only to wire Deps.Log (a loud,
// user-facing progress line — see apply.Deps.Log); may be nil (e.g. tests
// that never exercise a Log-emitting path), in which case Log falls back to
// stderr.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory, cmd *cobra.Command) (apply.Deps, backend.Backend, *scion.Client, error) {
	machine := machineName(app.Name)
	b, err := bf(app.Backend, machine)
	if err != nil {
		return apply.Deps{}, nil, nil, err
	}
	cfg := backend.Config{
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
	// Bring the jail up now so we can resolve the run-user/uid for the JailRunner.
	if err := b.EnsureUp(ctx, cfg); err != nil {
		return apply.Deps{}, nil, nil, err
	}
	jr := b.JailRunner()

	// state must be built before sc: sc's HubTokenSource closes over it so
	// every verb issued through sc picks up the controller PAT live, once
	// ensureControllerPAT (wired into Deps.EnsureControllerPAT below) persists
	// it mid-apply (see scion.Options.HubTokenSource's doc: lazy, read at
	// call time, wins over a static HubToken).
	state := stateFor(configPath)
	sc := brokerctl.HostScionClient(jr, state, app.Scion.AgentRole)

	// The hub's session-cookie signing key: generated once, adopted verbatim
	// on every later apply, never rotated by lever (see EnsureSessionSecret).
	// Threaded into Deps so every hub start signs sessions with the same key.
	sessionSecret, err := state.EnsureSessionSecret()
	if err != nil {
		return apply.Deps{}, nil, nil, err
	}

	adminURL := fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort())

	// The jail's resolved host-alias IP (host.orb.internal as seen from the jail).
	// Agents dial the broker by this IP — under closed-internet egress DNS/53 is
	// dropped, so the hostname can't be resolved; the IP is already allowlisted and
	// the broker cert carries it as a SAN. Falls back to the hostname if unresolved.
	aliasV4 := b.HostAliasV4()
	brokerHost := b.HostToolAlias() // host.orb.internal (DNS) by default…
	if aliasV4 != "" {
		brokerHost = aliasV4 // …but prefer the resolved IP (no DNS needed)
	}

	// bc owns the broker lifecycle (start/health/mint) and the re-arm
	// recombination — see brokerController's doc. Built here, after
	// EnsureUp/HostAliasV4, so runUser/runUID/aliasV4 are already resolved.
	bc := &brokerController{
		app:        app,
		state:      state,
		configPath: configPath,
		adminURL:   adminURL,
		aliasV4:    aliasV4,
		brokerHost: brokerHost,
		runUser:    b.RunUser(),
		runUID:     b.RunUID(),
	}

	// rc owns the remote-proxy lifecycle (see remoteController's doc). It
	// only ever runs when app.RemoteEnabled() (the remote-proxy apply step
	// — see internal/apply/plan.go) or, in the disabled direction, when
	// Run's own converge-off reconciliation calls StopRemoteProxy — see
	// internal/apply/run.go.
	rc := &remoteController{
		state:      state,
		configPath: configPath,
		port:       app.EffectiveRemotePort(),
		version:    versionString(),
		cfgHash:    brokerctl.RemoteConfigHash(app),
	}

	return apply.Deps{
		// JailUp is a no-op: buildApplyDeps already brought the jail up
		// (idempotent; resolves user/uid). The apply executor's jail-up step
		// is thus a confirmed no-op here.
		JailUp: func(context.Context, *config.App) error { return nil },
		LoadImage: func(ctx context.Context, ref string) error {
			return b.LoadImage(ctx, ref)
		},
		// ImageLoaded skips a redundant image re-import when the jail already
		// holds the exact bytes (same image ID as the host) — see the Deps field
		// doc. Fail-open in the backend, so a check failure just loads.
		ImageLoaded: func(ctx context.Context, ref string) bool {
			return b.ImageLoaded(ctx, ref)
		},
		// PruneImages reclaims the dangling image a rebuilt tag orphans, after a
		// load. Best-effort (the apply step logs, never fails, on error).
		PruneImages: func(ctx context.Context) error {
			return b.PruneJailImages(ctx)
		},
		Scion:            sc,
		JailMount:        b.MountDest(),
		HubSessionSecret: sessionSecret,

		// RemoveJailFile removes a regular file at a jail-absolute path THROUGH
		// the jail runner, so the removal shares the jail's own filesystem view
		// with the `scion init` that follows it in the register step (see the
		// comment at the register-project case in
		// internal/apply/run.go for the VirtioFS unlink/init race this closes).
		// The guard leaves directories untouched and is a no-op if the path is
		// already absent, mirroring removeStaleMarker's host-side semantics.
		RemoveJailFile: func(ctx context.Context, jailPath string) error {
			return removeJailFile(ctx, jr, jailPath)
		},

		// EnsureControllerPAT backs the "bootstrap-token" apply step (see
		// ensureControllerPAT's doc above): mint the controller PAT the real,
		// dev-auth-off hub is driven with, and (when remote access is
		// configured on) the narrower remote-access PAT, in one agent-free
		// window.
		EnsureControllerPAT: func(ctx context.Context) error {
			return ensureControllerPAT(ctx, jr, state, app.Tree, b.MountDest(), app.RemoteEnabled())
		},

		// RemoveScionProjectConfigs clears any stale ~/.scion/project-configs
		// registration(s) for a workspace path before the register step re-inits
		// (see internal/apply/run.go's register-project case) —
		// keeps apply from accumulating a duplicate registration every run.
		RemoveScionProjectConfigs: func(ctx context.Context, wp string) error {
			return b.RemoveScionProjectConfigs(ctx, wp)
		},

		// ScionProjectRegistered observes whether the register-project
		// apply step (internal/apply/run.go) even needs to run its
		// destructive clean+init path — see RemoveScionProjectConfigs's comment
		// above for why that path exists; this is the idempotency gate that
		// decides whether to run it at all, so a re-apply stops orphaning a
		// resumable scion agent record.
		ScionProjectRegistered: func(ctx context.Context, wp string) (bool, error) {
			return b.ScionProjectRegistered(ctx, wp)
		},

		// StripProjectSharedDirs declines scion's default cross-agent
		// `scratchpad` mount for this project — see the Deps field doc for why
		// it exists and why the removal must go through the hub. The request
		// runs IN THE JAIL, like every other scion interaction: the hub binds
		// the jail's loopback, and the Lima template suppresses every
		// guest→host port forward on purpose, so a host-side call could not
		// reach it there at all. The PAT is read per call so a re-mint is
		// picked up.
		StripProjectSharedDirs: func(ctx context.Context, projectName string) error {
			hc := &hubapi.Client{T: hubJailTransport(jr, state)}
			return hc.StripSharedDir(ctx, projectName, scion.DefaultHubEndpoint, scionScratchpadSharedDir)
		},

		// RepairScionHubEndpoint puts the project's recorded hub endpoint back to
		// the real hub after a controller-PAT re-mint linked it at the throwaway
		// one — see the Deps field doc.
		RepairScionHubEndpoint: func(ctx context.Context, wp string) error {
			return b.RepairScionHubEndpoint(ctx, wp, scion.DefaultHubEndpoint)
		},

		// VerifyAgentRole refuses to keep an agent whose hub record predates
		// scion's roles — see the Deps field doc for why that is a promotion to
		// full hub authority rather than a cosmetic gap. It fails CLOSED on
		// every question it cannot answer: not knowing whether the installed
		// scion has roles, or not being able to read the record, is exactly the
		// state in which guessing hands out authority.
		VerifyAgentRole: func(ctx context.Context, projectName, agentName string) error {
			return hubapi.VerifyAgentRole(ctx, sc.RolesSupported,
				&hubapi.Client{T: hubJailTransport(jr, state)}, projectName, agentName)
		},

		StartBroker:          bc.Start,
		BrokerHealthy:        bc.Healthy,
		MintManagerBootstrap: bc.Mint,

		// WaitBrokerReady gates start-manager on the scion runtime broker being
		// registered + online (see the Deps field doc): the broker registers
		// asynchronously after the Hub API, so the first create/resume would
		// otherwise race it. Fail-soft in the client, so it never fails bring-up.
		WaitBrokerReady: func(ctx context.Context, project string) error {
			return sc.WaitRuntimeBrokerReady(ctx, project)
		},

		// RearmBootstrap backs Deps.RearmBootstrap — see brokerController.Rearm
		// for the full rationale (re-arm the single-use latch on the create path,
		// staging the result because only the controller has app.Tree in scope).
		RearmBootstrap: bc.Rearm,

		// StartRemoteProxy/StopRemoteProxy back the remote-proxy apply step
		// (present only when app.RemoteEnabled() — see plan.go) and Run's
		// own converge-to-off reconciliation when it's false (see run.go).
		// StopRemoteProxy goes straight to brokerctl.State — no controller
		// method needed, since teardown-by-pidfile carries no lifecycle
		// state of its own (unlike Start's reuse probe).
		StartRemoteProxy: rc.Start,
		StopRemoteProxy:  func(context.Context) error { return state.StopRemoteProxy() },

		// EnsureHubLogin provisions the guest half of the remote login path
		// (see apply.Deps.EnsureHubLogin). It is a no-op with remote access
		// off: no provider runs host-side then, so a forwarder would point at
		// nothing and an oidc_login block would advertise a login that cannot
		// complete. The guest reaches the host at the same alias the agents
		// already use for the broker.
		EnsureHubLogin: func(ctx context.Context) (bool, error) {
			if !app.RemoteEnabled() {
				return false, nil
			}
			return b.EnsureHubLogin(ctx, backend.HubLogin{
				IssuerPort:  backend.GuestLoginIssuerPort,
				HostPort:    app.EffectiveRemoteLoginPort(),
				HostAddress: brokerHost,
				ClientID:    remoteproxy.LoginClientID,
			})
		},

		// DisableHubLogin removes the guest-side bridge when remote access is
		// off — see apply.Deps.DisableHubLogin for why leaving it running is
		// the part that matters.
		DisableHubLogin: b.DisableHubLogin,

		// EnsureAgentTemplate puts lever's overlay template in front of scion's
		// stock `default` — see apply.Deps.EnsureAgentTemplate, and
		// guest.EnsureLeverTemplate for WHY an empty system prompt is the fix.
		//
		// The two halves are ordered file-then-setting deliberately. Pointing
		// default_template at a template that does not exist yet would fail
		// every provision in between; the reverse order is inert, because a
		// template nothing selects is just an unused directory. So a failure
		// after the first half leaves the guest in a working state, and the
		// next apply converges it.
		//
		// The setting is read before it is written so an operator who has
		// deliberately chosen their own template keeps it: lever only claims
		// default_template while it is still scion's own default. Reading it
		// also keeps a re-apply quiet, since `config set` cannot report whether
		// it changed anything.
		EnsureAgentTemplate: func(ctx context.Context, projectDir string) (bool, error) {
			wrote, err := b.EnsureLeverTemplate(ctx)
			if err != nil {
				return false, err
			}
			cur, err := sc.ConfigGetProject(ctx, projectDir, "default_template")
			if err != nil {
				return false, fmt.Errorf("read default_template: %w", err)
			}
			if !leverMayClaimTemplate(cur) {
				return wrote, nil
			}
			if err := sc.ConfigSetProject(ctx, projectDir, "default_template", guest.LeverTemplateName); err != nil {
				return false, fmt.Errorf("set default_template: %w", err)
			}
			return true, nil
		},

		// Log surfaces start-manager's loud resume-failed recovery notice (see
		// apply.Deps.Log) on the invoking command's stderr, mirroring how other
		// user-facing warnings already surface (cmd.PrintErrf; see cli/stop.go,
		// cli/down.go). A nil cmd (defence in depth for any caller that doesn't
		// have one, e.g. a future direct test) falls back to os.Stderr so the
		// line is never silently lost.
		Log: func(format string, args ...any) {
			if cmd != nil {
				cmd.PrintErrf(format+"\n", args...)
				return
			}
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}, b, sc, nil
}
