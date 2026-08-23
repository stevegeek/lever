package captool

import (
	"context"
	"fmt"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Register announces this tool to the broker (first_party=true) and caches the
// broker's verification public key + current epoch from the response.
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

// freshEpoch returns the cached epoch floor, refreshing from /epoch when older
// than epochTTL. On a refresh error it keeps (and returns) the last good value.
func (s *Server) freshEpoch(ctx context.Context) int {
	s.mu.Lock()
	if time.Since(s.epochAt) < s.epochTTL {
		e := s.epoch
		s.mu.Unlock()
		return e
	}
	s.mu.Unlock()

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
