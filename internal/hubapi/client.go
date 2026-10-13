// Package hubapi is a minimal client for the scion Hub REST API.
//
// It covers only hub operations the scion CLI does not expose; prefer the
// CLI.
//
// Requests run inside the jail, never from the host: the hub binds the
// jail's loopback, and the Lima template suppresses every guest-to-host port
// forward as a containment guarantee. Going through the jail also picks the
// right hub when two instances are up.
//
// There are two transports, and a Client acts as whichever identity its Doer
// authenticates:
//
//   - JailCurl sends the controller PAT as a bearer token. It is the
//     transport for project-scoped work (projects, agents, shared dirs,
//     settings).
//   - AssertCurl sends a fresh lever-signed assertion in the
//     X-Lever-Hub-Assertion header and never an Authorization header. It is
//     the transport for identity and governance (users, invites, roles,
//     bindings, constraints, token mint and revoke, system identity), as
//     the admin or controller identity, and only for the routes
//     AssertionPathAllowed accepts.
//
// Neither secret reaches a command line: the jail runner carries both on
// the child's stdin.
package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Doer performs one hub request and returns the HTTP status and response body.
// A transport-level failure returns a non-nil error and status 0.
type Doer interface {
	Do(ctx context.Context, method, path string) (status int, body []byte, err error)
}

// BodyDoer is a Doer that can also send a request body. A Client whose Doer
// lacks it refuses the settings write rather than send an empty body, which
// would wipe every project setting.
type BodyDoer interface {
	Doer
	DoBody(ctx context.Context, method, path string, body []byte) (status int, respBody []byte, err error)
}

// Client talks to the hub as the identity its Doer authenticates. It is safe
// for concurrent use when its Doer is.
type Client struct {
	T Doer
}

// APIError means the hub answered but the answer was not usable: a non-2xx
// status, a body that would not decode, or a project the hub does not list.
// Transport failures come back as the Doer's own error, never as an APIError.
type APIError struct {
	// Status is the HTTP status, or 0 when the hub replied but the problem was
	// not the status (an undecodable body, or an absent project).
	Status int
	Msg    string
	// NoProject marks ProjectID's answer for a name the hub does not list.
	NoProject bool
}

func (e *APIError) Error() string { return e.Msg }

var errNoTransport = errors.New("hubapi: client has no transport")

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

func (c *Client) do(ctx context.Context, method, path string) (int, []byte, error) {
	if c.T == nil {
		return 0, nil, errNoTransport
	}
	return c.T.Do(ctx, method, path)
}

func (c *Client) doBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if c.T == nil {
		return 0, nil, errNoTransport
	}
	bd, ok := c.T.(BodyDoer)
	if !ok {
		return 0, nil, fmt.Errorf("hubapi: transport cannot send a request body (%s %s)", method, path)
	}
	return bd.DoBody(ctx, method, path, body)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	status, body, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &APIError{Status: status,
			Msg: fmt.Sprintf("GET %s: HTTP %d: %s", path, status, snippet(body))}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &APIError{
			Msg: fmt.Sprintf("GET %s: decoding response: %v", path, err)}
	}
	return nil
}

// snippet caps a body for an error message: a wrong service on the hub port
// can return anything, and the operator reads this text.
func snippet(body []byte) string {
	const max = 256
	if len(body) > max {
		return string(body[:max]) + "…"
	}
	return string(body)
}

// ProjectID resolves a project name or slug to its hub UUID. endpointHint
// names the hub in the not-found error, the operator's only clue when the jail
// reached the wrong hub.
func (c *Client) ProjectID(ctx context.Context, nameOrSlug, endpointHint string) (string, error) {
	var body struct {
		Projects []project `json:"projects"`
	}
	if err := c.get(ctx, "/api/v1/projects", &body); err != nil {
		return "", err
	}
	var matched []project
	for _, p := range body.Projects {
		if p.Name == nameOrSlug || p.Slug == nameOrSlug {
			matched = append(matched, p)
		}
	}
	// More than one match must fail, not pick the first: that could strip a
	// shared dir from the wrong record and report success while the live
	// project kept the writable mount.
	if len(matched) > 1 {
		ids := make([]string, 0, len(matched))
		for _, p := range matched {
			ids = append(ids, p.ID)
		}
		return "", &APIError{Msg: fmt.Sprintf("hub %s lists %d projects named %q (%s); refusing to guess which one",
			endpointHint, len(matched), nameOrSlug, strings.Join(ids, ", "))}
	}
	if len(matched) == 1 {
		return matched[0].ID, nil
	}
	return "", &APIError{NoProject: true, Msg: fmt.Sprintf("no project named %q at hub %s (it listed %d project(s))",
		nameOrSlug, endpointHint, len(body.Projects))}
}

// SharedDirs lists a project's shared directories by project UUID.
func (c *Client) SharedDirs(ctx context.Context, projectID string) ([]SharedDir, error) {
	// Require the key: StripSharedDir treats an empty list as proof of
	// removal, so a renamed field would make its verify pass vacuously.
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/projects/"+url.PathEscape(projectID)+"/shared-dirs", &raw); err != nil {
		return nil, err
	}
	field, ok := raw["sharedDirs"]
	if !ok {
		return nil, &APIError{Msg: "shared-dirs response has no \"sharedDirs\" field; the hub API changed shape and lever cannot confirm what is mounted"}
	}
	var dirs []SharedDir
	if err := json.Unmarshal(field, &dirs); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding sharedDirs: %v", err)}
	}
	return dirs, nil
}

// StripSharedDir removes one shared directory from a project, by project name
// or slug, and then VERIFIES the directory is gone. It is idempotent: removing
// a directory the project does not carry is a success.
//
// The hub answers 404 for no such dir, project or route alike, so the DELETE
// status cannot tell "already clean" from "did nothing"; the verify read is
// the post-condition.
//
// This declines scion's default scratchpad shared dir, which the hub stamps on
// every new project and mounts read-write into every agent; on a SQLite hub
// the default cannot be turned off. It must go through the hub: scion
// shared-dir remove edits only the broker-local settings file, and agent start
// falls back to the hub's list when that file is empty.
//
// Requires project:update on the caller's token; agent:manage does not expand
// to it.
func (c *Client) StripSharedDir(ctx context.Context, projectNameOrSlug, endpointHint, dir string) error {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return err
	}

	// Escaping keeps a hostile hub from choosing the URL's structure via the
	// id (a ? or # would change the request).
	status, body, err := c.do(ctx, http.MethodDelete,
		"/api/v1/projects/"+url.PathEscape(id)+"/shared-dirs/"+url.PathEscape(dir))
	if err != nil {
		return err
	}
	// 404 is left to the verify read; any other non-2xx must fail loud.
	if status != http.StatusNotFound && (status < 200 || status > 299) {
		return &APIError{Status: status,
			Msg: fmt.Sprintf("DELETE shared dir %q on project %q: HTTP %d: %s",
				dir, projectNameOrSlug, status, snippet(body))}
	}

	dirs, err := c.SharedDirs(ctx, id)
	if err != nil {
		return fmt.Errorf("verifying shared dir %q was removed from project %q: %w",
			dir, projectNameOrSlug, err)
	}
	for _, d := range dirs {
		if d.Name == dir {
			return &APIError{Status: status,
				Msg: fmt.Sprintf("shared dir %q is still on project %q after DELETE returned HTTP %d "+
					"(the hub did not remove it)", dir, projectNameOrSlug, status)}
		}
	}
	return nil
}

// ReadNotifications lists the caller's notifications (GET
// /api/v1/notifications) and discards them: a token without inbox:read gets
// 403.
func (c *Client) ReadNotifications(ctx context.Context) error {
	var raw json.RawMessage
	return c.get(ctx, "/api/v1/notifications", &raw)
}
