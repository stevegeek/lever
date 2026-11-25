package scion

import (
	"context"
	"encoding/json"
	"errors"
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
	projects, err := parseHubProjects(out)
	if err != nil {
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

type hubProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// errHubProjectsTruncated: scion lists one page of projects (500 by default).
// A name missing from a partial list may still be on the hub, and registering
// it again would make a second record for the same tree.
var errHubProjectsTruncated = errors.New("the hub's project list is truncated; lever will not decide from a partial list whether this project is registered")

// parseHubProjects reads `scion hub projects --format json`: an object
// {"projects": [...], "total": N, "truncated": bool} from scion d34ffbc1 on, a
// bare array before.
func parseHubProjects(out string) ([]hubProject, error) {
	var raw json.RawMessage
	if err := parseJSON(out, &raw); err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return nil, nil
	}
	if body[0] == '[' {
		var projects []hubProject
		if err := json.Unmarshal(raw, &projects); err != nil {
			return nil, fmt.Errorf("could not parse scion JSON output: %w", err)
		}
		return projects, nil
	}
	var page struct {
		Projects  *[]hubProject `json:"projects"`
		Truncated bool          `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("could not parse scion JSON output: %w", err)
	}
	if page.Projects == nil {
		return nil, errors.New("could not parse scion JSON output: no projects list")
	}
	if page.Truncated {
		return nil, errHubProjectsTruncated
	}
	return *page.Projects, nil
}
