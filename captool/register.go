package captool

import (
	"context"
	"fmt"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Register announces this tool to the broker and caches the broker's
// verification public key and current epoch.
func (s *Server) Register(ctx context.Context) error {
	ops := make([]wire.OperationSpec, 0, len(s.ops))
	for _, o := range s.ops {
		ops = append(ops, wire.OperationSpec{Name: o.Name, CaveatParam: o.CaveatParam})
	}
	body := wire.RegisterRequest{Name: s.name, Backend: s.backend, Operations: ops, FirstParty: true}
	var rr wire.RegisterResponse
	if err := httpjson.Post(ctx, nil, s.adminURL+wire.PathRegister, body, &rr); err != nil {
		return fmt.Errorf("captool: register: %w", err)
	}
	pub, err := token.DecodePublicKey(rr.PublicKey)
	if err != nil {
		return fmt.Errorf("captool: register pubkey: %w", err)
	}
	s.mu.Lock()
	s.pubKey, s.epoch, s.epochAt = pub, rr.Epoch, time.Now()
	s.mu.Unlock()
	return nil
}

// epochRefreshTimeout bounds one /epoch refresh.
const epochRefreshTimeout = 5 * time.Second

// freshEpoch returns the epoch floor, cached for epochTTL. On a refresh
// error it keeps the last good value.
func (s *Server) freshEpoch(ctx context.Context) int {
	s.mu.Lock()
	if time.Since(s.epochAt) < s.epochTTL {
		e := s.epoch
		s.mu.Unlock()
		return e
	}
	s.mu.Unlock()

	// Bounded: a broker admin listener that accepts but never answers would
	// otherwise hold every call past the cache (http.DefaultClient has no
	// timeout). A failed refresh keeps the last good value, as documented.
	// WithoutCancel: a caller that hangs up must not fail the refresh and
	// be verified against a stale floor; the timeout still bounds it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), epochRefreshTimeout)
	defer cancel()
	var er wire.EpochResponse
	if err := httpjson.Get(ctx, nil, s.adminURL+wire.PathEpoch, &er); err == nil {
		s.mu.Lock()
		s.epoch, s.epochAt = er.Epoch, time.Now()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}
