// Package hubapi is a minimal host-side client for the scion Hub REST API.
//
// lever drives scion through its CLI. This package covers only the hub
// operations the CLI does not expose — today, removing a project's shared
// directories (see StripSharedDir). Keep it that way: reach for the CLI first.
package hubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single hub call. The hub is jail-local (OrbStack
// forwards the jail's loopback to the host), so a slow response means the hub
// is unhealthy, not that the network is far away.
const DefaultTimeout = 15 * time.Second

// Client talks to the hub as the controller PAT. It is safe for concurrent use.
type Client struct {
	// BaseURL is the hub root, e.g. "http://127.0.0.1:8080" (no trailing slash).
	BaseURL string
	// Token returns the controller PAT. It is called per request so a re-mint
	// between calls is picked up without rebuilding the client.
	Token func() string
	// HTTP is optional; nil uses a client bounded by DefaultTimeout.
	HTTP *http.Client
}

// SharedDir mirrors the fields of scion's api.SharedDir that lever inspects.
type SharedDir struct {
	Name        string `json:"name"`
	ReadOnly    bool   `json:"readOnly"`
	InWorkspace bool   `json:"inWorkspace"`
}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// do issues one authenticated request and decodes a 2xx JSON body into out
// (skipped when out is nil). It returns the status code alongside the error so
// callers can treat specific codes — 404 in particular — as success.
func (c *Client) do(ctx context.Context, method, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return 0, err
	}
	if c.Token != nil {
		if tok := c.Token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Cap the body: an error page is not a protocol response, and the text
		// lands in an apply-step error the operator reads.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, resp.Status)
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

// ProjectID resolves a project name or slug to its hub UUID. The shared-dirs
// endpoints take a UUID only (the hub's GetProject parses the path segment as
// one), so every call below resolves first.
func (c *Client) ProjectID(ctx context.Context, nameOrSlug string) (string, error) {
	var body struct {
		Projects []project `json:"projects"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/projects", &body); err != nil {
		return "", err
	}
	for _, p := range body.Projects {
		if p.Name == nameOrSlug || p.Slug == nameOrSlug {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no hub project named %q", nameOrSlug)
}

// SharedDirs lists a project's shared directories by project UUID.
func (c *Client) SharedDirs(ctx context.Context, projectID string) ([]SharedDir, error) {
	var body struct {
		SharedDirs []SharedDir `json:"sharedDirs"`
	}
	if _, err := c.do(ctx, http.MethodGet,
		"/api/v1/projects/"+url.PathEscape(projectID)+"/shared-dirs", &body); err != nil {
		return nil, err
	}
	return body.SharedDirs, nil
}

// StripSharedDir removes one shared directory from a project, by project name
// or slug. It is idempotent: a 404 (the project carries no such directory)
// counts as success.
//
// This is how lever declines scion's default `scratchpad` shared dir
// (scion#925). The hub stamps it on every NEW project, it is mounted
// read-write into EVERY agent of that project, and on lever's file/SQLite hub
// the server-side default cannot be turned off — OperationalSettings is
// postgres-gated, the admin endpoint returns 501, and the section has no
// settings.yaml key. Removing it per project is the only supported route, and
// it must go through the hub: `scion shared-dir remove` edits the broker-local
// settings file only, and the agent start path falls back to the hub's list
// when that file carries no entries.
//
// Requires project:update on the caller's token (the endpoint gates on the
// project ActionUpdate policy; agent:manage does NOT expand to it).
func (c *Client) StripSharedDir(ctx context.Context, projectNameOrSlug, dir string) error {
	id, err := c.ProjectID(ctx, projectNameOrSlug)
	if err != nil {
		return err
	}
	code, err := c.do(ctx, http.MethodDelete,
		"/api/v1/projects/"+url.PathEscape(id)+"/shared-dirs/"+url.PathEscape(dir), nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}
