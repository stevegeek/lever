package ca

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// ticket is a single-use enrolment grant bound to a grove.
type ticket struct {
	worker  string
	expires time.Time
}

// TicketStore mints and redeems single-use enrolment tickets. Safe for
// concurrent use.
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ticket
}

// NewTicketStore returns an empty store.
func NewTicketStore() *TicketStore {
	return &TicketStore{tickets: map[string]ticket{}}
}

// Issue mints a random opaque ticket bound to grove, valid for ttl.
func (s *TicketStore) Issue(worker string, ttl time.Duration) (string, error) {
	if worker == "" {
		return "", fmt.Errorf("ca: ticket for empty grove")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ca: ticket randomness: %w", err)
	}
	tk := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.tickets {
		if now.After(t.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[tk] = ticket{worker: worker, expires: now.Add(ttl)}
	return tk, nil
}

// Redeem consumes a ticket: it must exist, be unexpired, and match grove. On
// success the ticket is burned. A grove or expiry mismatch leaves it intact so
// a legitimate holder can still redeem; only a successful match burns it.
func (s *TicketStore) Redeem(tk, worker string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[tk]
	if !ok {
		return fmt.Errorf("ca: unknown ticket")
	}
	if now.After(t.expires) {
		delete(s.tickets, tk) // expired: clean up
		return fmt.Errorf("ca: ticket expired")
	}
	if t.worker != worker {
		return fmt.Errorf("ca: ticket grove mismatch")
	}
	delete(s.tickets, tk)
	return nil
}
