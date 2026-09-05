// Package broker is the host-side capability authority and brokered-tool proxy.
// It mints per-agent signed capability tokens under the request/delegation
// policy and the tool registry, and gates every MCP call so real credentials
// never enter a container.
package broker

import (
	"cmp"
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

// Config assembles a Broker. Its groups map onto who fills them: brokerctl's
// BuildBroker fills Identity and LLM from the parsed app config, and its serve
// path fills Persistence, Dispatch and Directives plus the top-level fields
// from the state dir, the selected backend and the process environment.
type Config struct {
	Identity    IdentityConfig
	Persistence PersistenceConfig
	LLM         LLMConfig
	Dispatch    DispatchConfig
	Directives  DirectiveConfig

	// Log receives the audit decisions; nil ⇒ a discard logger.
	Log *slog.Logger
	// Version and ConfigHash identify this broker process: the binary's
	// version string and a digest of the broker-relevant configuration it was
	// started with. Reported by /epoch so apply's broker-reuse shortcut can
	// detect a stale broker (old binary or old tool set) and restart it
	// instead of silently reusing it (#19). Both optional (empty = unreported).
	Version    string
	ConfigHash string
}

// IdentityConfig is the broker's keys, CA and policy: everything that decides
// who may obtain which capability. Zero GrantTTL/TicketTTL are defaulted.
type IdentityConfig struct {
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
	GrantTTL    time.Duration
	TicketTTL   time.Duration
}

// PersistenceConfig seeds the broker's persisted state at construction and
// writes it through on every change, so a restart never silently un-revokes
// or drops a directive.
type PersistenceConfig struct {
	// Revocation seeds the epoch floor + revoke list (loaded from the state dir).
	Revocation RevocationState
	// PersistRevocation is called (under the broker lock) whenever revocation
	// state changes, to write it through to the state dir. nil ⇒ no persistence.
	PersistRevocation func(RevocationState) error
	// Directives seeds the persistent operator-directive store; PersistDirectives
	// writes it through on every mutation. Modeled on Revocation/PersistRevocation.
	Directives        DirectiveState
	PersistDirectives func(DirectiveState) error
}

// LLMConfig configures the /llm proxy.
type LLMConfig struct {
	// APIKey is the real Anthropic Console key bytes (loaded host-side from the
	// 0600 api_key_file by brokerctl). nil ⇒ no /llm route is served.
	APIKey []byte
	// Upstream is the proxy target; empty defaults to https://api.anthropic.com.
	// Set by tests to a fake upstream. NEVER derived from a client request.
	Upstream string
}

// DispatchConfig is the host-side worker dispatch and messaging wiring.
type DispatchConfig struct {
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
	// LiveSettle is how long a freshly started or resumed worker must STAY
	// live before its dispatch is reported as up (scion.LiveBudget.Settle,
	// lever#31). brokerctl sets the production value; zero — the default a
	// test gets — is the first-observation gate.
	LiveSettle time.Duration
	// ManagerBootstrapDir is the host path to <tree>/.lever — where the
	// MANAGER's bootstrap.json is staged (workers carry theirs in WorkerSpec).
	// Empty disables manager healing (audited as an error on lapse).
	ManagerBootstrapDir string
}

// DirectiveConfig configures the operator-directive UDS admin channel.
type DirectiveConfig struct {
	// Verifier gates the channel: nil means directives are disabled and every
	// /directive/* route 404s.
	Verifier *opsig.Verifier
	// InstanceID is this instance's name, checked against Statement.Instance /
	// Envelope.Instance on every signed directive op (opsig.ParseStatement /
	// ParseEnvelope's instance-mismatch fail-closed check).
	InstanceID string
	// AuditPath is the bounded JSON-lines audit log for the directive channel.
	// Empty ⇒ dirAudit.append is a no-op (still non-nil, so handlers never
	// nil-check it).
	AuditPath string
	// ExpiryMax clamps how far in the future a submitted directive's
	// expires_at may sit (on top of opsig's own 24h hard cap).
	ExpiryMax time.Duration
}

// Broker is the running capability authority + brokered-tool proxy.
type Broker struct {
	keys      token.KeyPair
	ca        *ca.CA
	tickets   *ca.TicketStore
	rules     *rules.Policy
	reg       *registry.Registry
	manager   string
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
	// shrink them per instance (like reenrolNow). liveSettle is the hold that
	// follows (DispatchConfig.LiveSettle).
	liveAttempts int
	liveInterval time.Duration
	liveSettle   time.Duration

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
	id, pe, llm, d, dir := c.Identity, c.Persistence, c.LLM, c.Dispatch, c.Directives
	if id.GrantTTL <= 0 {
		id.GrantTTL = defaultGrantTTL
	}
	if id.TicketTTL <= 0 {
		id.TicketTTL = defaultTicketTTL
	}
	if id.ManagerSlug == "" {
		id.ManagerSlug = id.ManagerIdentity
	}
	if c.Log == nil {
		// Default to a no-op logger so audit() never nil-panics when a caller
		// builds a Config without one. Every decision path audits, so a nil
		// log would otherwise crash the first request.
		c.Log = slog.New(slog.DiscardHandler)
	}
	revoked := make(map[string]bool, len(pe.Revocation.Revoked))
	for _, a := range pe.Revocation.Revoked {
		revoked[a] = true
	}
	up, _ := url.Parse(cmp.Or(llm.Upstream, "https://api.anthropic.com")) // operator/test-controlled, validated at serve time
	workers := make(map[string]WorkerSpec, len(d.Workers))
	for _, g := range d.Workers {
		workers[g.Name] = g
	}
	return &Broker{
		// identity, keys and policy
		keys: id.Keys, ca: id.CA, tickets: id.Tickets, rules: id.Rules, reg: id.Registry,
		manager: id.ManagerIdentity, managerSlug: id.ManagerSlug,
		grantTTL: id.GrantTTL, ticketTTL: id.TicketTTL,
		log: c.Log, version: c.Version, configHash: c.ConfigHash,
		// persisted state
		minEpoch: pe.Revocation.MinEpoch, revoked: revoked, persist: pe.PersistRevocation,
		directives: newDirectiveStore(pe.Directives, pe.PersistDirectives, c.Log),
		// llm proxy
		apiKey: llm.APIKey, llmUpstream: up,
		// worker dispatch and messaging
		runtime: d.Runtime, workers: workers, brokerCAPEM: d.BrokerCAPEM, brokerURL: d.BrokerURL,
		instanceProject: d.InstanceProject, workerToWorker: d.WorkerToWorker,
		verifyRole: d.VerifyAgentRole, resolveAgentID: d.ResolveAgentID, tree: d.Tree,
		liveAttempts: defaultLiveAttempts, liveInterval: defaultLiveInterval, liveSettle: d.LiveSettle,
		autoReenrol:         cmp.Or(d.AutoReenrol, autoReenrolAll),
		managerBootstrapDir: d.ManagerBootstrapDir,
		reenrolEvents:       make(chan string, reenrolQueueDepth),
		reenrolNow:          time.Now,
		reenrolLast:         map[string]time.Time{},
		reenrolTries:        map[string]int{},
		// operator directives
		directiveVerifier: dir.Verifier, instanceID: dir.InstanceID,
		dirAudit: newDirectiveAudit(dir.AuditPath), directiveExpiryMax: dir.ExpiryMax,
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

// audit logs a decision; detail is "" for plain allows. kvs are optional
// extra slog key/value pairs (token id, matched rule, minted claims).
func (b *Broker) audit(op, caller, decision, detail string, kvs ...any) {
	args := append([]any{"op", op, "caller", caller, "decision", decision, "detail", detail}, kvs...)
	b.log.Info("broker.decision", args...)
}
