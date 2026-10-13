package ghpush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// API is the small GitHub REST client the PR and release operations use.
// The token goes only in the Authorization header, never in a URL, and every
// error text is scrubbed of it.
type API struct {
	Base        string // https://api.github.com
	HTTP        *http.Client
	MaxResponse int64 // bytes read from one response body (0 = 4 MiB)
}

// apiError is a non-2xx answer: the status and GitHub's message, scrubbed
// and cut short.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub API: HTTP %d", e.Status)
	}
	return fmt.Sprintf("GitHub API: HTTP %d: %s", e.Status, e.Message)
}

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

// errRedirect: the client never follows a redirect (a renamed repository
// answers 301); the Authorization header must not travel anywhere else.
var errRedirect = errors.New("GitHub API redirected; refused (renamed repository?)")

// NewAPIClient returns an http.Client for API that never follows redirects.
func NewAPIClient(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &cp
}

// do sends one request. in, when non-nil, is the JSON body; out, when
// non-nil, receives a 2xx JSON answer.
func (a *API) do(ctx context.Context, token, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, body)
	if err != nil {
		return fmt.Errorf("GitHub API: %s", Scrub(err.Error(), token))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, errRedirect) {
			return errRedirect
		}
		return fmt.Errorf("GitHub API %s: %s", method, Scrub(err.Error(), token))
	}
	defer resp.Body.Close()
	limit := a.MaxResponse
	if limit <= 0 {
		limit = 4 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("GitHub API %s: read: %s", method, Scrub(err.Error(), token))
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("GitHub API %s: response over %d bytes; refused", method, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var m struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(raw, &m)
		msg := m.Message
		for _, e := range m.Errors {
			if d := strings.TrimSpace(e.Message + " " + e.Code); d != "" {
				msg += "; " + d
			}
		}
		// Scrub before truncating (see Git.Run).
		msg = Scrub(strings.Map(dropControl, msg), token)
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300]) + "…"
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("GitHub API %s: bad JSON answer", method)
		}
	}
	return nil
}

func dropControl(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return ' '
	}
	return r
}
