// Package broker is the host-side capability authority and brokered-tool proxy.
// It mints per-agent signed capability tokens under the request/delegation
// policy and the tool registry, and gates every MCP call so real credentials
// never enter a container.
package broker

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/opsig"
)

const (
	// defaultGrantTTL is the lifetime stamped on minted capability tokens when the
	// operator sets no broker.grant_ttl. It is session-scale (matching the 24h
	// agent cert TTL) by design: in api-key mode a long-running claude reads the
	// LLM capability token (ANTHROPIC_AUTH_TOKEN) from its settings.json env block
	// once at startup and holds it for the whole session, and the in-container
	// lever-renew sidecar only refreshes every 12h — so a short TTL would strand a
	// session between refreshes. Token TTL is a backstop only; the live
	// epoch/revoke gate (checked per call on tool routes and the /llm proxy) is the
	// real security cut, so a generous default is safe. Operators wanting tighter
	// expiry can lower broker.grant_ttl, but should keep it above the renew interval.
	defaultGrantTTL  = 24 * time.Hour
	defaultTicketTTL = 10 * time.Minute
)

const (
	// ReservedLLMTool is the built-in pseudo-tool name for the LLM proxy. It is
	// registered (api-key mode) so /request can mint capability(llm) tokens, but
	// it gets NO /mcp/llm/ tool route and is hidden from /tools — it is served
	// only by the /llm proxy route.
	ReservedLLMTool = registry.ReservedLLMTool
	ReservedLLMOp   = registry.ReservedLLMOp
)

// RevocationState is the persisted revocation floor + per-agent revoke list.
type RevocationState struct {
	MinEpoch int      `json:"min_epoch"`
	Revoked  []string `json:"revoked"`
}

// Config assembles a Broker. Zero GrantTTL/TicketTTL are defaulted. The
// fields are grouped by concern; brokerctl.Build fills the identity and
// persistence groups and brokerctl's serve path decorates the rest.
type Config struct {
	// ---- Identity, keys and policy ----

	Keys     token.KeyPair
	CA       *ca.CA
	Tickets  *ca.TicketStore
	Rules    *rules.Policy
	Registry *registry.Registry
	// ManagerIdentity is the cert CN permitted to call /provision and the
	// worker/msg routes.
	ManagerIdentity string
	// ManagerSlug is the manager's scion agent slug — the app name (apply's
	// start-manager dispatches the manager as Worker: app.Name). It is DISTINCT
	// from ManagerIdentity, the cert CN used for authn: scion knows the manager
	// only by its slug, so a message routed to agent:<CN> fails with
	// `Agent "<CN>" not found in project`. Empty defaults to ManagerIdentity
	// (embedders/tests that never message the manager).
	ManagerSlug string
	// Agents lists the worker identities /provision may issue a ticket for.
	Agents     []string
	GrantTTL   time.Duration
	TicketTTL  time.Duration
	ServerName string // the server cert hostname agents dial (host.orb.internal)
	Log        *slog.Logger
	// Version and ConfigHash identify this broker process: the binary's
	// version string and a digest of the broker-relevant configuration it was
	// started with. Reported by /epoch so apply's broker-reuse shortcut can
	// detect a stale broker (old binary or old tool set) and restart it
	// instead of silently reusing it (#19). Both optional (empty = unreported).
	Version    string
	ConfigHash string

	// ---- Persisted state ----

	// RevocationState seeds the epoch floor + revoke list at construction
	// (loaded from the state dir) so a restart never silently un-revokes.
	RevocationState RevocationState
	// PersistRevocation is called (under the broker lock) whenever revocation
	// state changes, to write it through to the state dir. nil ⇒ no persistence.
	PersistRevocation func(RevocationState) error
	// DirectiveState seeds the persistent operator-directive store at
	// construction (loaded from the state dir); PersistDirectives writes it
	// through on every mutation. Modeled on RevocationState/PersistRevocation.
	DirectiveState    DirectiveState
	PersistDirectives func(DirectiveState) error

	// ---- LLM proxy ----

	// APIKey is the real Anthropic Console key bytes (loaded host-side from the
	// 0600 api_key_file by brokerctl). nil ⇒ no /llm route is served.
	APIKey []byte
	// LLMUpstream is the proxy target; empty defaults to https://api.anthropic.com.
	// Set by tests to a fake upstream. NEVER derived from a client request.
	LLMUpstream string

	// ---- Worker dispatch and messaging (host-side) ----

	// Runtime is the scion client the broker drives; Workers are the
	// config-derived, path-authoritative worker descriptions; BrokerCAPEM/
	// BrokerURL are copied into each worker's staged bootstrap so it trusts
	// the same CA and dials the same broker as the manager.
	Runtime     WorkerRuntime
	Workers     []WorkerSpec
	BrokerCAPEM string
	BrokerURL   string
	// InstanceProject is the single Scion project (-g) that the manager and
	// all workers are agents in; = the jail mount root. Used when a message
	// is addressed to the manager's agent identity, and as the constant -g
	// for every worker dispatch/lifecycle/list call.
	InstanceProject string
	// WorkerToWorker enables worker→worker messaging; default false (deny).
	WorkerToWorker bool
	// VerifyAgentRole refuses to RESUME a worker whose hub record stores no
	// authorization role while the installed scion resolves that to full hub
	// authority (see hubapi.VerifyAgentRole). A worker created by a scion older
	// than scion#1089 carries no role, the role is immutable after creation, and
	// `scion resume` cannot set one — so resuming such a record grants it agent
	// create, lifecycle and secret-read.
	//
	// Only the resume paths consult it: a freshly started worker is stamped
	// `--role baseline` by the start itself. nil ⇒ no-op (tests, and a manual
	// `broker serve` with no runtime wired).
	VerifyAgentRole func(ctx context.Context, agent string) error
	// ResolveAgentID maps an agent slug to its hub agent id. /msg/list needs it
	// to cut the notification feed down to one agent: lever reads notifications
	// with the host controller PAT, and the hub scopes that query to the
	// authenticated USER, so the raw answer carries every agent's events. The id
	// is what attributes each event.
	//
	// nil ⇒ /msg/list fails closed. Falling back to the unfiltered feed would be
	// the leak this exists to close.
	ResolveAgentID func(ctx context.Context, agentSlug string) (string, error)
	// AutoReenrol gates the natural-lapse healer (reenrol.go): "all" |
	// "manager" | "off" (resolved by brokerctl from config; empty = all).
	AutoReenrol string
	// Tree is the host path to the instance tree: the confinement anchor for
	// staging enrolment material. Everything below it is agent-writable and the
	// broker writes there as the operator, so wire.Stage confines every write to
	// this root and refuses agent-planted symlinks. Empty ⇒ the staging
	// directory's parent is used instead (tests); see Broker.stagingPath.
	Tree string
	// ManagerBootstrapDir is the host path to <tree>/.lever — where the
	// MANAGER's bootstrap.json is staged (workers carry theirs in WorkerSpec).
	// Empty disables manager healing (audited as an error on lapse).
	ManagerBootstrapDir string

	// ---- Operator directives ----

	// DirectiveVerifier gates the operator-directive UDS admin channel: nil
	// means directives are disabled and every /directive/* route 404s.
	DirectiveVerifier *opsig.Verifier
	// InstanceID is this instance's name, checked against Statement.Instance /
	// Envelope.Instance on every signed directive op (opsig.ParseStatement /
	// ParseEnvelope's instance-mismatch fail-closed check).
	InstanceID string
	// DirectiveAuditPath is the bounded JSON-lines audit log for the
	// directive channel. Empty ⇒ dirAudit.append is a no-op (still non-nil,
	// so handlers never nil-check it).
	DirectiveAuditPath string
	// DirectiveExpiryMax clamps how far in the future a submitted directive's
	// expires_at may sit (on top of opsig's own 24h hard cap).
	DirectiveExpiryMax time.Duration
}

// Broker is the running capability authority + brokered-tool proxy.
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
	log       *slog.Logger

	apiKey      []byte
	llmUpstream *url.URL

	runtime        WorkerRuntime
	verifyRole     func(ctx context.Context, agent string) error
	resolveAgentID func(ctx context.Context, agentSlug string) (string, error)
	tree           string
	workers        map[string]WorkerSpec
	brokerCAPEM    string
	brokerURL      string
	// liveAttempts/liveInterval bound waitWorkerLive's post-start poll; tests
	// shrink them per instance (like reenrolNow).
	liveAttempts int
	liveInterval time.Duration

	instanceProject string
	managerSlug     string // the manager's scion agent slug (app name), ≠ the cert CN
	workerToWorker  bool

	mu           sync.Mutex
	minEpoch     int
	revoked      map[string]bool
	persist      func(RevocationState) error
	bootstrapped bool // /bootstrap latch (one manager ticket per process)

	directives *DirectiveStore

	// Natural-lapse auto-re-enrol state (reenrol.go). reenrolNow is a test
	// seam (time.Now in production). reenrolMu guards the two maps only.
	autoReenrol         string
	managerBootstrapDir string
	reenrolEvents       chan string
	reenrolNow          func() time.Time
	reenrolMu           sync.Mutex
	reenrolLast         map[string]time.Time
	reenrolTries        map[string]int

	directiveVerifier  *opsig.Verifier
	instanceID         string
	dirAudit           *directiveAudit
	directiveExpiryMax time.Duration
	dirRate            *rateWindow

	version    string // reported by /epoch (see Config.Version)
	configHash string // reported by /epoch (see Config.ConfigHash)
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
	directives := newDirectiveStore(c.DirectiveState, c.PersistDirectives, c.Log)
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
	if c.AutoReenrol == "" {
		c.AutoReenrol = autoReenrolAll
	}
	return &Broker{
		// identity, keys and policy
		keys: c.Keys, ca: c.CA, tickets: c.Tickets, rules: c.Rules, reg: c.Registry,
		manager: c.ManagerIdentity, managerSlug: c.ManagerSlug, agents: agents,
		grantTTL: c.GrantTTL, ticketTTL: c.TicketTTL, log: c.Log,
		version: c.Version, configHash: c.ConfigHash,
		// persisted state
		minEpoch: c.RevocationState.MinEpoch, revoked: revoked, persist: c.PersistRevocation,
		directives: directives,
		// llm proxy
		apiKey: c.APIKey, llmUpstream: up,
		// worker dispatch and messaging
		runtime: c.Runtime, workers: workers, brokerCAPEM: c.BrokerCAPEM, brokerURL: c.BrokerURL,
		instanceProject: c.InstanceProject, workerToWorker: c.WorkerToWorker,
		verifyRole: c.VerifyAgentRole, resolveAgentID: c.ResolveAgentID, tree: c.Tree,
		liveAttempts: defaultLiveAttempts, liveInterval: defaultLiveInterval,
		autoReenrol: c.AutoReenrol, managerBootstrapDir: c.ManagerBootstrapDir,
		reenrolEvents: make(chan string, reenrolQueueDepth),
		reenrolNow:    time.Now,
		reenrolLast:   map[string]time.Time{},
		reenrolTries:  map[string]int{},
		// operator directives
		directiveVerifier: c.DirectiveVerifier, instanceID: c.InstanceID,
		dirAudit: newDirectiveAudit(c.DirectiveAuditPath), directiveExpiryMax: c.DirectiveExpiryMax,
		dirRate: newRateWindow(),
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
