package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// The broker POST/GET helpers (Enrol, Renew, Request, Provision, ListTools) all
// perform the same wire dance — marshal→POST→Content-Type→Do→status-check→decode
// — but drifted apart on two axes the plan (D1) requires us to KEEP as parameters
// rather than unify: the body-read limit (ListTools bounds decode to 1 MiB, the
// rest are unbounded) and the non-200 error style (Request/Provision append the
// response body, Enrol/Renew/ListTools drop it). postJSON/getJSON parameterize
// exactly those two axes plus the message label, so each caller's wire-error
// contract is byte-identical to before. directivePost is deliberately EXCLUDED
// (it returns a raw json.RawMessage and a prefix-less error body — a different
// contract pinned by mcpserver_test).

// limited bounds r to limit bytes; limit <= 0 means unbounded (read r as-is).
func limited(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		return r
	}
	return io.LimitReader(r, limit)
}

// postJSON marshals body to JSON and POSTs it to url with client (Content-Type
// application/json), decoding a 200 response into T. See doJSON for the non-200
// and decode error contract. limit bounds the response-body read (0 = unbounded);
// includeBody controls whether a non-200 error carries the trimmed response body.
func postJSON[T any](ctx context.Context, client *http.Client, url string, body any, limit int64, label string, includeBody bool) (T, error) {
	var zero T
	// json.Marshal of the string/any maps these callers pass cannot fail; the
	// label-prefixed wrap keeps the (unreachable) message shape consistent.
	raw, err := json.Marshal(body)
	if err != nil {
		return zero, fmt.Errorf("%s: marshal: %w", label, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON[T](client, req, limit, label, includeBody)
}

// getJSON GETs url with client and decodes a 200 response into T. GET carries no
// body and no Content-Type; non-200 errors never include the body (the only GET
// caller, ListTools, drops it).
func getJSON[T any](ctx context.Context, client *http.Client, url string, limit int64, label string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	return doJSON[T](client, req, limit, label, false)
}

// doJSON executes req and decodes a 200 response into T. Errors are prefixed with
// label: transport failures as "<label>: <err>", a non-200 as "<label> status
// <code>" (plus ": <trimmed-body>" when includeBody), and a decode failure as
// "<label> decode: <err>". limit bounds both the error-body read and the decode
// read (0 = unbounded).
func doJSON[T any](client *http.Client, req *http.Request, limit int64, label string, includeBody bool) (T, error) {
	var zero T
	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if includeBody {
			b, _ := io.ReadAll(limited(resp.Body, limit))
			return zero, fmt.Errorf("%s status %d: %s", label, resp.StatusCode, bytes.TrimSpace(b))
		}
		return zero, fmt.Errorf("%s status %d", label, resp.StatusCode)
	}
	var out T
	if err := json.NewDecoder(limited(resp.Body, limit)).Decode(&out); err != nil {
		return zero, fmt.Errorf("%s decode: %w", label, err)
	}
	return out, nil
}
