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
	args = append(args, "start", o.Grove, o.Task, "--harness", harness, "--harness-auth", "oauth-token")
	if o.Image != "" {
		args = append(args, "--image", o.Image)
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
