package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Token is a hub access token (UAT) as the hub describes it; the secret is
// never part of it.
type Token struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ProjectID string    `json:"projectId"`
	Scopes    []string  `json:"scopes"`
	Revoked   bool      `json:"revoked"`
	ExpiresAt time.Time `json:"expiresAt"`
	Prefix    string    `json:"prefix"`
}

// tokenBodyWithheld replaces a mint response body in error text: the body of
// an answer lever could not use may still carry the secret.
const tokenBodyWithheld = "(response body withheld: it may carry the token)"

// CreateToken mints an access token for the calling identity
// (POST /api/v1/auth/tokens). The hub refuses this to any token-authenticated
// caller, so it needs an assertion. No error text ever includes the secret or
// the response body.
func (c *Client) CreateToken(ctx context.Context, name, projectID string, scopes []string, expiresAt time.Time) (string, Token, error) {
	const path = "/api/v1/auth/tokens"
	req := struct {
		Name      string   `json:"name"`
		ProjectID string   `json:"projectId"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expiresAt,omitempty"`
	}{Name: name, ProjectID: projectID, Scopes: scopes}
	if !expiresAt.IsZero() {
		req.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	}
	in, err := json.Marshal(req)
	if err != nil {
		return "", Token{}, fmt.Errorf("hubapi: encoding POST %s: %w", path, err)
	}
	status, resp, err := c.doBody(ctx, http.MethodPost, path, in)
	if err != nil {
		return "", Token{}, err
	}
	if status < 200 || status > 299 {
		msg := tokenBodyWithheld
		if !bytes.Contains(resp, []byte(`"token"`)) && !bytes.Contains(resp, []byte("scion_pat_")) {
			msg = snippet(resp)
		}
		return "", Token{}, &APIError{Status: status, Msg: fmt.Sprintf("POST %s: HTTP %d: %s", path, status, msg)}
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken *Token `json:"accessToken"`
	}
	// The decode error is dropped: encoding/json can quote input in it.
	if json.Unmarshal(resp, &body) != nil {
		return "", Token{}, &APIError{Msg: fmt.Sprintf("POST %s: response is not the expected JSON %s", path, tokenBodyWithheld)}
	}
	if body.Token == "" || body.AccessToken == nil || body.AccessToken.ID == "" {
		return "", Token{}, &APIError{Msg: fmt.Sprintf("POST %s: response has no token or no token id", path)}
	}
	return body.Token, *body.AccessToken, nil
}

// ListTokens lists the calling identity's access tokens
// (GET /api/v1/auth/tokens).
func (c *Client) ListTokens(ctx context.Context) ([]Token, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/auth/tokens", &raw); err != nil {
		return nil, err
	}
	field, ok := raw["items"]
	if !ok {
		return nil, &APIError{Msg: "token list has no \"items\" field; the hub API changed shape"}
	}
	var toks []Token
	if err := json.Unmarshal(field, &toks); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding token list: %v", err)}
	}
	return toks, nil
}

// RevokeToken revokes one of the calling identity's access tokens
// (POST /api/v1/auth/tokens/{id}/revoke). The hub answers 404 for any
// failure, including a token another identity owns; that comes back wrapping
// ErrNotFound.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("hubapi: revoking a token: empty id")
	}
	path := "/api/v1/auth/tokens/" + url.PathEscape(id) + "/revoke"
	status, resp, err := c.doBody(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: token %s: %s", ErrNotFound, id, snippet(resp))
	}
	return &APIError{Status: status, Msg: fmt.Sprintf("POST %s: HTTP %d: %s", path, status, snippet(resp))}
}
