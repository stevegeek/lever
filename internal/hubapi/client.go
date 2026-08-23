// Package hubapi is a minimal client for the scion Hub REST API.
//
// lever drives scion through its CLI. This package covers only the hub
// operations the CLI does not expose — today, listing a project's agents,
// resolving one's hub id and verifying its stored role (see Agents, AgentID,
// VerifyAgentRole) and removing a project's shared directories (see
// StripSharedDir). Keep it that way: reach for the CLI first.
//
// Requests run INSIDE the jail (see JailCurl), never from the host. The hub
// binds the jail's loopback, and lever's Lima template suppresses every
// guest→host port forward as a containment guarantee, so a host-side call
// cannot reach it on that backend. Going through the jail also picks the right
// hub when two instances are up, instead of racing for one host port.
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
// A transport-level failure (the hub is down, the jail is not up) returns a
// non-nil error and status 0, so callers can never mistake it for a status.
type Doer interface {
	Do(ctx context.Context, method, path string) (status int, body []byte, err error)
}

// Client talks to the hub as the controller PAT. It is safe for concurrent use
// when its Doer is.
type Client struct {
	T Doer
}

// APIError means the hub ANSWERED but the answer was not usable: a non-2xx
// status, a body that would not decode, or a project the hub does not list.
//
// The distinction matters to callers that must decide whether a failure is
// actionable. "lever could not reach the hub" is usually just a stopped
// instance; "the hub said 403" is a credential problem someone has to fix.
// Transport failures come back as the Doer's own error, never as an APIError.
type APIError struct {
	// Status is the HTTP status, or 0 when the hub replied but the problem was
	// not the status (an undecodable body, or an absent project).
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

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

// do issues one request, guarding against a Client built without a transport.
func (c *Client) do(ctx context.Context, method, path string) (int, []byte, error) {
	if c.T == nil {
		return 0, nil, errors.New("hubapi: client has no transport")
	}
	return c.T.Do(ctx, method, path)
}

// get issues a GET and decodes a 2xx JSON body into out.
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

// snippet caps a body for an error message. A wrong service on the hub port can
// return anything, and this text lands in an apply-step error the operator reads.
func snippet(body []byte) string {
	const max = 256
	if len(body) > max {
		return string(body[:max]) + "…"
	}
	return string(body)
}

// ProjectID resolves a project name or slug to its hub UUID. The shared-dirs
// endpoints take a UUID only — the hub parses that path segment as one — so
// every call below resolves first.
//
// endpointHint names the hub in the not-found error. That error is the
// operator's only clue when the jail reached a hub, but the wrong one.
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
	// More than one match must fail, not pick the first. Every other lever call
	// identifies the project by its in-jail path; only this one matches on a
	// name. If a hub ever accumulated two records for one tree, taking the first
	// would strip a shared dir from the wrong record and report success while
	// the live project kept the writable mount.
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
	return "", &APIError{Msg: fmt.Sprintf("no project named %q at hub %s (it listed %d project(s))",
		nameOrSlug, endpointHint, len(body.Projects))}
}

// SharedDirs lists a project's shared directories by project UUID.
func (c *Client) SharedDirs(ctx context.Context, projectID string) ([]SharedDir, error) {
	// Decode loosely first and REQUIRE the key. StripSharedDir treats an empty
	// list as proof of removal, so a hub that renamed this field would decode to
	// zero entries and the verify would pass vacuously — the one failure it
	// exists to catch. A moved route already fails loud (non-2xx), so this
	// closes the only silent case.
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
// The verify read is the point. The hub answers 404 for "no such shared dir",
// for "no such project", and for "no such route", so the DELETE status alone
// cannot distinguish "already clean" from "the endpoint moved and lever
// silently did nothing". Re-reading the list turns all of that into one
// post-condition, which is what a security control needs.
//
// This is how lever declines scion's default `scratchpad` shared dir
// (scion#925). The hub stamps it on every NEW project, mounts it read-write
// into EVERY agent of that project, and on lever's file/SQLite hub the
// server-side default cannot be turned off — OperationalSettings is
// postgres-gated, the admin endpoint returns 501, and the section has no
// settings.yaml key. Removal must go through the hub: `scion shared-dir
// remove` edits the broker-local settings file only, and scion's agent start
// path falls back to the hub's list when that file carries no entries.
//
// Requires project:update on the caller's token (the endpoint gates on the
// project ActionUpdate policy; agent:manage does NOT expand to it).
func (c *Client) StripSharedDir(ctx context.Context, projectNameOrSlug, endpointHint, dir string) error {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return err
	}

	// The id comes from the hub, and dir is a lever constant, so escaping is
	// belt-and-braces — but it keeps a hostile or buggy hub from choosing the
	// URL's structure (a `?` or `#` in an id would silently change the request).
	status, body, err := c.do(ctx, http.MethodDelete,
		"/api/v1/projects/"+url.PathEscape(id)+"/shared-dirs/"+url.PathEscape(dir))
	if err != nil {
		return err
	}
	// 404 is not yet an error: the verify read below decides. Anything else
	// outside 2xx is — a 403 (no project:update) must fail loud.
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
