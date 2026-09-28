package scion

import (
	"context"
	"strings"
)

// KnowsUATScope reports whether the jail's scion knows the user-access-token
// scope, read from `scion hub token create --help`, which lists every scope
// it accepts (scion cmd/hub_token.go, UATScopeHelp). It runs the CLI only,
// never the hub, so it answers while the hub is down (a `lever up` after a
// pin change). lever uses it to tell scion versions apart, e.g. whether
// agent:lifecycle exists (scion f7155ecb, #1838).
func (c *Client) KnowsUATScope(ctx context.Context, scope string) (bool, error) {
	out, err := c.run(ctx, "", "hub", "token", "create", "--help")
	if err != nil {
		return false, err
	}
	return helpListsScope(out, scope), nil
}

// helpListsScope reports whether a scope list (one "  <scope>  <description>"
// per line) names scope as a whole first field.
func helpListsScope(help, scope string) bool {
	for _, line := range strings.Split(help, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == scope {
			return true
		}
	}
	return false
}
