package scion

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/retry"
)

// AlreadyRunning reports whether err is a scion "already running" error — used to
// make bring-up steps idempotent on re-apply (the server/agent is already up).
func AlreadyRunning(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "already running") || strings.Contains(s, "already exists")
}

// notRunning reports whether err is scion refusing to act on the SERVER daemon
// because it is not running — `scion server stop` with nothing to stop
// (cmd/server_daemon.go: "server daemon is not running").
//
// A predicate of its own rather than another arm of AlreadyRunning, because
// AlreadyRunning also guards ServerStart, where "not running" must stay a real
// failure: a start that reports the daemon is not running has not started it.
//
// It matches the full "server daemon is not running", not a bare "not running"
// or even "daemon is not running". Both looser forms swallow messages about
// OTHER things: scion says "agent 'x' is not running (phase: …)" at the agent
// level, and "broker daemon is not running" for the runtime broker
// (cmd/broker.go), so a future caller wrapping `runtime-broker start` with
// this would get exactly the silent success this predicate exists to prevent.
func notRunning(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "server daemon is not running")
}

// IsBrokerUnavailable reports whether err is the "runtime broker not yet
// registered" condition during bring-up (the registration race), as opposed
// to a real failure. The scion workstation daemon starts its Hub API and its
// runtime broker separately: the hub serves before the runtime broker has
// registered ASYNCHRONOUSLY, so a call issued in that window fails. scion
// words it three ways depending on the verb and how far the call got before
// giving up:
//   - `scion start`: plural "No runtime brokers available".
//   - `scion resume`: singular "no runtime broker available" — the SAME race
//     (confirmed in scion pkg/hub/handlers_agent_create_helpers.go:354,408).
//   - either, on a cold VM: "deadline exceeded" — scion's own internal wait for
//     the broker times out and surfaces the hub timeout instead of the clean
//     message (observed live: "context deadline exceeded from the Hub during
//     start-manager", which needed a second `up` to reconcile).
//
// All must be matched or a retry never sees its own transient error as
// retryable. A caller that retries on it must check its OWN ctx between
// attempts, since a deadline from that ctx (a genuine timeout, not scion's
// internal one) matches too.
func IsBrokerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no_runtime_broker") ||
		strings.Contains(s, "No runtime brokers available") ||
		strings.Contains(s, "no runtime broker available") ||
		strings.Contains(s, "deadline exceeded")
}

// IsAgentAbsent reports whether err from a scion agent verb (`list`, `status`,
// `resume`…) DEFINITIVELY means the named agent cannot be running — as opposed
// to an unknown failure a caller must not paper over. It matches, case-
// insensitively:
//   - "is not responding" / "connection refused": no hub is reachable on the
//     machine — the hub is only started by apply's scion-server step, so
//     before the first apply nothing can be running;
//   - "project not found": the hub is up but the project was never
//     hub-registered (e.g. a partial prior bring-up where local `scion init`
//     ran but `scion hub link` didn't) — no agent can be running under a
//     project the hub doesn't know, and apply's register-project step (init +
//     hub link) is exactly the repair;
//   - "no git origin remote found": scion's documented fallback when the path
//     isn't a locally registered project at all (no ~/.scion/project-configs
//     entry — forced project resolution falls back to git; see the
//     waitHubReady comment documenting this exact string). Lever projects are
//     directory projects, never git-resolved, so for lever this can only mean
//     "not registered" — again definitively absent, and register-project is
//     the repair.
func IsAgentAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not responding") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "project not found") ||
		strings.Contains(msg, "no git origin remote found")
}

// hubReadyAttempts/hubReadyInterval are the default waitHubReady budget.
const (
	hubReadyAttempts = 30
	hubReadyInterval = 1 * time.Second
)

// waitHubReady polls a lightweight, PROJECT-INDEPENDENT hub call until it
// succeeds or attempts run out. `list --all` lists agents across all projects
// and hits the hub without resolving a current project — unlike `list --global`,
// which forces project resolution and fails with "no git origin remote found"
// when run (as here) before any project is registered (verified live 2026-06-17).
func (c *Client) waitHubReady(ctx context.Context) error {
	var lastErr error
	err := retry.Until(ctx, c.hubReadyAttempts, c.hubReadyInterval, func() (bool, error) {
		_, lastErr = c.run(ctx, "", "list", "--all", "--format", "json")
		return lastErr == nil, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return fmt.Errorf("hub not ready after %d attempts: %w", c.hubReadyAttempts, lastErr)
	}
	return err
}

// brokerReadyAttempts/brokerReadyInterval are the default WaitRuntimeBrokerReady
// budget.
const (
	brokerReadyAttempts = 30
	brokerReadyInterval = 1 * time.Second
)

// runtimeBroker is the subset of a `scion hub brokers --format json` row we read
// to judge readiness. A broker registers with the hub before it finishes
// connecting, so "a row exists" is not enough — we wait for one that is actually
// online/connected.
type runtimeBroker struct {
	Status          string `json:"status"`
	ConnectionState string `json:"connectionState"`
}

func (b runtimeBroker) ready() bool {
	return b.Status == "online" || b.ConnectionState == "connected"
}

// WaitRuntimeBrokerReady blocks until the hub reports at least one ONLINE
// runtime broker, or the attempt budget is exhausted. The scion workstation
// daemon brings up its Hub API and its runtime broker separately: waitHubReady
// (called from ServerStart) confirms the Hub API serves, but the runtime broker
// registers AND connects asynchronously afterward — and `scion start`/`resume`
// need it. Gating here closes that window at the source, so the create/resume
// that follows acts against a ready broker instead of racing it (which
// otherwise fails the first `up` and needs a second, or leans on the start
// path's broker-unavailable retry).
//
// FAIL-SOFT: on budget exhaustion it returns nil (not an error), so the caller
// proceeds to start regardless — the start path's own bounded broker-unavailable
// retry (internal/apply's retryOnBrokerUnavailable, gated on
// IsBrokerUnavailable) is the backstop, and hard-failing the whole bring-up on
// a readiness probe that can't confirm would be worse than letting start try. Only ctx cancellation returns an error. `hub brokers` lists
// brokers hub-wide; project scopes only the hub-client/settings resolution, so
// it is passed to dodge the "no project" resolution failure a bare call hits.
func (c *Client) WaitRuntimeBrokerReady(ctx context.Context, project string) error {
	args := append([]string{"hub", "brokers", "--format", "json"}, projectFlag(project)...)
	err := retry.Until(ctx, c.brokerReadyAttempts, c.brokerReadyInterval, func() (bool, error) {
		out, err := c.run(ctx, "", args...)
		if err != nil {
			return false, nil
		}
		// parseJSON (not raw Unmarshal): scion prints the dev-auth WARNING
		// banner into the same stream, and parseJSON strips it + ANSI before
		// decoding — matching List/messaging. A parse miss leaves brokers empty
		// (not ready), so it stays fail-soft.
		var brokers []runtimeBroker
		if parseJSON(out, &brokers) != nil {
			return false, nil
		}
		return slices.ContainsFunc(brokers, runtimeBroker.ready), nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return nil
	}
	return err
}

// InitMachine seeds the machine-level scion dir + default harness configs
// (claude/gemini). Required before `--harness claude` resolves.
func (c *Client) InitMachine(ctx context.Context) error {
	_, err := c.run(ctx, "", "init", "--machine", "--non-interactive")
	return err
}

// ConfigSetGlobal sets a global scion config key (e.g. image_registry=scionlocal).
func (c *Client) ConfigSetGlobal(ctx context.Context, key, value string) error {
	_, err := c.run(ctx, "", "config", "set", "--global", key, value)
	return err
}

// ConfigGetProject reads an effective scion config key as resolved FOR a
// project — the merge of embedded defaults, the machine settings, and the
// project's own, in that precedence.
func (c *Client) ConfigGetProject(ctx context.Context, projectDir, key string) (string, error) {
	// runValue, not run: the result is compared against an expected value, and
	// scion's settings loader writes warnings to stderr. See runValue.
	return c.runValue(ctx, projectDir, "config", "get", key)
}

// ConfigSetProject sets a scion config key at PROJECT scope, resolved from
// projectDir as the working directory.
//
// Project scope, not global, because it is the only one that wins: settings are
// merged with koanf in the order embedded defaults → machine
// (~/.scion/settings.yaml) → project, so a later file overrides an earlier one
// (pkg/config/settings_v1.go LoadVersionedSettings). Scion writes
// `default_template: default` into the project's own settings at registration,
// which would defeat the same key set globally.
func (c *Client) ConfigSetProject(ctx context.Context, projectDir, key, value string) error {
	_, err := c.run(ctx, projectDir, "config", "set", key, value)
	return err
}

// ServerOpts configures `scion server start`.
type ServerOpts struct {
	// WebPort, when > 0, sets the port the Hub API is reachable on (--web-port).
	// lever runs scion in workstation/combined mode, where the Hub API is mounted
	// on the web server's port and the standalone --port flag is IGNORED (verified
	// live — a `--port 48080` start binds :8080). So --web-port is what actually
	// controls the Hub API port. Zero lets scion pick its default (8080).
	WebPort int
	// DevAuth is always emitted explicitly (--dev-auth=true|false) so the real
	// hub is never left on the (dev-auth-on) default by omission.
	DevAuth bool
	// EnableWeb passes --enable-web. lever sets it only for remote access —
	// but it does NOT follow that a headless hub stays API-only, and reading
	// it that way is a mistake with teeth.
	//
	// scion applies WORKSTATION DEFAULTS to every `server start` that is not
	// --hosted, and one of them is `if !cmd.Flags().Changed("enable-web") {
	// enableWeb = true }` (cmd/server_config.go applyWorkstationDefaults).
	// Omitting the flag therefore enables the web frontend exactly as passing
	// it does — the daemon even re-emits a bare --enable-web into its own
	// --foreground child argv (cmd/server_daemon.go buildDaemonStartArgs runs
	// AFTER the defaults are applied). Only an explicit --enable-web=false
	// would turn it off.
	//
	// lever must never pass that. With web disabled the Hub API stops being
	// mounted on the web server and binds cfg.Hub.Port — 9810 — instead
	// (cmd/server_foreground.go, the `if !enableWeb` branch), while lever's
	// whole model puts the Hub API on --web-port 8080: the broker, the agents'
	// SCION_HUB_ENDPOINT, `lever doctor` and the remote proxy all dial it
	// there. WebPort's "the Hub API is mounted on the web server's port" is
	// true only because the web frontend is on.
	//
	// So what this field actually decides is WebAssetsDir below, which is only
	// ever passed alongside it. Turning remote access off does not take the
	// SPA away; it takes away the assets lever staged for it, and the login.
	//
	// There is deliberately NO --base-url here, even though the remote
	// feature knows its public tailnet origin. scion's --base-url is not a
	// web-only setting: it also becomes the hub's agent-facing endpoint
	// (cmd/server_foreground.go resolveHubEndpoint), which the hub injects
	// into every agent container as SCION_HUB_ENDPOINT/SCION_HUB_URL. A jail
	// agent cannot reach a tailnet name (no DNS for it, and lever's egress
	// drops 100.64/10), and — worse — a non-loopback endpoint SKIPS scion's
	// container-bridge rewrite (pkg/runtimebroker/hubenv.go
	// applyContainerBridgeOverride), the very step that turns the hub's
	// loopback address into the host.containers.internal form lever's pasta
	// --map-host-loopback makes reachable. Leaving the flag off keeps the
	// resolved endpoint on localhost, so the rewrite applies and agents keep
	// their status/notification/token-refresh callbacks. The SPA does not
	// need it: it builds every URL relative (no baseURL anywhere in scion's
	// web client), and lever's proxy strips the hub's session cookie and
	// never exercises the OAuth redirect path, which are --base-url's only
	// other consumers.
	EnableWeb bool
	// WebAssetsDir, when non-empty, serves the SPA from this GUEST directory
	// (--web-assets-dir) instead of the binary's embedded filesystem.
	//
	// It exists because a `scion.version:` pin has no embedded SPA at all: the
	// upstream repo tracks only web/dist/client/.gitkeep and .gitignores the
	// built output, so a binary compiled from the fetched module serves scion's
	// "Web UI Not Available" page. lever builds the assets host-side from that
	// same module and stages them into the guest, and this flag points the hub
	// at them (layout.WebAssetsDir).
	//
	// Only ever set together with EnableWeb, and only when lever actually staged
	// the assets — config.App.ScionWebAssets decides both. Setting it otherwise
	// would be worse than leaving it off: scion takes a non-empty value as an
	// override and serves that directory whether or not it has anything in it,
	// so a wrong path REPLACES working embedded assets with 404s rather than
	// falling back to them.
	WebAssetsDir string
	// SessionSecret, when non-empty, is the hub's session-cookie signing key
	// (--session-secret). Without it scion generates a random key per boot and
	// every hub restart signs every browser out of the web UI. The flag, not
	// env SCION_SERVER_SESSION_SECRET, because the flag is the durable channel:
	// scion's daemon persists its argv to ~/.scion/server-args.json and
	// `scion server restart` replays it verbatim, while env would be lost.
	// Like every argv-only option it applies at the next START (see below).
	// Empty omits the flag (scion's per-boot random key) — the throwaway
	// mint-window hub, whose sessions nobody keeps.
	SessionSecret string
}

// ServerStart starts the workstation daemon (Hub API + broker); it daemonises
// and returns.
//
// These options describe a START, not a desired state. A daemon that is
// already running keeps the argv it was started with: scion refuses with
// "server is already running (PID: n)" rather than reconfiguring anything
// (cmd/server_daemon.go runServerStartOrDaemon), and this call tolerates that
// refusal so a re-apply is cheap (see below). So an option that lives only in
// the argv — WebAssetsDir, SessionSecret — changes nothing until something
// stops the daemon first. A caller that needs one to take effect must
// ServerStop, and internal/apply does exactly that, on both the on- and the
// off-transition of remote access.
func (c *Client) ServerStart(ctx context.Context, o ServerOpts) error {
	args := []string{"server", "start"}
	if o.WebPort > 0 {
		args = append(args, "--web-port", strconv.Itoa(o.WebPort))
	}
	args = append(args, fmt.Sprintf("--dev-auth=%t", o.DevAuth))
	// Equals form, like --web-assets-dir below, so the argv matches scion's
	// own daemon re-exec form.
	if o.SessionSecret != "" {
		args = append(args, "--session-secret="+o.SessionSecret)
	}
	if o.EnableWeb {
		args = append(args, "--enable-web")
		// Equals form: it is how scion's own daemon re-emits the flag when it
		// re-execs itself into the background (cmd/server_daemon.go), so the
		// argv lever writes matches the argv scion writes for itself.
		if o.WebAssetsDir != "" {
			args = append(args, "--web-assets-dir="+o.WebAssetsDir)
		}
	}
	// Idempotent: tolerate an already-running server on re-apply; waitHubReady
	// then confirms the existing server is actually serving.
	//
	// runSecret, not run: the argv carries the session secret, and run's error
	// path renders argv verbatim (redactArgs only knows the hub-secret-set
	// shapes). The scrub is textual, so AlreadyRunning still matches.
	if _, err := c.runSecret(ctx, "", o.SessionSecret, args...); err != nil && !AlreadyRunning(err) {
		return err
	}
	return c.waitHubReady(ctx)
}

// ServerStop stops the workstation daemon, tolerating a daemon that is not
// running so callers can call it unconditionally — during teardown, and as the
// stop half of a restart.
//
// That tolerance is notRunning's, NOT AlreadyRunning's. This comment used to
// claim AlreadyRunning covered scion's not-running wording; it does not, and a
// live apply proved it: `scion server stop` against a stopped daemon exits
// non-zero with "server daemon is not running", which contains neither
// "already running" nor "already exists", so the error propagated and failed
// the whole apply. Do not fold the two predicates back together — see
// notRunning for why.
//
// NOTE (live): if the pinned scion build lacks `server stop`, callers should
// fall back to a jail-process kill; this stays the seam either way.
func (c *Client) ServerStop(ctx context.Context) error {
	if _, err := c.run(ctx, "", "server", "stop"); err != nil && !AlreadyRunning(err) && !notRunning(err) {
		return err
	}
	return nil
}

// SecretSet stores a Hub secret that is ALWAYS projected into the agent
// container. It goes through `hub env set --secret`, not `hub secret set`:
// both write the same secret row (scion's cmd/hub_env.go redirects --secret to
// the Secret API with type=environment, target=key), but only the env form can
// set the injection mode. scion normalises an unset mode to as_needed, and
// since scion #944 (221d2eaf, 2026-08-01) an as_needed value is filtered out of
// the projected container env — so the secret would sit in the Hub and never
// reach the agent. The flags exist from 5f56069e (2026-02-11), well below
// lever's minimum supported pin.
//
// The value goes over in PLAINTEXT. scion's secret API base64-decodes a value
// by default, but since ce96122c (#1111, 2026-08-10) the CLI stamps
// encoding=raw on every non-@file value, so the argument is stored verbatim.
// Encoding it here would store the base64 TEXT and hand the agent a corrupt
// credential — which is why this needs a pin of ce96122c or later. An older
// pin fails loudly at apply, and errBase64Pin explains it.
func (c *Client) SecretSet(ctx context.Context, key, value string) error {
	_, err := c.runSecret(ctx, "", value, "hub", "env", "set", "--secret", "--always", key, value)
	return secretSetErr(key, err)
}

// secretSetErr reports the cause rather than the symptom: the hub rejects a
// plaintext value only on a pin that predates the CLI's encoding=raw flip.
func secretSetErr(key string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "value must be base64-encoded") {
		return fmt.Errorf("%w (setting %s): %v", errBase64Pin, key, err)
	}
	return err
}

// errBase64Pin names the pin floor rather than the symptom: the hub rejects a
// plaintext secret only on a scion older than ce96122c, where the CLI did not
// yet mark values raw.
var errBase64Pin = errors.New(
	"scion rejected a plaintext secret value: this scion pin predates ce96122c " +
		"(#1111, 2026-08-10) — raise scion.source/binary to that commit or later, " +
		"or scion.version to dbf52f22 or later (commits between 4c045fc8 and " +
		"dbf52f22 cannot be fetched through the Go module proxy at all)")

// EnvSet sets a NON-secret Hub env var scoped to one agent's project. Unlike
// SecretSet (encrypted, user-scoped), this is a plain value scoped to the agent
// by running `hub env set --project` with the agent's project dir as cwd (bare
// --project infers the project from the working directory), so it does not leak
// to other agents in the instance. Used to convey LEVER_LLM_AUTH=api-key so an
// agent's pre-start hook enters api-key mode.
//
// --always is load-bearing for the same reason as in SecretSet: without it the
// variable is stored as_needed and never projected. LEVER_LLM_AUTH is not a
// key any harness declares as required, so scion's env-gather second pass never
// asks for it either — as_needed here means "never delivered".
//
// projectDir must be a registered project's dir (run after register-project /
// InitProject).
func (c *Client) EnvSet(ctx context.Context, projectDir, key, value string) error {
	_, err := c.run(ctx, projectDir, "hub", "env", "set", "--project", "--always", key+"="+value)
	return err
}
