package scion

import (
	"context"
	"strings"
)

// KnowsUATScope reports whether the jail's scion knows the user-access-token
// scope, read from scion hub token create --help. It runs the CLI only, never
// the hub, so it answers while the hub is down.
func (c *Client) KnowsUATScope(ctx context.Context, scope string) (bool, error) {
	out, err := c.run(ctx, "", "hub", "token", "create", "--help")
	if err != nil {
		return false, err
	}
	return helpListsScope(out, scope), nil
}

func helpListsScope(help, scope string) bool {
	for _, line := range strings.Split(help, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == scope {
			return true
		}
	}
	return false
}
