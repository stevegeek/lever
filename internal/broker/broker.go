// Package broker is the host-side capability authority and MCP gateway. It mints
// per-agent signed capability tokens under the request/delegation policy and the
// tool registry, and gates every MCP call so real credentials never enter a
// container.
package broker

import (
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

const (
	// defaultGrantTTL is the lifetime stamped on minted capability tokens when the
	// operator sets no broker.grant_ttl. It is session-scale (matching the 24h
	// agent cert TTL) by design: in api-key mode a long-running claude reads the
	// LLM capability token (ANTHROPIC_AUTH_TOKEN) from its settings.json env block
	// once at startup and holds it for the whole session, and the in-container
	// lever-renew sidecar only refreshes every 12h — so a short TTL would strand a
	// session between refreshes. Token TTL is a backstop only; the live
	// epoch/revoke gate (checked per call at the gateway and /llm proxy) is the
	// real security cut, so a generous default is safe. Operators wanting tighter
	// expiry can lower broker.grant_ttl, but should keep it above the renew interval.
	defaultGrantTTL  = 24 * time.Hour
	defaultTicketTTL = 10 * time.Minute
)

const (
	// ReservedLLMTool is the built-in pseudo-tool name for the LLM proxy. It is
	// registered (api-key mode) so /request can mint capability(llm) tokens, but
	// it gets NO /mcp/llm/ gateway route and is hidden from /tools — it is served
	// only by the /llm proxy route.
	ReservedLLMTool = "llm"
	ReservedLLMOp   = "generate"
)

// RevocationState is the persisted revocation floor + per-agent revoke list.
type RevocationState struct {
	MinEpoch int      `json:"min_epoch"`
	Revoked  []string `json:"revoked"`
}

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
	// RevocationState seeds the epoch floor + revoke list at construction
	// (loaded from the state dir) so a restart never silently un-revokes.
	RevocationState RevocationState
	// PersistRevocation is called (under the broker lock) whenever revocation
	// state changes, to write it through to the state dir. nil ⇒ no persistence.
	PersistRevocation func(RevocationState) error

	// APIKey is the real Anthropic Console key bytes (loaded host-side from the
	// 0600 api_key_file by brokerctl). nil ⇒ no /llm route is served.
	APIKey []byte
	// LLMUpstream is the proxy target; empty defaults to https://api.anthropic.com.
	// Set by tests to a fake upstream. NEVER derived from a client request.
	LLMUpstream string

	// Grove dispatch (host-side). Runtime is the scion client the broker drives;
	// Groves are the config-derived, path-authoritative grove descriptions;
	// BrokerCAPEM/BrokerURL are copied into each grove's staged bootstrap so it
	// trusts the same CA and dials the same broker as the manager.
	Runtime     GroveRuntime
	Groves      []GroveSpec
	BrokerCAPEM string
	BrokerURL   string

	// ManagerProject is the manager's own scion project (-g), used when a
	// message is addressed to the manager's agent identity.
	ManagerProject string
	// GroveToGrove enables grove→grove messaging; default false (deny).
	GroveToGrove bool
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

	apiKey      []byte
	llmUpstream *url.URL

	runtime     GroveRuntime
	groves      map[string]GroveSpec
	brokerCAPEM string
	brokerURL   string

	managerProject string
	groveToGrove   bool

	mu           sync.Mutex
	minEpoch     int
	revoked      map[string]bool
	persist      func(RevocationState) error
	bootstrapped bool // /bootstrap latch (one manager ticket per process)
}

// New builds a Broker from c.
func New(c Config) *Broker {
	if c.GrantTTL <= 0 {
		c.GrantTTL = defaultGrantTTL
	}
	if c.TicketTTL <= 0 {
		c.TicketTTL = defaultTicketTTL
	}
	if c.Log == nil {
		// Default to a no-op logger so audit() never nil-panics when a caller
		// (e.g. brokerctl.Serve, which leaves Log unset) builds a Config without
		// one. Every decision path audits, so a nil log would otherwise crash
		// the first request.
		c.Log = slog.New(slog.DiscardHandler)
	}
	agents := make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		agents[a] = struct{}{}
	}
	revoked := make(map[string]bool, len(c.RevocationState.Revoked))
	for _, a := range c.RevocationState.Revoked {
		revoked[a] = true
	}
	upstream := c.LLMUpstream
	if upstream == "" {
		upstream = "https://api.anthropic.com"
	}
	up, _ := url.Parse(upstream) // operator/test-controlled, validated at serve time
	groves := make(map[string]GroveSpec, len(c.Groves))
	for _, g := range c.Groves {
		groves[g.Name] = g
	}
	return &Broker{
		keys: c.Keys, ca: c.CA, tickets: c.Tickets, rules: c.Rules, reg: c.Registry,
		manager: c.ManagerIdentity, agents: agents,
		grantTTL: c.GrantTTL, ticketTTL: c.TicketTTL, srvName: c.ServerName, log: c.Log,
		minEpoch: c.RevocationState.MinEpoch,
		revoked:  revoked,
		persist:  c.PersistRevocation,
		apiKey:   c.APIKey, llmUpstream: up,
		runtime: c.Runtime, groves: groves, brokerCAPEM: c.BrokerCAPEM, brokerURL: c.BrokerURL,
		managerProject: c.ManagerProject, groveToGrove: c.GroveToGrove,
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
	b.persistLocked()
}

// Revoke blocks one agent from any further authorization.
func (b *Broker) Revoke(agent string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[agent] = true
	b.persistLocked()
}

// persistLocked writes the current revocation state through the persist hook.
// Caller holds b.mu.
func (b *Broker) persistLocked() {
	if b.persist == nil {
		return
	}
	revoked := make([]string, 0, len(b.revoked))
	for a := range b.revoked {
		revoked = append(revoked, a)
	}
	if err := b.persist(RevocationState{MinEpoch: b.minEpoch, Revoked: revoked}); err != nil {
		b.log.Error("broker.persist_revocation", "err", err.Error())
	}
}

// revocationState returns a snapshot of the current revocation state.
// Caller must NOT hold b.mu.
func (b *Broker) revocationState() RevocationState {
	b.mu.Lock()
	defer b.mu.Unlock()
	revoked := make([]string, 0, len(b.revoked))
	for a := range b.revoked {
		revoked = append(revoked, a)
	}
	return RevocationState{MinEpoch: b.minEpoch, Revoked: revoked}
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
