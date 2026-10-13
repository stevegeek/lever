package scion

import (
	"context"
	"fmt"
	"strings"
)

// InitProject runs `scion init` inside dir, registering it as an external
// (directory) project. dir must be a non-git directory.
func (c *Client) InitProject(ctx context.Context, dir string) error {
	_, err := c.run(ctx, dir, "init", nonInteractive)
	return err
}

// HubLink registers dir's project with the hub and this broker as a provider
// (non-git → in-place live mount). Runs inside dir.
//
// Under a user access token, scion links only a project the hub already knows
// by the ID dir's settings hold (a token cannot register one); see
// PinHubProject.
func (c *Client) HubLink(ctx context.Context, dir string) error {
	_, err := c.run(ctx, dir, "hub", "link", "--hub", c.hubEndpoint, "-y", nonInteractive)
	return err
}

// HubProjectID returns the hub's ID for the project named name, "" when the
// hub lists none the caller can read. More than one is an error: linking to a
// guess would bind the tree to another project's record.
func (c *Client) HubProjectID(ctx context.Context, name string) (string, error) {
	// runValue: the JSON is on stdout; banners and warnings are on stderr.
	out, err := c.runValue(ctx, "", "hub", "projects", "--format", "json", nonInteractive)
	if err != nil {
		return "", err
	}
	var projects []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := parseJSON(out, &projects); err != nil {
		return "", fmt.Errorf("scion hub projects: %w", err)
	}
	var ids []string
	for _, p := range projects {
		if p.Name == name && p.ID != "" {
			ids = append(ids, p.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("the hub lists %d projects named %q (%s); lever will not guess which one this tree is",
		len(ids), name, strings.Join(ids, ", "))
}

// PinHubProject records id as dir's hub project (hub.projectId in the
// project's own settings), so the next HubLink finds the hub's record by ID
// instead of by the new local ID a fresh init generates.
func (c *Client) PinHubProject(ctx context.Context, dir, id string) error {
	return c.ConfigSetProject(ctx, dir, "hub.projectId", id)
}
