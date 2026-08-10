package scion

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ContainerLive reports whether a scion `list --format json` containerStatus
// value describes a LIVE container. For a running container scion passes
// through the podman status TEXT ("Up 6 seconds", "Up About a minute"), not a
// canonical token — live-observed 2026-07-04 when a liveness gate wrongly
// failed a healthy manager by comparing == "running". Non-live values seen:
// "stopped", "Exited (1) 4 minutes ago". Shared by apply's waitManagerLive and
// the broker's waitWorkerLive so both consumers use one predicate.
func ContainerLive(status string) bool {
	return status == "running" || strings.HasPrefix(status, "Up")
}

// WaitAgentLive polls list until the agent named slug shows BOTH Phase=="running"
// AND a live container (ContainerLive), or attempts run out. It is the shared
// post-action liveness backstop for scion's false-success classes: a blind
// `scion start`'s 409 "already exists" (exit code is non-zero but its text can
// match an idempotency predicate a layer up) and a `scion resume`/`scion start`
// that reports success ("resumed") for a container whose harness dies moments
// later (scion's own liveness check is a single immediate poll). Trusting the
// observed record — not CLI exit codes or error wording — is what makes a
// caller's success meaningful.
//
// A mid-poll list error does NOT mean the agent isn't live: by the time a caller
// reaches this poll its observe-first List and the create/resume action have
// already succeeded, so the hub is demonstrably up and a single error here is
// far more likely a transient hiccup. The failed attempt is consumed within the
// SAME budget and polling continues; the error is surfaced only if the whole
// budget exhausts without ever observing a live record.
//
// On exhaustion it returns a "did not come up" error (no subject prefix — the
// caller wraps it with its own, e.g. `start-manager: manager %q %w` or
// `worker %q %w`) reporting the last observed phase/container, or the last list
// error if the final attempts could not observe the record. On context
// cancellation it returns ctx.Err() unwrapped so callers can detect it.
func WaitAgentLive(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, attempts int, interval time.Duration) error {
	var lastPhase, lastContainer string
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		agents, err := list(ctx)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
			continue
		}
		lastErr = nil
		// Reset first so a record that vanished mid-poll reports "" (its true
		// last-observed state), not a stale earlier phase.
		lastPhase, lastContainer = "", ""
		if a := FindAgent(agents, slug); a != nil {
			lastPhase, lastContainer = a.Phase, a.ContainerStatus
		}
		if lastPhase == "running" && ContainerLive(lastContainer) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	if lastErr != nil {
		// %v, not %w: this is a terminal "budget exhausted" condition, and a
		// list error may itself carry an inner context deadline (an HTTP
		// timeout inside the scion client) even while OUR ctx stayed live.
		// Wrapping it would let a caller's errors.Is(err, context.DeadlineExceeded)
		// match here and misclassify exhaustion as cancellation (dropping the
		// caller's subject prefix). True cancellation of ctx returns ctx.Err()
		// via the select above, unwrapped.
		return fmt.Errorf("did not come up (last error observing agents: %v)", lastErr)
	}
	return fmt.Errorf("did not come up (last phase %q, container %q) — scion reported success but the harness is not live", lastPhase, lastContainer)
}

// FindAgent returns a pointer to the first agent whose Slug matches, or nil
// when no record matches. The returned pointer aliases the slice element, so
// callers read the live record (phase, container status) without copying.
func FindAgent(agents []Agent, slug string) *Agent {
	for i := range agents {
		if agents[i].Slug == slug {
			return &agents[i]
		}
	}
	return nil
}

type Agent struct {
	Slug     string `json:"slug"`
	Phase    string `json:"phase"`
	Activity string `json:"activity"`
	// ContainerStatus backstops apply's post-start liveness verification:
	// scion's own "resumed"/exit-0 can lie (live-proven 2026-07-04 — CLI exits
	// 0 on a 409 "already exists"; reports "resumed" on a container whose
	// harness dies moments later), so Phase alone is not trusted.
	ContainerStatus string `json:"containerStatus"`
}

// Phase values for Agent.Phase. These mirror upstream scion's agent-state wire
// vocabulary (pkg/agent/state/state.go); the values change only with a pin
// bump. scion's full enum also includes created/provisioning/cloning/starting/
// stopping, which no lever comparison site needs. Untyped string constants so
// they compare directly against the raw Agent.Phase field and switch on it.
const (
	PhaseRunning   = "running"
	PhaseSuspended = "suspended"
	PhaseStopped   = "stopped"
	PhaseError     = "error"
)


// agentRoleBaseline is the role lever wants for every agent: heartbeat and
// self-token-refresh, with no agent create/lifecycle or secret scope. Worker
// dispatch runs host-side under the controller PAT, never an agent's own token,
// so nothing lever does needs more. Used whenever the installed scion supports
// roles and the instance did not name one.
const agentRoleBaseline = "baseline"

// roleFlagSupported reports whether the installed scion accepts `start --role`
// (scion#1089). It asks the binary rather than the pin, because a commit hash
// says nothing about which features it carries — and getting this wrong in
// either direction is costly: too eager breaks pre-#1089 pins, too shy hands
// agents FULL authority on pins at or after scion#1090.
//
// Probed once per Client and cached: Start runs on every dispatch, and this
// answer cannot change under a running instance (the binary is installed at
// bring-up).
func (c *Client) roleFlagSupported(ctx context.Context) (bool, error) {
	c.roleProbe.Do(func() {
		out, err := c.run(ctx, "", "start", "--help")
		if err != nil {
			c.roleProbeErr = err
			return
		}
		c.roleSupported = strings.Contains(out, "--role")
	})
	return c.roleSupported, c.roleProbeErr
}

type StartOpts struct {
	Worker  string
	Task    string
	Harness string // default "claude"
	Project string
	Image   string // optional
	// Workspace is the path mounted as /workspace in the agent container,
	// passed as `--workspace`. For directory projects this MUST be set to the
	// (in-jail) project tree to get a live in-place bind mount: scion's default
	// resolution mounts a managed COPY of the externalized config dir instead
	// (verified 2026-06-16 — the explicit flag takes provision.go's Case-1
	// "mount this path directly" path). Empty leaves scion to resolve it.
	Workspace string
	// WorkspaceSubdir, when set, is a path RELATIVE to the project's workspace_path
	// (e.g. "workers/scratch"), passed as a relative `--workspace`. Scion resolves
	// a relative --workspace against the project root with a containment guard
	// (rejecting `..`/symlink escape) and mounts exactly that subtree at
	// /workspace — an absolute --workspace instead mounts that exact host path.
	// Used for workers so each is confined to its own subdir. Mutually exclusive
	// with Workspace (both emit the same flag), so a caller sets one or the
	// other. Requires scion >= b4c9911d (upstream PR #815, relative --workspace).
	WorkspaceSubdir string
	// APIKey selects api-key mode for the agent: scion starts the claude harness
	// with `--harness-auth api-key` (instead of `--harness-auth oauth-token`),
	// satisfied by a PLACEHOLDER ANTHROPIC_API_KEY the host sets as a Hub secret.
	// The placeholder is a sentinel, not a real credential: the agent's actual LLM
	// credential is the broker /llm capability token, written into the claude
	// settings.json as ANTHROPIC_AUTH_TOKEN by lever-agent boot. claude sends that
	// token as `Authorization: Bearer` AND the placeholder as `x-api-key`, both to
	// ANTHROPIC_BASE_URL (the broker /llm), which verifies the token and
	// overwrites x-api-key with the real key host-side (verified live 2026-06-28).
	// This placeholder is needed only because scion's start-time auth gate requires
	// some credential before the container — and thus lever-agent boot — can run.
	APIKey bool
}

// List lists the agents in project (directory-project `-g` scope), parsing
// the `scion list --format json` array. --non-interactive (implies --yes) so
// the lazy hub-sync prompt can never wedge a non-tty run (the sync itself is
// benign for observers: it only removes container-less stale records, which
// correctly read as absent). Empty/whitespace stdout parses as an empty
// slice, not an error (parseJSON no-ops on an empty body).
func (c *Client) List(ctx context.Context, project string) ([]Agent, error) {
	args := append([]string{"list", "--format", "json"}, projectFlag(project)...)
	args = append(args, "--non-interactive")
	out, err := c.run(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var agents []Agent
	if err := parseJSON(out, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// Delete removes an agent's record entirely (scion alias `rm`) — "containers
// and their associated files and worktrees" per scion's own help text. Used by
// start-manager's loud recovery path when Resume fails and the conversation
// cannot be restored, to clear the way for a fresh Start.
func (c *Client) Delete(ctx context.Context, worker, project string) error {
	args := append([]string{"delete", worker}, projectFlag(project)...)
	args = append(args, "--non-interactive")
	_, err := c.run(ctx, "", args...)
	return err
}

func (c *Client) Start(ctx context.Context, o StartOpts) error {
	harness := o.Harness
	if harness == "" {
		harness = "claude"
	}
	args := projectFlag(o.Project)
	args = append(args, "start", o.Worker, o.Task, "--harness", harness)
	if o.APIKey {
		// api-key: satisfy scion's start gate with the placeholder ANTHROPIC_API_KEY
		// (set as a Hub secret host-side); the real credential is the in-container
		// broker capability token. See StartOpts.APIKey.
		args = append(args, "--harness-auth", "api-key")
	} else {
		args = append(args, "--harness-auth", "oauth-token")
	}
	// Pin the agent role whenever the installed scion understands roles at all.
	//
	// This CANNOT be decided from config alone. `--role` does not exist before
	// scion#1089, so passing it unconditionally breaks every earlier pin — but
	// scion#1090 then flipped the default role from baseline to FULL, so NOT
	// passing it on a pin at or after that commit hands every agent
	// create/lifecycle/secret-read authority. An opaque commit hash tells lever
	// nothing about which side of that line a pin sits on, so it asks the
	// binary (see roleFlagSupported) instead of trusting the operator to know.
	//
	// Pre-#1089 scion has no roles at all, so omitting the flag there widens
	// nothing: agents get the old fixed scope set.
	supported, err := c.roleFlagSupported(ctx)
	switch {
	case err != nil:
		// Fail closed. This probe is a local exec of the binary we are about to
		// run; if it cannot answer, `start` was not going to work either, and
		// guessing risks silently granting full authority.
		return fmt.Errorf("determining scion agent-role support: %w", err)
	case supported:
		role := c.agentRole
		if role == "" {
			role = agentRoleBaseline
		}
		args = append(args, "--role", role)
	case c.agentRole != "":
		return fmt.Errorf("scion.agent_role is %q but this scion has no --role flag "+
			"(it predates scion#1089); remove the setting or move to a newer pin", c.agentRole)
	}
	if o.Image != "" {
		args = append(args, "--image", o.Image)
	}
	if o.WorkspaceSubdir != "" {
		// Relative to the project's workspace_path; scion mounts exactly this
		// subtree at /workspace (guarded). Same flag as Workspace — scion
		// branches on filepath.IsAbs.
		args = append(args, "--workspace", o.WorkspaceSubdir)
	} else if o.Workspace != "" {
		args = append(args, "--workspace", o.Workspace)
	}
	_, runErr := c.run(ctx, "", args...)
	return runErr
}

func (c *Client) Resume(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", append([]string{"resume", worker}, projectFlag(project)...)...)
	return err
}

// ResumeForce is Resume with scion's --force (scion#895, pin >= 68507153):
// the only verb that recovers a record from phase "error" (plain resume is
// documented for suspended/stopped only; --force deliberately refuses
// phase "running"). Used by the recovery paths (#3, #22) so an error-phase
// record can be retried before the loud delete+fresh fallback discards the
// conversation.
func (c *Client) ResumeForce(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", append([]string{"resume", worker, "--force"}, projectFlag(project)...)...)
	return err
}

func (c *Client) Stop(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", append([]string{"stop", worker}, projectFlag(project)...)...)
	return err
}
func (c *Client) Suspend(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", append([]string{"suspend", worker}, projectFlag(project)...)...)
	return err
}

// AttachArgv returns the argv to attach interactively. The caller exec()s it to
// hand over the TTY — it never goes through the runner, so it bypasses env()
// entirely. When the client holds a controller PAT, it is embedded as an
// `env SCION_HUB_TOKEN=<pat>` prefix (mirroring how the jail env is embedded
// for attach — see internal/jail/attach.go) so the exec'd scion binary still
// authenticates; omitted entirely when no token is set.
func (c *Client) AttachArgv(worker, project string) []string {
	argv := append([]string{c.bin, "attach", worker}, projectFlag(project)...)
	if tok := c.currentHubToken(); tok != "" {
		argv = append([]string{"env", "SCION_HUB_TOKEN=" + tok}, argv...)
	}
	return argv
}
