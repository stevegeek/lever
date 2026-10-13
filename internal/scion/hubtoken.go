package scion

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// HubToken is what scion hub token create hands back. Token is the one field
// that must be present; the rest lets a later apply judge the token without a
// hub call, since a UAT cannot list or revoke tokens. Scopes are as the hub
// expanded them (an alias comes back as its members).
//
// ExpiresAt is the hub's expiry. When scion reports none that lever can read,
// it is a lower bound worked out from the requested --expires, so the token is
// still renewed in time; it is zero only when neither is known.
type HubToken struct {
	Token     string
	ID        string
	Scopes    []string
	ExpiresAt time.Time
}

// scionDefaultTokenLifetime is what scion gives a token minted without
// --expires.
const scionDefaultTokenLifetime = 90 * 24 * time.Hour

// HubTokenCreate mints a user access token against the current hub endpoint
// (dev-auth admin). projectDir is the cwd scion resolves the project context
// from. expires is scion's --expires value; empty leaves scion's 90-day
// default, so callers should always pass one: a token that silently expires
// takes the instance down.
//
// It reads scion's --json answer. A scion whose create command has no --json
// rejects the flag before minting anything, and the call is repeated without
// it and its text block parsed.
func (c *Client) HubTokenCreate(ctx context.Context, projectDir, project, name string, scopes []string, expires string) (HubToken, error) {
	args := []string{"hub", "token", "create",
		"--project", project, "--name", name, "--scopes", strings.Join(scopes, ",")}
	if expires != "" {
		args = append(args, "--expires", expires)
	}
	args = append(args, nonInteractive)
	start := time.Now()
	// runValue: the token is on stdout; the dev-auth banner is on stderr.
	out, err := c.runValue(ctx, projectDir, append(args, "--json")...)
	var tok HubToken
	switch {
	case err == nil:
		tok, err = parseHubTokenJSON(out)
		if err != nil {
			// The mint happened: a scion that took --json but printed its text
			// block still hands over a token worth keeping.
			if t, terr := parseHubToken(out); terr == nil {
				tok, err = t, nil
			}
		}
	case unknownJSONFlag(err):
		out, err = c.run(ctx, projectDir, args...)
		if err != nil {
			return HubToken{}, err
		}
		tok, err = parseHubToken(out)
	}
	if err != nil {
		return HubToken{}, err
	}
	if tok.ExpiresAt.IsZero() {
		tok.ExpiresAt = expiryLowerBound(expires, start)
	}
	return tok, nil
}

// unknownJSONFlag reports cobra's refusal of --json, which it makes while
// parsing flags, before the command runs.
func unknownJSONFlag(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown flag: --json")
}

// HubTokenRevoke revokes a token by the id `hub token create` printed. Only an
// interactive or dev identity may revoke, so this runs inside the same
// throwaway dev-auth window that mints.
func (c *Client) HubTokenRevoke(ctx context.Context, projectDir, id string) error {
	_, err := c.run(ctx, projectDir, "hub", "token", "revoke", id, nonInteractive)
	return err
}

// parseHubTokenJSON reads scion hub token create --json:
//
//	{"token": "scion_pat_...",
//	 "accessToken": {"id": "<uuid>", "scopes": [...], "expiresAt": "<RFC 3339>", ...}}
//
// The token is required. An expiry that does not parse is left zero for
// HubTokenCreate's lower bound rather than failing a mint that already
// happened.
func parseHubTokenJSON(out string) (HubToken, error) {
	var resp struct {
		Token       string `json:"token"`
		AccessToken *struct {
			ID        string   `json:"id"`
			Scopes    []string `json:"scopes"`
			ExpiresAt string   `json:"expiresAt"`
		} `json:"accessToken"`
	}
	if err := parseJSON(out, &resp); err != nil {
		return HubToken{}, fmt.Errorf("`hub token create --json`: %w", err)
	}
	if resp.Token == "" {
		return HubToken{}, fmt.Errorf("no token found in `hub token create --json` output")
	}
	tok := HubToken{Token: resp.Token}
	if a := resp.AccessToken; a != nil {
		tok.ID, tok.Scopes = a.ID, a.Scopes
		if t, err := time.Parse(time.RFC3339Nano, a.ExpiresAt); err == nil {
			tok.ExpiresAt = t
		}
	}
	return tok, nil
}

// expiryLowerBound is the earliest expiry scion can have given a token for
// expires, a --expires value, when the mint started at start. scion counts
// from its own clock after start, so this is never later than the real
// expiry on a clock in step with the jail's. Zero for a form lever does not
// know.
func expiryLowerBound(expires string, start time.Time) time.Time {
	if expires == "" {
		return start.Add(scionDefaultTokenLifetime)
	}
	if t, err := time.Parse(time.RFC3339, expires); err == nil {
		return t
	}
	if len(expires) < 2 {
		return time.Time{}
	}
	n, err := strconv.Atoi(expires[:len(expires)-1])
	if err != nil || n <= 0 {
		return time.Time{}
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'y': 365 * 24 * time.Hour}[expires[len(expires)-1]]
	if unit == 0 || time.Duration(n) > time.Duration(math.MaxInt64)/unit {
		return time.Time{}
	}
	return start.Add(time.Duration(n) * unit)
}

// parseHubToken reads the text block of a scion whose hub token create has no
// --json:
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
// is the fallback. ID, Scopes and Expires are best-effort.
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
