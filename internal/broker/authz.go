package broker

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// callerID returns the mTLS-authenticated caller identity.
func (b *Broker) callerID(r *http.Request) (string, error) {
	if r.TLS == nil {
		return "", errors.New("broker: request has no TLS state")
	}
	return ca.AgentFromConnState(*r.TLS)
}

// bearerBiscuit extracts and base64url-decodes the biscuit from the
// Authorization header.
func bearerBiscuit(r *http.Request) ([]byte, error) {
	h := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if !strings.HasPrefix(h, pfx) {
		return nil, errors.New("broker: missing bearer biscuit")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(h, pfx))
	if err != nil {
		return nil, fmt.Errorf("broker: decode biscuit: %w", err)
	}
	return raw, nil
}

// authorize verifies the caller may perform operation: mTLS identity, not
// revoked, and a biscuit bound to that caller granting the operation. Returns
// the caller identity on success. Fails closed.
func (b *Broker) authorize(r *http.Request, operation string) (string, error) {
	caller, err := b.callerID(r)
	if err != nil {
		return "", err
	}
	if b.isRevoked(caller) {
		return "", fmt.Errorf("broker: caller %q is revoked", caller)
	}
	tok, err := bearerBiscuit(r)
	if err != nil {
		return "", err
	}
	if err := token.Verify(b.keys.Public, tok, token.Request{
		Caller:    caller,
		Operation: operation,
		Now:       time.Now(),
		MinEpoch:  b.MinEpoch(),
	}); err != nil {
		return "", err
	}
	return caller, nil
}
