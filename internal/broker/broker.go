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
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/wire"
)

const (
	// defaultGrantTTL is session-scale because claude reads the LLM token
	// (ANTHROPIC_AUTH_TOKEN) once at startup and lever-renew refreshes only
	// every 12h. Token TTL is a backstop: the per-call epoch/revoke gate is the
	// real security cut. A lower broker.grant_ttl must stay above the renew
	// interval.
	defaultGrantTTL  = 24 * time.Hour
	defaultTicketTTL = 10 * time.Minute
)

const (
	// ReservedLLMTool is the pseudo-tool for the LLM proxy: /request can mint it,
	// but it has no /mcp/llm/ route, is hidden from /tools, and is served only
	// by /llm.
	ReservedLLMTool = registry.ReservedLLMTool
	ReservedLLMOp   = registry.ReservedLLMOp
)

// RevocationState is the persisted revocation floor + per-agent revoke list.
type RevocationState struct {
	MinEpoch int      `json:"min_epoch"`
	Revoked  []string `json:"revoked"`
}

// Config assembles a Broker.
type Config struct {
	Identity    IdentityConfig
	Persistence PersistenceConfig
	LLM         LLMConfig
	Dispatch    DispatchConfig
	Directives  DirectiveConfig
	Chat        ChatConfig
	Timeouts    TimeoutConfig
	// AgentMessages is remote.agent_messages. Zero = off.
	AgentMessages AgentMessagesConfig
	// Files is remote.files: the file exchange of the chat page.
	Files FilesConfig

	// Log receives the audit decisions; nil ⇒ a discard logger.
	Log *slog.Logger
	// Version and ConfigHash (a digest of the broker-relevant config) are
	// reported by /epoch so apply can restart a stale broker instead of
	// reusing it. Empty = unreported.
	Version    string
	ConfigHash string
	// InstanceID (state.EnsureInstanceID) is reported by /epoch so a host
	// caller can tell this instance's broker from another on the same ports.
	// Empty is treated as "not ours". Not DirectiveConfig.InstanceID.
	InstanceID string
	// ToolSecret is the per-boot broker-to-tool secret. The gateway sends it
	// in X-Lever-Tool-Secret to first-party tools, which refuse a request
	// without it, so X-Lever-Caller is never trusted on a direct dial past the
	// broker. Empty ⇒ no header is sent.
	ToolSecret string
}

// IdentityConfig is the broker's keys, CA and policy: everything that decides
// who may obtain which capability. Zero GrantTTL/TicketTTL are defaulted.
type IdentityConfig struct {
	Keys     token.KeyPair
	CA       *ca.CA
	Tickets  *ca.TicketStore
	Rules    *rules.Policy
	Registry *registry.Registry
	// ManagerIdentity is the cert CN permitted to call the worker/msg
	// routes.
	ManagerIdentity string
	// ManagerSlug is the manager's scion agent slug (the app name), distinct
	// from the cert CN: scion knows the manager only by its slug. Empty
	// defaults to ManagerIdentity.
	ManagerSlug string
	// ManagerClaude is carried in every envelope the broker stages for the
	// manager. nil when none is configured.
	ManagerClaude *wire.Claude
	GrantTTL      time.Duration
	TicketTTL     time.Duration
}

// PersistenceConfig seeds the broker's persisted state at construction and
// writes it through on every change, so a restart never silently un-revokes
// or drops a directive.
type PersistenceConfig struct {
	Revocation RevocationState
	// PersistRevocation is called under the broker lock on every revocation
	// change. nil ⇒ no persistence.
	PersistRevocation func(RevocationState) error
	// PersistDirectives is called on every directive store mutation.
	Directives        DirectiveState
	PersistDirectives func(DirectiveState) error
}

// LLMConfig configures the /llm proxy.
type LLMConfig struct {
	// APIKey is the real Anthropic key, loaded host-side from the 0600
	// api_key_file. nil ⇒ no /llm route is served.
	APIKey []byte
	// Upstream is the proxy target; empty defaults to https://api.anthropic.com.
	// NEVER derived from a client request.
	Upstream string
}

// ManagerResumeCheck is the half of DispatchConfig.ManagerResumeGuard that
// runs after the bounce's resume (apply.ManagerResume in production).
type ManagerResumeCheck interface {
	// After reads back from the guest where each of the resumed manager's
	// tree mounts comes from; a mismatch suspends the manager and is the
	// error.
	After(ctx context.Context) error
	// AfterFailure is After for a resume that failed with cause: a
	// container it started anyway is checked, its error joined to cause.
	AfterFailure(ctx context.Context, cause error) error
	// Verified reports whether After could read the mounts back.
	Verified() bool
}

// DispatchConfig is the host-side worker dispatch and messaging wiring.
type DispatchConfig struct {
	// BrokerCAPEM and BrokerURL are copied into each worker's staged
	// bootstrap so it trusts the same CA and dials the same broker.
	Runtime     WorkerRuntime
	Workers     []WorkerSpec
	BrokerCAPEM string
	BrokerURL   string
	// InstanceProject is the single scion project (-g) of the manager and
	// all workers.
	InstanceProject string
	// WorkerToWorker enables worker→worker messaging; default false (deny).
	WorkerToWorker bool
	// VerifyAgentRole refuses to RESUME a worker whose hub record stores no
	// role while the installed scion resolves that to full hub authority
	// (hubapi.VerifyAgentRole). The role is immutable and `scion resume`
	// cannot set one, so resuming such a record would grant agent create,
	// lifecycle and secret-read. A fresh start is stamped `--role baseline`
	// itself. nil ⇒ no-op.
	VerifyAgentRole func(ctx context.Context, agent string) error
	// ResolveAgentID maps an agent slug to its hub agent id. /msg/list needs
	// it because the hub scopes the notification query to the controller
	// PAT's user, so the raw feed carries every agent's events. nil ⇒
	// /msg/list fails closed; the unfiltered feed would be a leak.
	ResolveAgentID func(ctx context.Context, agentSlug string) (string, error)
	// ResolveControllerSender returns the sender every lever message wears
	// ("user:" + the controller PAT's user). /message/verify looks such a
	// message up in the sent ledger only. nil ⇒ it answers unavailable.
	ResolveControllerSender func(ctx context.Context) (string, error)
	// BeginSession is called just before a worker is created fresh; its
	// commit records that start once scion created it. A resume records
	// nothing: the remote proxy lets a contact post to an agent only while
	// its last fresh start saw the skill on disk. nil ⇒ nothing is recorded.
	BeginSession func(agent string) (commit func() error)
	// LockManager takes, without waiting, the lock lever's own changes to
	// the manager record hold (a new conversation, a restore, up, apply, stop,
	// reload, destroy), so neither the certificate heal nor the hub token
	// reset acts on the manager in the middle of one; what names the holder
	// for the command it turns away. nil ⇒ no lock (the state directory is
	// inside the tree, where none of those that stop the record can run).
	LockManager func(what string) (unlock func(), err error)
	// AutoReenrol gates the natural-lapse healer: "all" | "manager" | "off"
	// (empty = all).
	AutoReenrol string
	// MaxRunningWorkers is broker.max_running_workers. 0 = no cap.
	MaxRunningWorkers int
	// Tree is the instance tree, the confinement anchor for staging. It is
	// agent-writable and the broker writes there as the operator, so
	// wire.Stage confines every write to it and refuses agent-planted
	// symlinks. Empty ⇒ the staging dir's parent (Broker.treePath).
	Tree string
	// LiveSettle is how long a started or resumed worker must STAY live
	// before its dispatch is reported up (scion.LiveBudget.Settle). Zero is
	// the first-observation gate.
	LiveSettle time.Duration
	// ManagerBootstrapDir is <tree>/.lever, where the manager's
	// bootstrap.json is staged. Empty disables manager healing.
	ManagerBootstrapDir string
	// ReadOnlyDirs is manager.read_only. Non-empty makes worker workspace
	// creation strict (ensureWorkspaceDir): no symlink on the path, and no
	// workspace that is, contains or lies in one of these, since a worker
	// mounts its dir read-write and would undo the protection.
	ReadOnlyDirs []string
	// RecordVolumes reads a worker's hub record volumes so a resume is
	// refused when the record holds a shared folder mount the config no
	// longer grants (refuseStaleShares). Wired only while shared_folders is
	// set; a failed read refuses (fail closed). nil skips it.
	RecordVolumes RecordVolumesFunc
	// LeftoverShareVolumes is the same reader for an instance with no shared
	// folder: a /shared mount still refuses the resume, but a failed read
	// lets it go on. nil skips it.
	LeftoverShareVolumes RecordVolumesFunc
	// SharedGuard checks that the running manager holds its tree plan
	// (config.App.ManagerTreeMounts) before any worker starts or resumes.
	// nil skips it. ErrSharedCheckUnavailable means the mounts could not be
	// read.
	SharedGuard func(ctx context.Context) error
	// ManagerResumeGuard runs apply's manager-resume checks for the re-enrol
	// bounce: before staging it refuses when the host tree or the record's
	// mounts are not as planned; the returned check runs after the resume.
	// nil: the broker never resumes the manager (fail closed).
	ManagerResumeGuard func(ctx context.Context) (ManagerResumeCheck, error)
	// RecordManagerBoot notes the guest boot the manager runs in after a
	// bounce whose checks passed, as lever's own resumes do, so a later
	// `lever up` does not take the boot for one the manager never ran in.
	// nil records nothing.
	RecordManagerBoot func(ctx context.Context)
	// Tickets stages a worker's enrolment envelope in the guest, outside the
	// instance tree: the manager mounts the whole tree, so nothing a worker
	// must redeem may be written there. nil ⇒ every dispatch fails closed.
	Tickets TicketStager
	// Purge deletes a worker's scion record and staged ticket (recycle
	// only). nil ⇒ every recycle fails closed.
	Purge WorkerPurger
	// ContainerRunning reads podman's own state for the worker's container,
	// which no agent can set (the hub's phase and status are agent-settable
	// text). A recycle refuses a running one. nil ⇒ only the hub record is
	// read.
	ContainerRunning func(ctx context.Context, worker string) (bool, error)
	// HubTokens heals an agent whose scion hub token expired, governed by
	// AutoReenrol. nil ⇒ no watch.
	HubTokens HubTokenHealer
}

// TimeoutConfig bounds request handling on the jail listener per route class.
// They are per route because a server-level ReadTimeout/WriteTimeout would cut
// a streamed /llm completion. /mcp/<name>/ and /worker/* deliberately have no
// handler bound (see JailHandler). Zero fields take the defaults.
type TimeoutConfig struct {
	// Body bounds request body delivery on every jail route, /llm included.
	Body time.Duration
	// Control bounds the JSON control routes, handler start to response.
	Control time.Duration
}

const (
	defaultJailBodyTimeout    = 30 * time.Second
	defaultJailControlTimeout = 30 * time.Second
)

func (tc TimeoutConfig) withDefaults() TimeoutConfig {
	return TimeoutConfig{
		Body:    cmp.Or(tc.Body, defaultJailBodyTimeout),
		Control: cmp.Or(tc.Control, defaultJailControlTimeout),
	}
}

// DirectiveConfig configures the operator-directive UDS admin channel.
type DirectiveConfig struct {
	// Verifier gates the channel: nil means directives are disabled and every
	// /directive/* route 404s.
	Verifier *opsig.Verifier
	// InstanceID is this instance's name; every signed directive op fails
	// closed when its Instance differs.
	InstanceID string
	// AuditPath is the bounded JSON-lines directive audit log. Empty ⇒ no-op.
	AuditPath string
	// ExpiryMax clamps a directive's expires_at, on top of opsig's 24h cap.
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
	beginSession   func(agent string) (commit func() error)
	lockManager    func(what string) (func(), error)
	tree           string
	readOnlyDirs   []string
	recordVolumes  RecordVolumesFunc
	// leftoverShareVolumes is best-effort (see DispatchConfig).
	leftoverShareVolumes RecordVolumesFunc
	sharedGuard          func(ctx context.Context) error
	managerResumeGuard   func(ctx context.Context) (ManagerResumeCheck, error)
	recordManagerBoot    func(ctx context.Context)
	// afterWorkspaceMkdir is a test seam between a strict workspace mkdir
	// and the walk that follows it.
	afterWorkspaceMkdir func()
	workers             map[string]WorkerSpec
	brokerCAPEM         string
	brokerURL           string
	// liveAttempts/liveInterval bound waitWorkerLive's poll (test seams).
	liveAttempts int
	liveInterval time.Duration
	liveSettle   time.Duration

	instanceProject string
	managerSlug     string // the manager's scion agent slug (app name), ≠ the cert CN
	managerClaude   *wire.Claude
	workerToWorker  bool

	// workerLocks serialises the read-phase-then-act paths of one worker:
	// two at once would both read "suspended" and both resume, or a bounce
	// undo a stop (lockWorker).
	workerLocksMu sync.Mutex
	workerLocks   map[string]chan struct{}
	// onWorkerLockWait is a test seam run when lockWorker starts to wait.
	onWorkerLockWait func(name string)
	// admitting holds the starts admitted and not yet returned (admitMu).
	maxRunningWorkers int
	admitMu           sync.Mutex
	admitting         map[string]int

	mu           sync.Mutex
	minEpoch     int
	revoked      map[string]bool
	persist      func(RevocationState) error
	bootstrapped bool // /bootstrap latch (one manager ticket per process)

	directives *DirectiveStore

	// reenrolNow is a test seam. reenrolMu guards the reenrol maps only.
	autoReenrol         string
	managerBootstrapDir string
	ticketStager        TicketStager
	purger              WorkerPurger
	containerRunning    func(ctx context.Context, worker string) (bool, error)
	recycleRate         *rateWindow // recycles per worker
	reenrolEvents       chan string
	reenrolNow          func() time.Time
	// reenrolLockWait bounds how long a heal waits for the worker lock
	// before it skips (reenrolLockWaitDefault when zero).
	reenrolLockWait time.Duration
	reenrolMu       sync.Mutex
	reenrolLast     map[string]time.Time
	reenrolTries    map[string]int
	// tokenHealLast is guarded by reenrolMu.
	hubTokens       HubTokenHealer
	tokenWatchEvery time.Duration
	tokenHealLast   map[string]time.Time

	directiveVerifier  *opsig.Verifier
	instanceID         string
	dirAudit           *directiveAudit
	directiveExpiryMax time.Duration
	dirRate            *rateWindow

	// msgPhaseTimeout bounds the phase read before a send
	// (msgPhaseTimeoutDefault when zero).
	msgPhaseTimeout time.Duration

	chatConfigured bool
	chatLedger     string // "" = no web post verifies
	webSenders     map[string]bool
	verifyRate     *rateWindow // every verify call
	verifyMissRate *rateWindow // verify calls that match no record
	chatUses       *chatUses
	sent           *sentRecord
	controller     *controllerSender

	agentMsgs   AgentMessagesConfig
	agentLedger *agentRecord
	contactRate *rateWindow // contacts + contact_message calls

	files      FilesConfig
	fileLedger *fileRecord
	filesRate  *rateWindow // list + share calls

	version         string
	configHash      string
	epochInstanceID string
	toolSecret      string
	timeouts        TimeoutConfig
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
		keys: id.Keys, ca: id.CA, tickets: id.Tickets, rules: id.Rules, reg: id.Registry,
		manager: id.ManagerIdentity, managerSlug: id.ManagerSlug, managerClaude: id.ManagerClaude,
		grantTTL: id.GrantTTL, ticketTTL: id.TicketTTL,
		log: c.Log, version: c.Version, configHash: c.ConfigHash, epochInstanceID: c.InstanceID, toolSecret: c.ToolSecret,
		timeouts: c.Timeouts.withDefaults(),
		minEpoch: pe.Revocation.MinEpoch, revoked: revoked, persist: pe.PersistRevocation,
		directives: newDirectiveStore(pe.Directives, pe.PersistDirectives, c.Log),
		apiKey:     llm.APIKey, llmUpstream: up,
		runtime: d.Runtime, workers: workers, brokerCAPEM: d.BrokerCAPEM, brokerURL: d.BrokerURL,
		instanceProject: d.InstanceProject, workerToWorker: d.WorkerToWorker,
		verifyRole: d.VerifyAgentRole, resolveAgentID: d.ResolveAgentID, beginSession: d.BeginSession, lockManager: d.LockManager, tree: d.Tree, readOnlyDirs: d.ReadOnlyDirs,
		recordVolumes: d.RecordVolumes, leftoverShareVolumes: d.LeftoverShareVolumes, sharedGuard: d.SharedGuard,
		managerResumeGuard: d.ManagerResumeGuard,
		recordManagerBoot:  d.RecordManagerBoot,
		liveAttempts:       defaultLiveAttempts, liveInterval: defaultLiveInterval, liveSettle: d.LiveSettle,
		maxRunningWorkers:   d.MaxRunningWorkers,
		autoReenrol:         cmp.Or(d.AutoReenrol, autoReenrolAll),
		managerBootstrapDir: d.ManagerBootstrapDir,
		ticketStager:        d.Tickets,
		purger:              d.Purge,
		containerRunning:    d.ContainerRunning,
		recycleRate:         newRateWindow(recycleRateLimit),
		reenrolEvents:       make(chan string, reenrolQueueDepth),
		reenrolNow:          time.Now,
		reenrolLast:         map[string]time.Time{},
		reenrolTries:        map[string]int{},
		hubTokens:           d.HubTokens,
		tokenWatchEvery:     tokenWatchInterval,
		tokenHealLast:       map[string]time.Time{},
		directiveVerifier:   dir.Verifier, instanceID: dir.InstanceID,
		dirAudit: newDirectiveAudit(dir.AuditPath), directiveExpiryMax: dir.ExpiryMax,
		dirRate:        newRateWindow(directiveRateLimit),
		chatConfigured: c.Chat.Configured, chatLedger: c.Chat.LedgerPath, webSenders: senderSet(c.Chat.WebSenders),
		verifyRate: newRateWindow(verifyRateLimit), verifyMissRate: newRateWindow(verifyMissLimit),
		chatUses:   newChatUses(c.Chat.UsedPath, time.Now()),
		sent:       &sentRecord{dir: c.Chat.SentLedgerDir},
		controller: &controllerSender{resolve: d.ResolveControllerSender},
		agentMsgs:  c.AgentMessages.withDefaults(), agentLedger: &agentRecord{dir: c.AgentMessages.LedgerDir},
		contactRate: newRateWindow(contactCallLimit),
		files:       c.Files.withDefaults(), fileLedger: &fileRecord{dir: c.Files.LedgerDir},
		filesRate: newRateWindow(filesCallLimit),
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

// persistLocked requires b.mu held.
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

// audit logs a decision; detail is "" for plain allows.
func (b *Broker) audit(op, caller, decision, detail string, kvs ...any) {
	// The audit log outlives the incident and a runtime error's detail can
	// echo a container env: redact here as the backstop for any detail that
	// did not pass through the masking scion client.
	detail = logging.Redact(detail)
	args := append([]any{"op", op, "caller", caller, "decision", decision, "detail", detail}, kvs...)
	b.log.Info("broker.decision", args...)
}
