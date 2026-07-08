package scion

import "context"

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
	if o.Image != "" {
		args = append(args, "--image", o.Image)
	}
	if o.Workspace != "" {
		args = append(args, "--workspace", o.Workspace)
	}
	_, err := c.run(ctx, "", args...)
	return err
}

func (c *Client) Resume(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", append([]string{"resume", worker}, projectFlag(project)...)...)
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
