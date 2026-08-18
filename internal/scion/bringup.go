package scion

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// hubReadyAttempts/hubReadyInterval are package vars so tests can shrink them.
var hubReadyAttempts = 30
var hubReadyInterval = 1 * time.Second

// waitHubReady polls a lightweight, PROJECT-INDEPENDENT hub call until it
// succeeds or attempts run out. `list --all` lists agents across all projects
// and hits the hub without resolving a current project — unlike `list --global`,
// which forces project resolution and fails with "no git origin remote found"
// when run (as here) before any project is registered (verified live 2026-06-17).
func (c *Client) waitHubReady(ctx context.Context) error {
	var lastErr error
	for i := 0; i < hubReadyAttempts; i++ {
		if _, err := c.run(ctx, "", "list", "--all", "--format", "json"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hubReadyInterval):
		}
	}
	return fmt.Errorf("hub not ready after %d attempts: %w", hubReadyAttempts, lastErr)
}

// brokerReadyAttempts/brokerReadyInterval bound WaitRuntimeBrokerReady; package
// vars so tests can shrink them.
var brokerReadyAttempts = 30
var brokerReadyInterval = 1 * time.Second

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
// retry (internal/apply's isBrokerUnavailable) is the backstop, and hard-failing
// the whole bring-up on a readiness probe that can't confirm would be worse than
// letting start try. Only ctx cancellation returns an error. `hub brokers` lists
// brokers hub-wide; project scopes only the hub-client/settings resolution, so
// it is passed to dodge the "no project" resolution failure a bare call hits.
func (c *Client) WaitRuntimeBrokerReady(ctx context.Context, project string) error {
	args := append([]string{"hub", "brokers", "--format", "json"}, projectFlag(project)...)
	for i := 0; i < brokerReadyAttempts; i++ {
		if out, err := c.run(ctx, "", args...); err == nil {
			// parseJSON (not raw Unmarshal): scion prints the dev-auth WARNING
			// banner into the same stream, and parseJSON strips it + ANSI before
			// decoding — matching List/messaging. A parse miss leaves brokers empty
			// (not ready), so it stays fail-soft.
			var brokers []runtimeBroker
			if parseJSON(out, &brokers) == nil {
				for _, b := range brokers {
					if b.ready() {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(brokerReadyInterval):
		}
	}
	return nil
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
	// EnableWeb serves the hub's embedded SPA (--enable-web). Only the
	// remote-access feature turns this on; a headless hub stays API-only.
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
	// at them (internal/backend/guest.ScionWebAssetsDir).
	//
	// Only ever set together with EnableWeb, and only when lever actually staged
	// the assets — config.App.ScionWebAssets decides both. Setting it otherwise
	// would be worse than leaving it off: scion takes a non-empty value as an
	// override and serves that directory whether or not it has anything in it,
	// so a wrong path REPLACES working embedded assets with 404s rather than
	// falling back to them.
	WebAssetsDir string
}

// ServerStart starts the workstation daemon (Hub API + broker); it daemonises
// and returns.
func (c *Client) ServerStart(ctx context.Context, o ServerOpts) error {
	args := []string{"server", "start"}
	if o.WebPort > 0 {
		args = append(args, "--web-port", strconv.Itoa(o.WebPort))
	}
	args = append(args, fmt.Sprintf("--dev-auth=%t", o.DevAuth))
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
	if _, err := c.run(ctx, "", args...); err != nil && !AlreadyRunning(err) {
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
