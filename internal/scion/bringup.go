package scion

import (
	"context"
	"encoding/base64"
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
}

// ServerStart starts the workstation daemon (Hub API + broker); it daemonises
// and returns.
func (c *Client) ServerStart(ctx context.Context, o ServerOpts) error {
	args := []string{"server", "start"}
	if o.WebPort > 0 {
		args = append(args, "--web-port", strconv.Itoa(o.WebPort))
	}
	args = append(args, fmt.Sprintf("--dev-auth=%t", o.DevAuth))
	// Idempotent: tolerate an already-running server on re-apply; waitHubReady
	// then confirms the existing server is actually serving.
	if _, err := c.run(ctx, "", args...); err != nil && !AlreadyRunning(err) {
		return err
	}
	return c.waitHubReady(ctx)
}

// ServerStop stops the workstation daemon. Tolerates the "not running" case
// (AlreadyRunning's message set also covers scion's not-running wording) so
// callers can call it unconditionally during teardown. NOTE (live): if the
// pinned scion build lacks `server stop`, callers should fall back to a
// jail-process kill; this stays the seam either way.
func (c *Client) ServerStop(ctx context.Context) error {
	if _, err := c.run(ctx, "", "server", "stop"); err != nil && !AlreadyRunning(err) {
		return err
	}
	return nil
}

// SecretSet stores a Hub secret. scion (>= da49e14) requires the value to be
// base64-encoded on input ("value must be base64-encoded", HTTP 400 otherwise)
// and decodes it for projection to the agent. We pass the raw secret and encode
// here. (Earlier vendored scion took the raw value verbatim — encoding is
// version-specific; verified against da49e14 on 2026-06-17.)
func (c *Client) SecretSet(ctx context.Context, key, value string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	_, err := c.run(ctx, "", "hub", "secret", "set", key, enc)
	return err
}

// EnvSet sets a NON-secret Hub env var scoped to one agent's project. Unlike
// SecretSet (encrypted, user-scoped), this is a plain value scoped to the agent
// by running `hub env set --project` with the agent's project dir as cwd (bare
// --project infers the project from the working directory), so it does not leak
// to other agents in the instance. Used to convey LEVER_LLM_AUTH=api-key so an
// agent's pre-start hook enters api-key mode (scion projects Hub env into the
// container before pre-start hooks run, so the hook sees it). projectDir must be
// a registered project's dir (run after register-project / InitProject).
func (c *Client) EnvSet(ctx context.Context, projectDir, key, value string) error {
	_, err := c.run(ctx, projectDir, "hub", "env", "set", "--project", key+"="+value)
	return err
}
