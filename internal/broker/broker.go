// Package broker is the host-side capability authority and MCP gateway. It mints
// per-agent biscuit capabilities under the request/delegation policy and the
// tool registry, and gates every MCP call so real credentials never enter a
// container.
package broker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

const (
	defaultGrantTTL  = time.Hour
	defaultTicketTTL = 10 * time.Minute
)

// Config assembles a Broker. Zero GrantTTL/TicketTTL are defaulted.
type Config struct {
	Keys            token.KeyPair
	CA              *ca.CA
	Tickets         *ca.TicketStore
	Rules           *rules.Policy
	Registry        *registry.Registry
	ManagerIdentity string   // the cert CN permitted to call /provision
	Agents          []string // valid grove identities that may be provisioned
	GrantTTL        time.Duration
	TicketTTL       time.Duration
	ServerName      string // the server cert hostname agents dial (host.orb.internal)
	Log             *slog.Logger
}

// Broker is the running capability authority + gateway.
type Broker struct {
	keys      token.KeyPair
	ca        *ca.CA
	tickets   *ca.TicketStore
	rules     *rules.Policy
	reg       *registry.Registry
	manager   string
	agents    map[string]struct{}
	grantTTL  time.Duration
	ticketTTL time.Duration
	srvName   string
	log       *slog.Logger

	mu       sync.Mutex
	minEpoch int
	revoked  map[string]bool
}

// New builds a Broker from c.
func New(c Config) *Broker {
	if c.GrantTTL <= 0 {
		c.GrantTTL = defaultGrantTTL
	}
	if c.TicketTTL <= 0 {
		c.TicketTTL = defaultTicketTTL
	}
	agents := make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		agents[a] = struct{}{}
	}
	return &Broker{
		keys: c.Keys, ca: c.CA, tickets: c.Tickets, rules: c.Rules, reg: c.Registry,
		manager: c.ManagerIdentity, agents: agents,
		grantTTL: c.GrantTTL, ticketTTL: c.TicketTTL, srvName: c.ServerName, log: c.Log,
		revoked: map[string]bool{},
	}
}

// MinEpoch returns the current minimum acceptable token epoch.
func (b *Broker) MinEpoch() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.minEpoch
}

// BumpEpoch invalidates every token minted at the current epoch (revoke-all).
func (b *Broker) BumpEpoch() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.minEpoch++
}

// Revoke blocks one agent from any further authorization.
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

func (b *Broker) isAgent(name string) bool {
	_, ok := b.agents[name]
	return ok
}

// audit logs a decision; detail is "" for plain allows.
func (b *Broker) audit(op, caller, decision, detail string) {
	b.log.Info("broker.decision", "op", op, "caller", caller, "decision", decision, "detail", detail)
}
