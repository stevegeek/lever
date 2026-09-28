package hubapi

import (
	"context"
	"errors"
)

// Me returns the hub user the client's token belongs to
// (GET /api/v1/auth/me).
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	if err := c.get(ctx, "/api/v1/auth/me", &u); err != nil {
		return User{}, err
	}
	if u.ID == "" {
		return User{}, errors.New("GET /api/v1/auth/me returned no user id")
	}
	return u, nil
}

// UATScopes lists the user-access-token scopes the hub knows
// (GET /api/v1/auth/scopes; aliases such as agent:manage are not included).
// lever reads it to tell scion versions apart, e.g. whether agent:lifecycle
// exists (scion f7155ecb, #1838).
func (c *Client) UATScopes(ctx context.Context) ([]string, error) {
	var body struct {
		Scopes []struct {
			ID string `json:"id"`
		} `json:"scopes"`
	}
	if err := c.get(ctx, "/api/v1/auth/scopes", &body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		if s.ID != "" {
			out = append(out, s.ID)
		}
	}
	return out, nil
}
