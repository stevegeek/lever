package scion

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HubToken is what `scion hub token create` hands back. Token is the opaque
// bearer value and the one field that MUST be present; the rest is what lever
// records beside it so a later apply can judge the token without a hub call
// (a UAT cannot list or revoke tokens on newer scions — only the dev identity
// in the mint window can): ID is what `hub token revoke` takes, Scopes are the
// scopes AS THE HUB EXPANDED THEM (an alias like agent:manage comes back as
// its members), ExpiresAt is zero when scion printed none.
type HubToken struct {
	Token     string
	ID        string
	Scopes    []string
	ExpiresAt time.Time
}

// HubTokenCreate mints a user access token against the current hub endpoint
// (dev-auth admin). scion's `hub token create` requires a target project
// (--project, resolved by name or ID) and a label (--name), so both are passed
// explicitly; projectDir is the registered project's dir, supplied as cwd so
// scion resolves the current project context. expires is scion's --expires
// value ("360d", "1y", an RFC 3339 date); empty leaves scion's default, which
// is 90 days — lever always passes one, because a token that silently expires
// takes the whole instance down with it.
func (c *Client) HubTokenCreate(ctx context.Context, projectDir, project, name string, scopes []string, expires string) (HubToken, error) {
	args := []string{"hub", "token", "create",
		"--project", project, "--name", name, "--scopes", strings.Join(scopes, ",")}
	if expires != "" {
		args = append(args, "--expires", expires)
	}
	out, err := c.run(ctx, projectDir, args...)
	if err != nil {
		return HubToken{}, err
	}
	return parseHubToken(out)
}

// HubTokenRevoke revokes a token by the id `hub token create` printed. Only an
// interactive or dev identity may revoke, so this runs inside the same
// throwaway dev-auth window that mints.
func (c *Client) HubTokenRevoke(ctx context.Context, projectDir, id string) error {
	_, err := c.run(ctx, projectDir, "hub", "token", "revoke", id)
	return err
}

// parseHubToken reads `scion hub token create` output. scion prints a
// human-readable block (verified live: --format json is NOT honored for this
// command):
//
//	Created access token: lever-controller
//	  ID:      <uuid>
//	  Project: lever (<uuid>)
//	  Scopes:  agent:create, agent:read, ...
//	  Expires: 2026-10-07T10:08:14+02:00
//
//	Token: scion_pat_...
//
// The token line is required; a bare scion_pat_ token anywhere in the output
// is the fallback. ID, Scopes and Expires are best-effort: a scion that prints
// less still mints, and the record lever keeps just says less.
func parseHubToken(out string) (HubToken, error) {
	var tok HubToken
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case cutLabel(ln, "Token:", &tok.Token):
		case cutLabel(ln, "ID:", &tok.ID):
		case strings.HasPrefix(ln, "Scopes:"):
			for _, s := range strings.Split(strings.TrimPrefix(ln, "Scopes:"), ",") {
				if s = strings.TrimSpace(s); s != "" {
					tok.Scopes = append(tok.Scopes, s)
				}
			}
		case strings.HasPrefix(ln, "Expires:"):
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(strings.TrimPrefix(ln, "Expires:"))); err == nil {
				tok.ExpiresAt = t
			}
		}
	}
	if tok.Token != "" {
		return tok, nil
	}
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "scion_pat_") {
			tok.Token = f
			return tok, nil
		}
	}
	return HubToken{}, fmt.Errorf("no token found in `hub token create` output")
}

// cutLabel stores the value after label in *dst when ln starts with label and
// the value is non-empty. It reports whether the label matched, so a switch
// can fall through to the next label otherwise.
func cutLabel(ln, label string, dst *string) bool {
	rest, ok := strings.CutPrefix(ln, label)
	if !ok {
		return false
	}
	if v := strings.TrimSpace(rest); v != "" {
		*dst = v
	}
	return true
}
