package scion

import "context"

// InitProject runs `scion init` inside dir, registering it as an external
// (directory) project. dir must be a non-git directory per the Lever model.
func (c *Client) InitProject(ctx context.Context, dir string) error {
	_, err := c.runIn(ctx, dir, "init")
	return err
}

// HubLink registers dir's project with the hub and this broker as a provider
// (non-git → in-place live mount). Runs inside dir.
func (c *Client) HubLink(ctx context.Context, dir string) error {
	_, err := c.runIn(ctx, dir, "hub", "link", "--hub", c.hubEndpoint, "-y")
	return err
}
