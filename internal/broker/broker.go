package broker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

const defaultGrantTTL = time.Hour

// Broker is the running capability broker: keys, CA, policy, and mutable
// epoch/revocation state.
type Broker struct {
	keys   token.KeyPair
	ca     *ca.CA
	policy Policy
	log    *slog.Logger

	mu       sync.Mutex
	minEpoch int
	revoked  map[string]bool
}

// New builds a Broker. A zero GrantTTL is defaulted.
func New(keys token.KeyPair, c *ca.CA, p Policy, log *slog.Logger) *Broker {
	if p.GrantTTL <= 0 {
		p.GrantTTL = defaultGrantTTL
	}
	return &Broker{keys: keys, ca: c, policy: p, log: log, revoked: map[string]bool{}}
}

// MinEpoch returns the current minimum acceptable token epoch.
func (b *Broker) MinEpoch() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.minEpoch
}

// BumpEpoch invalidates all tokens minted at the current epoch (revoke-all).
func (b *Broker) BumpEpoch() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.minEpoch++
}

// Revoke blocks a single agent from any further authorization.
func (b *Broker) Revoke(agent string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[agent] = true
}

func (b *Broker) isRevoked(agent string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revoked[agent]
}

// audit logs a decision. reason is "" for allows.
func (b *Broker) audit(operation, caller, decision, detail string) {
	b.log.Info("broker.decision",
		"operation", operation, "caller", caller, "decision", decision, "detail", detail)
}
