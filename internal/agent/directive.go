package agent

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// directivePost posts {"id":id} to a directive route over the agent's own
// mTLS channel and returns the raw JSON body on 200. A non-200 surfaces as
// httpjson's *StatusError (the broker's terse body is carried verbatim in
// its text), the same shape the `request` tool already reports.
func directivePost(ctx context.Context, brokerURL string, client *http.Client, route, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := httpjson.Post(ctx, client, brokerURL+route, wire.DirectiveIDRequest{ID: id}, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DirectiveConsume atomically consumes an operator directive over the
// agent's mTLS channel. The returned JSON is the ONLY authoritative body.
func DirectiveConsume(ctx context.Context, brokerURL string, client *http.Client, id string) (json.RawMessage, error) {
	return directivePost(ctx, brokerURL, client, wire.PathDirectiveConsume, id)
}

// DirectiveCheck reads a directive's status (target-gated, read-only).
func DirectiveCheck(ctx context.Context, brokerURL string, client *http.Client, id string) (json.RawMessage, error) {
	return directivePost(ctx, brokerURL, client, wire.PathDirectiveCheck, id)
}

// DirectivePreview reads a pending directive's verified action WITHOUT
// consuming it (target-gated). The result is not operator authority.
func DirectivePreview(ctx context.Context, brokerURL string, client *http.Client, id string) (json.RawMessage, error) {
	return directivePost(ctx, brokerURL, client, wire.PathDirectivePreview, id)
}

// MessageVerify asks the broker who wrote a received message (web chat, a
// lever send, or no record), from host records only. The returned JSON is the
// only authoritative copy of the message text.
func MessageVerify(ctx context.Context, brokerURL string, client *http.Client, timestamp, from, ref string) (json.RawMessage, error) {
	var raw json.RawMessage
	req := wire.MessageVerifyRequest{Timestamp: timestamp, From: from, Ref: ref}
	if err := httpjson.Post(ctx, client, brokerURL+wire.PathMessageVerify, req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
