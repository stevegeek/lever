package captool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

type registerOp struct {
	Name        string            `json:"name"`
	CaveatParam map[string]string `json:"caveat_param,omitempty"`
}

type registerBody struct {
	Name       string       `json:"name"`
	Backend    string       `json:"backend"`
	Operations []registerOp `json:"operations"`
	FirstParty bool         `json:"first_party"`
}

type registerResp struct {
	PublicKey string `json:"public_key"`
	Epoch     int    `json:"epoch"`
}

// Register announces this tool to the broker (first_party=true) and caches the
// broker's verification public key + current epoch from the response.
func (s *Server) Register(ctx context.Context) error {
	ops := make([]registerOp, 0, len(s.ops))
	for _, o := range s.ops {
		ops = append(ops, registerOp{Name: o.Name, CaveatParam: o.CaveatParam})
	}
	body, _ := json.Marshal(registerBody{Name: s.name, Backend: s.backend, Operations: ops, FirstParty: true})
	req, err := http.NewRequestWithContext(ctx, "POST", s.adminURL+"/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("captool: register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("captool: register status %d", resp.StatusCode)
	}
	var rr registerResp
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("captool: register decode: %w", err)
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

	req, err := http.NewRequestWithContext(ctx, "GET", s.adminURL+"/epoch", nil)
	if err == nil {
		if resp, derr := http.DefaultClient.Do(req); derr == nil {
			defer resp.Body.Close()
			var er struct {
				Epoch int `json:"epoch"`
			}
			if json.NewDecoder(resp.Body).Decode(&er) == nil {
				s.mu.Lock()
				s.epoch, s.epochAt = er.Epoch, time.Now()
				s.mu.Unlock()
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}
