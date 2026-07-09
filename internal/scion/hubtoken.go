package scion

import (
	"context"
	"fmt"
	"strings"
)

// HubTokenCreate mints a personal access token against the current hub endpoint
// (dev-auth admin), returning the opaque token string. scion's `hub token
// create` requires a target project (--project, resolved by name or ID) and a
// label (--name), so both are passed explicitly; projectDir is the registered
// project's dir, supplied as cwd so scion resolves the current project context.
func (c *Client) HubTokenCreate(ctx context.Context, projectDir, project, name string, scopes []string) (string, error) {
	out, err := c.run(ctx, projectDir, "hub", "token", "create",
		"--project", project, "--name", name, "--scopes", strings.Join(scopes, ","))
	if err != nil {
		return "", err
	}
	return parseHubToken(out)
}

// parseHubToken extracts the opaque PAT from `scion hub token create` output.
// scion prints a human-readable block with the token on a "Token: <value>" line
// (verified live: --format json is NOT honored for this command). We scan for
// that line, and fall back to any scion_pat_ token elsewhere in the output.
func parseHubToken(out string) (string, error) {
	for _, ln := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "Token:"); ok {
			if t := strings.TrimSpace(rest); t != "" {
				return t, nil
			}
		}
	}
	// Fallback: a bare scion_pat_ token anywhere in the output.
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "scion_pat_") {
			return f, nil
		}
	}
	return "", fmt.Errorf("no token found in `hub token create` output")
}
