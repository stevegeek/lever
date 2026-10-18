package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/termsafe"
)

// operatorRefusal is a non-200 answer from the broker's operator socket, with
// its machine-readable reason (broker.ReasonHeader) and the worker's phase
// when the refusal names one. Text is the broker's human text, made safe to
// print: it can quote a phase or a path.
type operatorRefusal struct {
	Status int
	Reason string
	Phase  string
	Text   string
}

func (e *operatorRefusal) Error() string { return e.Text }

// maxOperatorBody bounds what the CLI reads of an operator-socket answer.
const maxOperatorBody = 1 << 20

// operatorPost POSTs in as JSON to path on the operator socket sock and
// decodes a 200 answer into out (nil discards it). A refusal comes back as
// *operatorRefusal; a socket nobody answers on, as the transport's error
// (a *url.Error). Unlike httpjson.Post it keeps the response headers, which
// carry the reason.
func operatorPost(ctx context.Context, sock, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, udsURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := udsClient(sock).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOperatorBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &operatorRefusal{
			Status: resp.StatusCode,
			Reason: safeToken(resp.Header.Get(broker.ReasonHeader)),
			Phase:  safeToken(resp.Header.Get(broker.PhaseHeader)),
			Text:   termsafe.Sanitize(strings.TrimSpace(string(data))),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s answer: %w", path, err)
	}
	return nil
}

// safeToken keeps a header value only when it is one short token of
// [a-z0-9-]: reasons and phases are, and anything else is not printed.
func safeToken(s string) string {
	if len(s) == 0 || len(s) > 40 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}
