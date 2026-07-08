package scion

import (
	"context"
	"strings"
)

// HubTokenCreate mints a personal access token with the given scopes against
// the current hub endpoint (dev-auth admin), returning the opaque token
// string.
func (c *Client) HubTokenCreate(ctx context.Context, scopes []string) (string, error) {
	out, err := c.run(ctx, "", "hub", "token", "create", "--scopes", strings.Join(scopes, ","))
	if err != nil {
		return "", err
	}
	// Accept either a bare token line or {"token":"..."} JSON.
	var j struct {
		Token string `json:"token"`
	}
	if perr := parseJSON(out, &j); perr == nil && j.Token != "" {
		return j.Token, nil
	}
	return strings.TrimSpace(out), nil
}
