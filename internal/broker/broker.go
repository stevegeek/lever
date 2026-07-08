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

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
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
	Agents          []string // valid worker identities that may be provisioned
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

	// Worker dispatch (host-side). Runtime is the scion client the broker drives;
	// Workers are the config-derived, path-authoritative worker descriptions;
	// BrokerCAPEM/BrokerURL are copied into each worker's staged bootstrap so it
	// trusts the same CA and dials the same broker as the manager.
	Runtime     WorkerRuntime
	Workers     []WorkerSpec
	BrokerCAPEM string
	BrokerURL   string

	// InstanceProject is the single Scion project (-g) that the manager and
	// all workers are agents in; = the jail mount root. Used when a message
	// is addressed to the manager's agent identity, and as the constant -g
	// for every worker dispatch/lifecycle/list call.
	InstanceProject string
	// ManagerSlug is the manager's scion agent slug — the app name (apply's
	// start-manager dispatches the manager as Worker: app.Name). It is DISTINCT
	// from ManagerIdentity, the cert CN used for authn: scion knows the manager
	// only by its slug, so a message routed to agent:<CN> fails with
	// `Agent "<CN>" not found in project`. Empty defaults to ManagerIdentity
	// (embedders/tests that never message the manager).
	ManagerSlug string
	// WorkerToWorker enables worker→worker messaging; default false (deny).
	WorkerToWorker bool
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

	runtime     WorkerRuntime
	workers     map[string]WorkerSpec
	brokerCAPEM string
	brokerURL   string

	instanceProject string
	managerSlug     string // the manager's scion agent slug (app name), ≠ the cert CN
	workerToWorker  bool

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
	workers := make(map[string]WorkerSpec, len(c.Workers))
	for _, g := range c.Workers {
		workers[g.Name] = g
	}
	if c.ManagerSlug == "" {
		c.ManagerSlug = c.ManagerIdentity
	}
	return &Broker{
		keys: c.Keys, ca: c.CA, tickets: c.Tickets, rules: c.Rules, reg: c.Registry,
		manager: c.ManagerIdentity, agents: agents,
		grantTTL: c.GrantTTL, ticketTTL: c.TicketTTL, srvName: c.ServerName, log: c.Log,
		minEpoch: c.RevocationState.MinEpoch,
		revoked:  revoked,
		persist:  c.PersistRevocation,
		apiKey:   c.APIKey, llmUpstream: up,
		runtime: c.Runtime, workers: workers, brokerCAPEM: c.BrokerCAPEM, brokerURL: c.BrokerURL,
		instanceProject: c.InstanceProject, managerSlug: c.ManagerSlug, workerToWorker: c.WorkerToWorker,
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

// audit logs a decision; detail is "" for plain allows. kvs are optional
// extra slog key/value pairs (token id, matched rule, minted claims).
func (b *Broker) audit(op, caller, decision, detail string, kvs ...any) {
	args := append([]any{"op", op, "caller", caller, "decision", decision, "detail", detail}, kvs...)
	b.log.Info("broker.decision", args...)
}
