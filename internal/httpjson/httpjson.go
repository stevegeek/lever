// Package httpjson is the minimal JSON-over-HTTP client helper shared by the
// lever binaries that talk to the broker, with one bounded body read and one
// error shape.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBody bounds every response-body read (both the decoded 200 body and the
// body quoted in a StatusError).
const MaxBody = 1 << 20

// StatusError is returned when the server answers with a status other than
// 200. Body is the trimmed response body (at most MaxBody bytes), so callers can
// branch on Code or show the server's message without string matching.
type StatusError struct {
	Method string
	URL    string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s: status %d", e.Method, e.URL, e.Code)
	}
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.Code, e.Body)
}

// Status returns the HTTP status code carried by err when it is (or wraps) a
// StatusError, and 0 otherwise.
func Status(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Post POSTs in as JSON and decodes a 200 response into out (nil discards
// it). A non-200 yields a *StatusError.
func Post(ctx context.Context, client *http.Client, url string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("POST %s: marshal: %w", url, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(client, req, out)
}

// Get GETs url and decodes a 200 response into out. Errors follow Post.
func Get(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return do(client, req, out)
}

func do(client *http.Client, req *http.Request, out any) error {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, MaxBody)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(body)
		return &StatusError{Method: req.Method, URL: req.URL.String(), Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", req.Method, req.URL, err)
	}
	return nil
}
