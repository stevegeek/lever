package scion

import "context"

type Agent struct {
	Slug     string `json:"slug"`
	Phase    string `json:"phase"`
	Activity string `json:"activity"`
}

type StartOpts struct {
	Grove   string
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

func (c *Client) List(ctx context.Context, project string) ([]Agent, error) {
	args := append([]string{"list", "--format", "json"}, projectFlag(project)...)
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

func (c *Client) Start(ctx context.Context, o StartOpts) error {
	harness := o.Harness
	if harness == "" {
		harness = "claude"
	}
	args := projectFlag(o.Project)
	args = append(args, "start", o.Grove, o.Task, "--harness", harness)
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

func (c *Client) Resume(ctx context.Context, grove, project string) error {
	_, err := c.run(ctx, "", append([]string{"resume", grove}, projectFlag(project)...)...)
	return err
}
func (c *Client) Stop(ctx context.Context, grove, project string) error {
	_, err := c.run(ctx, "", append([]string{"stop", grove}, projectFlag(project)...)...)
	return err
}
func (c *Client) Suspend(ctx context.Context, grove, project string) error {
	_, err := c.run(ctx, "", append([]string{"suspend", grove}, projectFlag(project)...)...)
	return err
}

// AttachArgv returns the argv to attach interactively. The caller exec()s it to
// hand over the TTY — it never goes through the runner.
func (c *Client) AttachArgv(grove, project string) []string {
	return append([]string{c.bin, "attach", grove}, projectFlag(project)...)
}
