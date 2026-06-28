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
	// NoAuth disables scion's auth propagation (`--no-auth`) instead of requesting
	// `--harness-auth oauth-token`. Set for api-key agents: they hold no
	// CLAUDE_CODE_OAUTH_TOKEN (scion's oauth-token env-gather would fail), and get
	// their credential in-container instead (mTLS cert via image env, LLM
	// capability token written into the claude settings.json by lever-agent boot).
	NoAuth bool
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
	if o.NoAuth {
		// api-key: propagate no auth; the agent's credential arrives in-container.
		args = append(args, "--no-auth")
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
