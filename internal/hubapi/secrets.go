package hubapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// SetUserSecret sets a user-scope environment secret of the caller that is
// injected into every agent the caller starts (PUT /api/v1/secrets/{key}),
// as `scion hub env set --secret --always KEY VALUE` does. From scion
// e8160a10 on (8ee1dbd2f) the hub refuses user- and hub-level secrets to an
// access token and wants an interactive login; a hub assertion is one, so
// lever sends this as the controller. The value is plaintext (encoding raw).
// A PUT is idempotent, so a request that got no answer is sent again.
func (c *Client) SetUserSecret(ctx context.Context, key, value string) error {
	if key == "" || value == "" {
		return errors.New("hubapi: a user secret needs a key and a value")
	}
	body := struct {
		Value         string `json:"value"`
		Encoding      string `json:"encoding"`
		Scope         string `json:"scope"`
		InjectionMode string `json:"injectionMode"`
		Type          string `json:"type"`
		Target        string `json:"target"`
	}{value, "raw", "user", "always", "environment", key}
	return retryUnanswered(ctx, func() error {
		return c.send(ctx, http.MethodPut, "/api/v1/secrets/"+url.PathEscape(key), body, nil)
	}, nil)
}
