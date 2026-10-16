package apply

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wait"
	"github.com/stevegeek/lever/internal/wire"
)

// ErrBootstrapLatched is returned by MintManagerBootstrap when the broker's
// single-use /bootstrap latch is already consumed (HTTP 403). The mint step
// tolerates it; a broker restart reopens the latch.
var ErrBootstrapLatched = errors.New("broker /bootstrap latch already consumed")

// RetryBudget bounds one of apply's polls: Attempts tries, Interval apart.
// A zero value means the production default (see Deps).
type RetryBudget struct {
	Attempts int
	Interval time.Duration
	// Settle is how long a record must stay live after it is first seen live
	// before the manager counts as up. Only the manager liveness budget reads it.
	Settle time.Duration
}

// or fills unset Attempts and Interval from def. Only the all-zero budget
// inherits def's Settle: a zero settle is a legitimate explicit choice.
func (b RetryBudget) or(def RetryBudget) RetryBudget {
	if b == (RetryBudget{}) {
		return def
	}
	if b.Attempts == 0 {
		b.Attempts = def.Attempts
	}
	if b.Interval == 0 {
		b.Interval = def.Interval
	}
	return b
}

// spec is b as a wait of b.Attempts probes b.Interval apart.
func (b RetryBudget) spec(name string) wait.Spec {
	return wait.Attempts(name, b.Attempts, b.Interval)
}

// budgetOf is the RetryBudget of a fixed-interval wait.Spec.
func budgetOf(s wait.Spec) RetryBudget {
	return RetryBudget{Attempts: s.MinAttempts, Interval: s.Interval}
}

// defaultBrokerStartRetry absorbs the runtime broker's asynchronous
// registration with the hub: a start-manager that runs too soon gets "no
// runtime brokers available" although the hub is up.
var defaultBrokerStartRetry = budgetOf(wait.BrokerUnavailable)

// defaultManagerLiveRetry bounds waitManagerLive's post-start poll. scion
// reports the record running before the harness has run a line, and every
// harness death observed (a task past the tmux cap, claude --continue with no
// conversation) landed within three seconds of that; the 10 s settle catches
// those with margin but is still probabilistic. It applies to a gate that never
// saw claude's pane; after that the window is harnessSettle, counted from the
// pane.
var defaultManagerLiveRetry = RetryBudget{
	Attempts: wait.ManagerRunning.MinAttempts, Interval: wait.ManagerRunning.Interval, Settle: wait.ManagerSettle,
}

// defaultPhaseSettleRetry bounds settleManagerPhase. It errs long: exhausting
// it is a loud failure the operator must act on, while an early settle costs
// one more list.
var defaultPhaseSettleRetry = budgetOf(wait.ManagerPhaseSettle)

// transitionalPhases are scion record phases (pkg/agent/state/state.go) that a
// record only passes through. planManager has no verb for them and a
// snapshot of one says nothing about where the record ends up, so apply waits
// for it to settle (settleManagerPhase) instead of acting on it.
//
// "resumed" is not in scion's hub phase enum: it is the local runtime's interim
// status after scion resume, until the container's sciontool reports
// "running", and it settles on its own.
var transitionalPhases = []string{"created", "provisioning", "cloning", "starting", "stopping", "resumed"}

// apiKeyPlaceholder is the ANTHROPIC_API_KEY Hub secret for api-key instances.
// It is not a credential: it only satisfies scion's start-time auth gate. claude
// sends it as x-api-key to the broker /llm, which strips it and injects the real
// key host-side. It is shaped like an Anthropic key in case scion checks the
// format.
const apiKeyPlaceholder = "sk-ant-placeholder0lever0broker0injects0the0real0key0do0not0use000000000000000000000000"

// BootstrapMaterial is what the manager's lever-agent consumes to enrol.
type BootstrapMaterial = wire.Bootstrap

// run is one apply execution: the config it converges, its collaborators, and
// the cross-step state the plan carries.
type run struct {
	app                                   *config.App
	d                                     Deps
	brokerStart, managerLive, phaseSettle RetryBudget
	// minted records whether this run minted fresh bootstrap material rather
	// than tolerating a spent latch (ErrBootstrapLatched); start-manager's create
	// path depends on it.
	minted bool
	// progress is the start-manager progress line; nil outside that window.
	progress *Progress
	// fresh is PlanOpts.Fresh: discard any present manager record at
	// start-manager.
	fresh bool
	// attach is PlanOpts.Attach: the gate for a manager this run created ends once
	// claude's pane is alive; a resume's gate still waits for claude's session
	// start.
	attach bool
	// freshNoConv is PlanOpts.FreshWithoutConversation.
	freshNoConv bool
	// unattended is set while start-manager makes the fresh start of a manager
	// with no conversation: the create records its session through
	// unattendedBegin, never Deps.BeginSession. A nil unattendedBegin records
	// nothing.
	unattended        bool
	unattendedBegin   func() (commit func() error, err error)
	unattendedRefused bool
	// managerCreated records that this run created the manager record ("already
	// exists" does not count). Only such a record is known to carry the
	// manager.read_only mounts; any other is probed (warnManagerTreeMounts).
	managerCreated bool
	// brokerOnline records that the scion-server step already saw the runtime
	// broker online, so start-manager need not wait for it again.
	brokerOnline bool
	// subject prefixes the resume checks' messages; empty means "start-manager".
	subject string
}

func (r *run) subjectName() string {
	if r.subject != "" {
		return r.subject
	}
	return "start-manager"
}

// StageBootstrapMaterial writes m as the manager's one-time enrolment ticket to
// treeDir/.lever/bootstrap.json (0600), where lever-agent boot reads it. It is
// the only staging path, shared by the mint step and Deps.RearmBootstrap.
func StageBootstrapMaterial(treeDir string, m BootstrapMaterial) error {
	// treeDir is the mount point, the one component the agent cannot replace.
	// Everything below it is agent-writable, so wire.Stage refuses to follow a
	// symlink planted at .lever.
	return wire.Stage(treeDir, ".lever", m)
}

// Deps are the executor's collaborators, injected so Run is testable offline.
// Scion runs in the jail. Every func field except ReadCred, ImageTagPolicy,
// InspectContainerMounts and CheckLiveMounts is required: Run refuses
// (Deps.check) before its first step when one is nil, so a wiring gap fails
// loudly instead of skipping a step.
type Deps struct {
	LoadImage func(ctx context.Context, imageRef string) error
	// ImageLoaded reports whether the jail already holds imageRef at the host's
	// image ID, so a re-apply skips the multi-GB re-stream. It is fail-open (false
	// on any uncertainty): a wrong answer costs a redundant load, never a wrongly
	// skipped one.
	ImageLoaded func(ctx context.Context, imageRef string) bool
	// LoadImageTar and ImageLoadedTar are the same pair for an image shipped as a
	// docker archive (Step.TarPath); neither touches host docker. Same fail-open
	// contract. The load takes ImageTagPolicy as its allowTag.
	LoadImageTar   func(ctx context.Context, imageRef, tarPath string, allowTag func(ref string) error) error
	ImageLoadedTar func(ctx context.Context, imageRef, tarPath string) bool
	// ImageTagPolicy is applied to every tag an image_tar archive carries before
	// any byte is streamed into the jail: an archive can hold several images and
	// podman load imports all of them, so the config ref passing the registry
	// allowlist proves nothing about the rest. Optional: nil means no policy.
	ImageTagPolicy func(ref string) error
	// PruneImages removes dangling images after a load, so a rebuilt image does
	// not grow the grow-only jail disk by a full image. Best-effort.
	PruneImages func(ctx context.Context) error
	// WarmImage starts imageRef once under scion's user-namespace mapping so the
	// first agent start finds podman's id-mapped layer copy built. It runs on an
	// image the jail held already only when no finished warm-up of the same
	// inputs is recorded (Fingerprints), bounded by imageWarmTimeout.
	// Best-effort.
	WarmImage func(ctx context.Context, imageRef string) error
	// Fingerprints records what a step last converged on, so a converge skips
	// work that step's own check still vouches for. It never gates a fence.
	// nil skips nothing.
	Fingerprints StepFingerprints
	Scion        *scion.Client
	ReadCred     func(path string) (string, error) // nil ⇒ defaultReadCred
	JailMount    string                            // jail path where app.Tree is bind-mounted (e.g. "/lever"); "" disables translation
	// HubSessionSecret is the hub's session-cookie signing key, passed to every
	// hub start (HubServerOpts) so browser sessions survive hub restarts. It is
	// argv-only: a running hub keeps its old key, and apply does not restart the
	// hub to adopt it, since a restart drops every agent's hub connection and would
	// force the logout the key prevents. "" omits the flag (scion then uses a
	// per-boot random key).
	HubSessionSecret string
	StartBroker      func(ctx context.Context) error
	BrokerHealthy    func(ctx context.Context) error
	// EnsureHubIdentity runs the hub-identity step against the live hub: the
	// admin and controller identities, scion's experiments lever keeps off,
	// the project, the controller PAT and the remote logins (invited and bound
	// before they first sign in). It is idempotent, never lists containers,
	// and runs while agents run.
	EnsureHubIdentity func(ctx context.Context) error
	// HubAuthEnv pins the hub's auth configuration on every hub start (see
	// scion.ServerOpts.Env). It also carries the assertion key's fingerprint,
	// so a hub started before a key change is detected and restarted.
	HubAuthEnv map[string]string
	// HubConduitEnv reads the running hub's conduit environment, which
	// scion's RunningHub does not report, so HubRestartReason compares the
	// conduit policy HubAuthEnv carries (HubEnv) too. Optional: nil reads
	// none.
	HubConduitEnv func(ctx context.Context) (map[string]string, error)
	// EnsureProjectProvider runs before each hub link (whose agent sync needs a
	// broker) and again when the project settles: when the instance's hub
	// project lists no runtime broker, it links the jail's broker to it as
	// lever-admin, or fails. A project the hub does not list is left alone.
	EnsureProjectProvider func(ctx context.Context, jailWorkspacePath string) error
	// WaitBrokerReady blocks until the scion runtime broker is registered and
	// online. The workstation daemon brings up its Hub API and its runtime broker
	// separately, so without it the first create or resume races the broker's
	// async registration. It is fail-soft (nil on timeout); the start path's
	// broker-unavailable retry backstops it. It reports whether it saw the broker
	// online so start-manager can skip its own wait.
	WaitBrokerReady func(ctx context.Context, project string) (online bool, err error)
	// BrokerOnline asks the hub once whether it lists an online runtime
	// broker; seen describes what it listed, for a timeout's evidence. It is
	// the probe of the hard wait after lever starts or restarts the hub, so
	// it must work before hub-identity has minted any token.
	BrokerOnline         func(ctx context.Context) (online bool, seen string, err error)
	MintManagerBootstrap func(ctx context.Context) (BootstrapMaterial, error)
	// RearmBootstrap restarts the broker to re-arm its single-use /bootstrap latch
	// (the CA and signing keys persist, so agent certs and capability tokens
	// survive), then mints and stages fresh bootstrap material. start-manager's
	// create path calls it when this apply minted none.
	RearmBootstrap func(ctx context.Context) error
	// EnsureHubAuth installs the assertion public key in the guest and
	// converges the hub's auth settings before the hub starts. It reports
	// whether a running hub must restart to read them.
	EnsureHubAuth func(ctx context.Context) (bool, error)
	// BeforeHubRestart, when set, writes the database settings the hub reads
	// only at start (scion's experiments) into a hub the scion-server step is
	// about to restart, so that one restart takes them. An error is logged:
	// hub-identity converges them after the restart and restarts again if it
	// must.
	BeforeHubRestart func(ctx context.Context) error
	// EnsureScionTelemetry converges the telemetry block of the guest's
	// ~/.scion/settings.yaml on config scion.telemetry and reports whether it
	// changed. A change is only logged: the runtime broker reads the block at every
	// agent start, but a running container keeps its env and the hub caches the
	// block at startup, and lever does not bounce either for telemetry.
	EnsureScionTelemetry func(ctx context.Context) (bool, error)
	// EnsureAgentTemplate puts lever's overlay template in front of scion's stock
	// default so new agents do not launch with --system-prompt '# Placeholder',
	// which replaces Claude Code's whole built-in system prompt. projectDir is
	// jail-side; project scope is the only settings scope that wins. It reports
	// whether it changed anything. scion stages an agent's system prompt only when
	// its home is created, so this governs agents created from now on.
	EnsureAgentTemplate func(ctx context.Context, projectDir string) (bool, error)
	// BeginSession, when set, is called before the manager is created and returns
	// what records that fresh start (package sessionrec): it notes the skill on
	// disk now, and commit writes the record once scion created the agent. It is
	// never called on a resume, and commit never runs when scion reports the agent
	// already existed, so a resumed conversation is never recorded as fresh. The
	// remote proxy lets a contact post to an agent only after such a record. A
	// commit error is logged, not fatal: without the record contacts are refused
	// (fail closed). Nil records nothing.
	BeginSession func(agent string) (commit func() error)
	// BeginUnattendedSession is BeginSession for a fresh start lever makes on its
	// own, for a manager with no conversation to continue. The agent can bring
	// that start about and its tree holds the skill the record names, so such a
	// start must not re-baseline the session on a skill the agent wrote. It is
	// called before the record is deleted; when the skill on disk is not the one
	// the last recorded session started with, it records at once a session
	// matching no skill (UnattendedSession.Refused), so contacts stay refused even
	// if the old skill text comes back. UnattendedSession.Begin runs after the
	// delete, when the old container can no longer write the tree: it re-reads the
	// skill and returns a commit that records the new session only over that same
	// skill. An error from either means the refusing record could not be written,
	// and lever then does not start or create the manager. When BeginSession is
	// set and this is nil, lever does not start it either.
	BeginUnattendedSession func(agent string) (UnattendedSession, error)
	// BackupManagerConversation, when set, runs under PlanOpts.Fresh just before
	// the manager record rec is deleted (scion deletes its home and conversation
	// with it). An error stops the fresh start with nothing deleted. Nil backs
	// nothing up. It also runs, as a second guard, before the fresh start that
	// replaces a resume with no conversation (PlanOpts.FreshWithoutConversation).
	BackupManagerConversation func(ctx context.Context, rec *scion.Agent) error
	// ManagerConversationEmpty proves, from the guest, that the manager's agent
	// home holds no conversation: no transcript with content in .claude/projects,
	// no link or special file there, and no other projects directory beside it
	// (convbackup.Guest.Transcripts). nil means proven; any error (the reason, made
	// safe to print) means not proven, and start-manager then starts nothing fresh
	// on its own. A nil func never proves it.
	ManagerConversationEmpty func(ctx context.Context) error
	// InspectContainerMounts reads a jail container's mounts. It is used to warn
	// when a kept or resumed manager lacks the manager.read_only mounts, which only
	// a create adds, and to find mounts the record holds that the config dropped or
	// whose directory is gone (checkStaleTreeMounts). nil: the warning says it
	// could not check.
	InspectContainerMounts func(ctx context.Context, ref string) ([]jail.Mount, error)
	// CheckLiveMounts checks a running container's mounts from the guest side
	// (mountinfo plus device and inode; nothing runs inside the agent-controlled
	// container): that a read_only entry is mounted read-only and was not replaced
	// on the host since create. nil: the warning says it could not check.
	CheckLiveMounts LiveMountChecker
	// ProbeContainerDevice reports whether path is a character device in a
	// running container. It is used to warn when nested_virt is on and the
	// manager lacks /dev/kvm (the podman drop-in reaches containers only at
	// create). Optional.
	ProbeContainerDevice func(ctx context.Context, ref, path string) (bool, error)
	// ReadClaudeSettings reads back the Claude config in a running agent
	// container, to warn when the live manager holds other claude_settings or
	// after_compact_note than the config (they apply only at start). Optional.
	ReadClaudeSettings func(ctx context.Context, ref string) (jail.ClaudeDelivered, error)
	// BackgroundSessions counts stale Claude background-session records in an
	// agent container: the first cause the manager gate names when the harness
	// dies and the container still runs (withHarnessDeathHint). Only counts come
	// back. Optional.
	BackgroundSessions func(ctx context.Context, ref string) (jail.BackgroundRecords, error)
	// HarnessLogTail reads the tail of an agent container's log, the fallback when
	// BackgroundSessions names no cause. The tail is agent-controlled: it is only
	// matched for fixed phrases (HarnessDeathHint) and never printed. Optional.
	HarnessLogTail func(ctx context.Context, ref string) (string, error)
	// RecordVolumes reads the extra mounts the hub record of agent was created
	// with, as jail mounts: checkStaleTreeMounts' fallback when the manager has no
	// container to inspect. nil (or an error): no fallback.
	RecordVolumes func(ctx context.Context, project, agent string) ([]jail.Mount, error)
	// AgentSession reads a running agent's session from inside its container so
	// start-manager can heal an expired hub token or a "stopped" phase over a live
	// harness (HealAgentSession) before it converges. nil: no heal.
	AgentSession AgentSessionProbe
	// ContainerRunning asks podman, not the hub heartbeat, whether an agent
	// container runs. The manager gate reads it when claude's pane is not alive, to
	// tell a stopped container (a failed pre-start hook) from a booting one, and as
	// its fallback when the pane cannot be probed.
	ContainerRunning func(ctx context.Context, ref string) (bool, error)
	// ContainerStartedBeforeBoot reports whether an agent container last started
	// before the guest's current boot. The guest-restart recovery reads it only
	// when GuestBoot kept no boot id. nil: no fallback.
	ContainerStartedBeforeBoot func(ctx context.Context, ref string) (bool, error)
	// GuestBoot reads the guest's boot id and keeps the one the manager last ran
	// live in, so a record left "running" by a hard VM kill is suspended and
	// resumed instead of refused. Optional.
	GuestBoot GuestBoot
	// HarnessWait bounds how long the manager gate waits, once the record reads
	// live, for claude's tmux pane (the pre-start hook runs first). Zero means
	// 2 min.
	HarnessWait time.Duration
	// ReadyWait bounds how long the manager gate waits, once claude's pane is
	// alive, for claude's session-start marker. Zero means 2 min.
	ReadyWait time.Duration
	// clock and progressTicker drive the progress line; nil means time.Now and a
	// real ticker.
	clock          func() time.Time
	progressTicker func(time.Duration) (<-chan time.Time, func())
	// AgentRevoked reports an agent the broker has revoked, so the session heal
	// never resets a revoked agent's hub token. nil: none known.
	AgentRevoked func(agent string) bool
	// StartRemoteProxy starts, or confirms running, the daemonized lever remote
	// serve proxy. Plan includes its step only when remote access is on.
	StartRemoteProxy func(ctx context.Context) error
	// StopRemoteProxy stops the remote proxy by pidfile, tolerating an absent or
	// stale one. Run calls it whenever remote access is off, so a proxy left by a
	// prior apply stops serving traffic the config no longer authorizes.
	StopRemoteProxy func(ctx context.Context) error
	// FenceRemoteLogins runs before every step: it records what each remote
	// login may now do, which a running proxy checks on every request, and
	// stops the proxy when remote access is off. A removed or narrowed login
	// so loses the proxy at once, whatever a later step does: nothing an
	// agent can disturb (the hub, the controller's address) stands between
	// the operator's edit and the revocation.
	FenceRemoteLogins func(ctx context.Context) error
	// RemoveJailFile removes a regular file at a jail-absolute path through the
	// jail's own view, so removing the stale .scion marker cannot race the in-jail
	// scion init across VirtioFS (a host-side unlink is not promptly visible in the
	// guest's directory cache). Must not remove directories.
	RemoveJailFile func(ctx context.Context, jailPath string) error
	// RemoveScionProjectConfigs removes stale ~/.scion/project-configs
	// registrations whose workspace_path is jailWorkspacePath before the
	// register-project step re-inits; otherwise every scion init leaves another
	// registration behind.
	RemoveScionProjectConfigs func(ctx context.Context, jailWorkspacePath string) error
	// ScionProjectRegistered reports whether jailWorkspacePath already has exactly
	// one valid scion registration (one project-configs entry and the in-tree
	// marker). true skips the destructive clean and init, which would orphan a
	// resumable agent record. A query error falls through to the destructive path
	// (fail-open: zero, duplicate or torn registrations need it).
	ScionProjectRegistered func(ctx context.Context, jailWorkspacePath string) (bool, error)
	// ProjectLinked reports whether the jail's scion settings for
	// jailWorkspacePath say the project is hub-linked to the project ID
	// hub-identity recorded. A sound registration that is not linked is
	// re-linked, never settled as is.
	ProjectLinked func(ctx context.Context, jailWorkspacePath string) (bool, error)
	// StripProjectSharedDirs removes scion's default scratchpad shared dir from
	// the hub's record of the project. The hub stamps it on every new project and
	// mounts it read-write into every agent: a writable channel between the manager
	// and every worker that breaks lever's subtree isolation, and lever's
	// file/SQLite hub cannot turn the default off. It runs on both register paths,
	// since an older project still carries the mount.
	//
	// A failure is fatal to apply: agents silently sharing a writable directory is
	// worse than a loud bring-up failure. The project name is the workspace
	// basename, as ensureControllerPAT passes to hub token create.
	StripProjectSharedDirs func(ctx context.Context, projectName string) error
	// EnsureAgentRoleCeiling writes the project's max and default agent role so
	// the hub refuses any create above the role lever stamps (the hub-side
	// counterpart of --role). It runs on both register paths, and a failure is
	// fatal for the same reason as the strip: a ceiling the operator believes the
	// hub enforces but it does not is a silent regression.
	EnsureAgentRoleCeiling func(ctx context.Context, projectName string) error
	// RepairScionHubEndpoint rewrites the project's recorded hub endpoint when it
	// differs from the real hub's. A registration linked against a hub on another
	// port keeps that endpoint, since the register step's re-init is skipped when
	// the registration is sound. lever passes the endpoint explicitly, so a stale
	// one only breaks where scion runs bare in the jail (lever attach).
	RepairScionHubEndpoint func(ctx context.Context, jailWorkspacePath string) error
	// VerifyAgentRole gates keeping an existing agent record: it errors when the
	// hub's record stores no authorization role while the installed scion
	// understands roles (a record created before scion had roles; the role is set
	// only at create and is immutable).
	//
	// scion resolves an unset stored role to full (agent create, agent lifecycle,
	// project-secret-read) at dispatch and on every token refresh, and scion
	// resume takes no --role, so resuming such a record would silently promote it
	// past the ceiling lever's model assumes. lever cannot repair the record: the
	// hub has no route to set a stored role.
	VerifyAgentRole func(ctx context.Context, project, agent string) error
	// Log surfaces a user-facing progress or warning line, such as the --fresh
	// discard notice, which must reach the user.
	Log func(format string, args ...any)
	// StepTail, when set, names where a failure of a step of this kind is
	// explained: the returned function runs only on failure and gives the source's
	// name and its last lines. nil, or a nil function, shows no tail.
	StepTail func(kind StepKind) func() (string, []string)
	// BrokerStartRetry bounds the retry over scion's runtime-broker registration
	// race on manager start, resume and list; ManagerLiveRetry bounds the
	// post-start liveness poll. Zero values take the defaults.
	BrokerStartRetry RetryBudget
	ManagerLiveRetry RetryBudget
	// PhaseSettleRetry bounds the wait for a manager record in a transitional
	// phase to settle. Zero takes the default.
	PhaseSettleRetry RetryBudget
	// StaleContainer is podman in the jail, for clearing a hub stop claim that
	// only a container removal releases (resumeClearingStopClaim), and for
	// telling a running record whose container is gone or exited
	// (containerGoneUnder). nil: lever removes nothing and names the manual
	// fix, and keeps such a record for the gate to refuse.
	StaleContainer StaleContainerOps
	// StopClaimRetry bounds the wait for the hub to release that claim; the
	// interval doubles up to stopClaimMaxInterval. Zero takes the default.
	StopClaimRetry RetryBudget
	// ManagerLock is the lifecycle lock EnsureManager takes before it reads or
	// changes the manager. nil: EnsureManager refuses.
	ManagerLock ManagerLock
	// ManagerVerbs, when set, takes the place of Scion for the verbs on the
	// manager's record. nil means Scion.
	ManagerVerbs ManagerScion
	// ManagerPhaseWait bounds the wait for a suspend or stop of the manager to
	// read in the hub. Zero takes the default.
	ManagerPhaseWait RetryBudget
}

func (d Deps) check() error {
	required := []struct {
		name string
		nil_ bool
	}{
		{"LoadImage", d.LoadImage == nil},
		{"ImageLoaded", d.ImageLoaded == nil},
		{"LoadImageTar", d.LoadImageTar == nil},
		{"ImageLoadedTar", d.ImageLoadedTar == nil},
		{"PruneImages", d.PruneImages == nil},
		{"WarmImage", d.WarmImage == nil},
		{"Scion", d.Scion == nil},
		{"StartBroker", d.StartBroker == nil},
		{"BrokerHealthy", d.BrokerHealthy == nil},
		{"EnsureHubIdentity", d.EnsureHubIdentity == nil},
		{"EnsureProjectProvider", d.EnsureProjectProvider == nil},
		{"WaitBrokerReady", d.WaitBrokerReady == nil},
		{"BrokerOnline", d.BrokerOnline == nil},
		{"MintManagerBootstrap", d.MintManagerBootstrap == nil},
		{"RearmBootstrap", d.RearmBootstrap == nil},
		{"EnsureHubAuth", d.EnsureHubAuth == nil},
		{"EnsureScionTelemetry", d.EnsureScionTelemetry == nil},
		{"EnsureAgentTemplate", d.EnsureAgentTemplate == nil},
		{"StartRemoteProxy", d.StartRemoteProxy == nil},
		{"StopRemoteProxy", d.StopRemoteProxy == nil},
		{"FenceRemoteLogins", d.FenceRemoteLogins == nil},
		{"RemoveJailFile", d.RemoveJailFile == nil},
		{"RemoveScionProjectConfigs", d.RemoveScionProjectConfigs == nil},
		{"ScionProjectRegistered", d.ScionProjectRegistered == nil},
		{"ProjectLinked", d.ProjectLinked == nil},
		{"StripProjectSharedDirs", d.StripProjectSharedDirs == nil},
		{"RepairScionHubEndpoint", d.RepairScionHubEndpoint == nil},
		{"VerifyAgentRole", d.VerifyAgentRole == nil},
		{"Log", d.Log == nil},
	}
	for _, r := range required {
		if r.nil_ {
			return fmt.Errorf("apply: Deps.%s is not set", r.name)
		}
	}
	return nil
}

// StepFingerprints is the lifecycle record's step fingerprints
// (state.Lifecycle). inputs is a digest of everything the step's result
// depends on.
type StepFingerprints interface {
	Matches(step, inputs string) bool
	Record(step, inputs string) error
	Forget(step string) error
}

// UnattendedSession is Deps.BeginUnattendedSession's answer.
type UnattendedSession struct {
	// Refused, when set, says why the start is not recorded as a new
	// session: a session matching no skill was recorded already, and
	// contacts stay refused until the operator's --fresh.
	Refused error
	// Begin runs right before the create, after the delete. Its commit
	// records the new session, or returns why it does not. Its error means
	// a refusing record could not be written, and the manager is not
	// created. Nil records nothing.
	Begin func() (commit func() error, err error)
}

// Run executes the bring-up Plan for app. The jail is already up: the CLI needs
// it to build Deps.
func Run(ctx context.Context, app *config.App, d Deps, opts PlanOpts) error {
	if err := d.check(); err != nil {
		return err
	}
	r := &run{app: app, d: d,
		brokerStart: d.BrokerStartRetry.or(defaultBrokerStartRetry),
		managerLive: d.ManagerLiveRetry.or(defaultManagerLiveRetry),
		phaseSettle: d.PhaseSettleRetry.or(defaultPhaseSettleRetry),
		fresh:       opts.Fresh,
		attach:      opts.Attach,
		freshNoConv: opts.FreshWithoutConversation,
	}
	if err := d.FenceRemoteLogins(ctx); err != nil {
		return fmt.Errorf("remote-proxy: %w", err)
	}
	steps := Plan(app, opts)
	rep := progress.From(ctx)
	rep.Group("apply", len(steps))
	// broker-up and load-image run alongside the steps after them (see runSteps).
	if err := r.runSteps(ctx, steps); err != nil {
		return err
	}
	rep.Group("", 0)
	// Plan omits the remote-proxy step when remote access is off, so a proxy left
	// running by a prior apply is converged off here.
	if !app.RemoteEnabled() {
		if err := d.StopRemoteProxy(ctx); err != nil {
			return fmt.Errorf("remote-proxy: %w", err)
		}
		// Deliberately left in place:
		//
		//   - lever's overlay agent template and the project's default_template: they
		//     keep scion's placeholder system prompt out of every agent, remote access
		//     or not.
		//   - The staged hub SPA (layout.WebAssetsDir). Nothing lever starts reads it
		//     with remote access off (no --web-assets-dir), stagedWebDigest keeps it
		//     from going stale, and the few MB of it are not worth a delete path whose
		//     failure mode is 404ing a UI the hub still serves. scion's workstation
		//     defaults keep the web frontend on either way (scion.ServerOpts.EnableWeb).
	}
	return nil
}

func planHas(steps []Step, kind StepKind) bool {
	for _, s := range steps {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// stepLabel adds an image step's reference, since the plan can load several.
func stepLabel(s Step) string {
	if s.Kind == KindLoadImage && s.Target != "" {
		return string(s.Kind) + " " + s.Target
	}
	return string(s.Kind)
}

// The default arm is a hard error so a Plan emitting an unknown kind fails
// loudly.
func (r *run) step(ctx context.Context, s Step) error {
	switch s.Kind {
	case KindBrokerUp:
		return runBrokerUp(ctx, r.d)
	case KindLoadImage:
		return runLoadImage(ctx, s, r.d, r.app)
	case KindInitMachine:
		return r.d.Scion.InitMachine(ctx)
	case KindConfigRegistry:
		return r.d.Scion.ConfigSetGlobal(ctx, "image_registry", ImageRegistry)
	case KindScionServer:
		return r.scionServer(ctx)
	case KindHubIdentity:
		return r.d.EnsureHubIdentity(ctx)
	case KindCredential:
		return runCredential(ctx, s, r.d)
	case KindRegisterProject:
		return r.registerProject(ctx, s)
	case KindAgentTemplate:
		return r.agentTemplate(ctx, s)
	case KindMintManagerBootstrap:
		return r.mintManagerBootstrap(ctx, s)
	case KindStartManager:
		return r.startManager(ctx, s)
	case KindRemoteProxy:
		// Plan includes this step only when remote access is on.
		return r.d.StartRemoteProxy(ctx)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// scionServer runs the scion-server step. It restarts a running hub only when
// it was started with other auth settings, another key, or another argv than
// this run wants (HubRestartReason), or when the guest's key file or
// settings changed in a way the hub reads only at startup: a restart drops
// every agent's hub connection.
func (r *run) scionServer(ctx context.Context) error {
	telemetryChanged, err := r.d.EnsureScionTelemetry(ctx)
	if err != nil {
		return fmt.Errorf("scion telemetry: %w", err)
	}
	if telemetryChanged {
		r.d.Log("lever: agent telemetry in the jail's scion settings is now %q (scion.telemetry) — agents started from here on use it; "+
			"a hub or manager that was already running keeps the old setting until `lever stop && lever up`", r.app.EffectiveScionTelemetry())
	}
	guestChanged, err := r.d.EnsureHubAuth(ctx)
	if err != nil {
		return fmt.Errorf("hub auth: %w", err)
	}
	opts := r.hubServerOpts()
	live, err := r.d.Scion.RunningHub(ctx)
	if err != nil {
		return fmt.Errorf("hub auth: %w", err)
	}
	if live.Running && r.d.HubConduitEnv != nil {
		env, err := r.d.HubConduitEnv(ctx)
		if err != nil {
			return fmt.Errorf("hub conduit policy: %w", err)
		}
		live.Env = maps.Clone(live.Env)
		if live.Env == nil {
			live.Env = map[string]string{}
		}
		maps.Copy(live.Env, env)
	}
	reason := HubRestartReason(live, opts)
	if reason == "" && guestChanged && live.Running {
		reason = "the hub's key file or settings changed"
	}
	if reason != "" {
		// The database settings the hub reads at start go in first, so this one
		// restart takes them too and hub-identity has none left to make.
		if r.d.BeforeHubRestart != nil {
			if err := r.d.BeforeHubRestart(ctx); err != nil {
				r.d.Log("lever: %v; hub-identity pins them after the restart", err)
			}
		}
		if err := RestartHub(ctx, r.app, r.d, reason); err != nil {
			return err
		}
		r.brokerOnline = true
		return nil
	}
	// On a running hub the start only waits until it answers.
	if err := r.d.Scion.ServerStart(ctx, opts); err != nil {
		return err
	}
	if !live.Running {
		if err := hubBrokerOnline(ctx, r.d, hubColdBroker); err != nil {
			return fmt.Errorf("scion-server: %w", err)
		}
		r.brokerOnline = true
		return nil
	}
	// A hub that kept running: its runtime broker is only looked at, so a
	// broker that went away is the manager's start retry's to wait out.
	online, err := r.d.WaitBrokerReady(ctx, scionGlobalProject)
	if err != nil {
		return fmt.Errorf("scion-server: waiting for runtime broker: %w", err)
	}
	r.brokerOnline = online
	return nil
}

// RestartHub is the one way lever restarts the hub: stop it (stop-then-start,
// not scion server restart, which replays the old argv without this run's
// env and refuses when no daemon runs), converge the guest settings it reads
// at start again (agents can rewrite them while it runs), start it, and hold
// it to the start's postcondition (hubBrokerOnline). reason, when set, is
// logged.
func RestartHub(ctx context.Context, app *config.App, d Deps, reason string) error {
	if reason != "" {
		d.Log("lever: restarting the hub: %s", reason)
	}
	if err := d.Scion.ServerStop(ctx); err != nil {
		return fmt.Errorf("restart the hub: %w", err)
	}
	if _, err := d.EnsureHubAuth(ctx); err != nil {
		return fmt.Errorf("the hub is stopped: it was stopped for a restart, and converging its settings before the start failed (%w); fix the cause and run `lever up`, which starts it again", err)
	}
	opts := HubServerOpts(app, d.HubSessionSecret, d.HubAuthEnv)
	opts.Progress = d.Log
	if err := d.Scion.ServerStart(ctx, opts); err != nil {
		return fmt.Errorf("the hub is stopped: it was stopped for a restart, and starting it again failed (%w); fix the cause and run `lever up`, which starts it again", err)
	}
	return hubBrokerOnline(ctx, d, hubRestartedBroker)
}

// hubRestartedBroker and hubColdBroker bound the wait, after lever restarted
// or started the hub, for its runtime broker. A cold start also covers a
// freshly booted VM, where the broker's first connect is slowest.
var (
	hubRestartedBroker = wait.Spec{Name: "the hub's runtime broker to come online after the restart",
		Budget: 2 * time.Minute, MinAttempts: 3, Interval: time.Second, MaxInterval: 5 * time.Second}
	hubColdBroker = wait.Spec{Name: "the hub's runtime broker to come online after the start",
		Budget: 5 * time.Minute, MinAttempts: 3, Interval: time.Second, MaxInterval: 5 * time.Second}
)

// hubBrokerOnline is the postcondition of a hub lever started: the hub
// answers (ServerStart waited for that) and its runtime broker is online. A
// hub that serves before its broker registers fails every agent create and
// resume, so this is a hard requirement. A probe error is evidence, not the
// end of the wait: the hub may still be settling.
func hubBrokerOnline(ctx context.Context, d Deps, s wait.Spec) error {
	err := wait.For(ctx, s, func(ctx context.Context) wait.Result {
		online, seen, err := d.BrokerOnline(ctx)
		switch {
		case err != nil:
			return wait.NotYetErr(err)
		case online:
			return wait.Ready()
		}
		return wait.NotYet(seen)
	})
	if errors.Is(err, wait.ErrTimeout) {
		return fmt.Errorf("the hub is running, but its runtime broker did not come online: %w; "+
			"check the hub's log in the jail (~/.scion/server.log), then run `lever up` again", err)
	}
	return err
}

// HubRestartReason says why a running hub does not match the start opts, or ""
// when it does (or none runs). It compares the env lever pins, dev auth (a hub
// an operator started by hand gets scion's dev-auth default) and the web
// assets argv that toggling remote access changes. scion re-execs its daemon
// with a normalised argv, so the argv is compared flag by flag, not as a whole.
func HubRestartReason(live scion.RunningHub, want scion.ServerOpts) string {
	if !live.Running {
		return ""
	}
	for k, v := range want.Env {
		if got, ok := live.Env[k]; !ok || got != v {
			return fmt.Sprintf("it was started without lever's %s setting", k)
		}
	}
	for k := range live.Env {
		if _, ok := want.Env[k]; !ok {
			return fmt.Sprintf("it was started with %s, which lever does not set", k)
		}
	}
	if flagValue(live.Argv, "--dev-auth") != "false" {
		return "it was started without --dev-auth=false"
	}
	if got := flagValue(live.Argv, "--web-assets-dir"); got != want.WebAssetsDir {
		return fmt.Sprintf("its web assets argv is %q, lever wants %q", got, want.WebAssetsDir)
	}
	return ""
}

// flagValue returns the value of --name=value or --name value in argv; a bare
// boolean --name reads as "true".
func flagValue(argv []string, name string) string {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name {
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1]
			}
			return "true"
		}
	}
	return ""
}

// scionGlobalProject is the value of scion's -g flag that names its global
// settings (~/.scion, made by init-machine) instead of a project directory.
const scionGlobalProject = "global"

// hubServerOpts makes the run's log the start's progress sink, so a slow cold
// start says it is waiting.
func (r *run) hubServerOpts() scion.ServerOpts {
	opts := HubServerOpts(r.app, r.d.HubSessionSecret, r.d.HubAuthEnv)
	opts.Progress = r.d.Log
	return opts
}

// HubServerOpts is the one description of how lever starts the hub for a
// config. Every start and restart shares it: a restart exists to replace an
// argv that no longer matches the config, and a second copy could re-apply the
// flags it should drop.
func HubServerOpts(app *config.App, sessionSecret string, authEnv map[string]string) scion.ServerOpts {
	opts := scion.ServerOpts{
		WebPort:       scion.DefaultHubPort,
		DevAuth:       false,
		EnableWeb:     app.RemoteEnabled(),
		SessionSecret: sessionSecret,
		Env:           authEnv,
	}
	if app.ScionWebAssets() {
		// Same predicate as the asset staging, so the flag never points at an empty
		// directory (see ServerOpts.WebAssetsDir).
		opts.WebAssetsDir = layout.WebAssetsDir
	}
	return opts
}

func runBrokerUp(ctx context.Context, d Deps) error {
	if err := d.StartBroker(ctx); err != nil {
		return err
	}
	return d.BrokerHealthy(ctx)
}

// agentTemplate logs only on change: the change is otherwise invisible, and a
// no-op re-apply stays quiet.
func (r *run) agentTemplate(ctx context.Context, s Step) error {
	// Jail-side path: the scion client runs inside the jail, where the host tree is
	// visible only at the mount.
	changed, err := r.d.EnsureAgentTemplate(ctx, JailPath(s.Target, r.app.Tree, r.d.JailMount))
	if err != nil {
		return err
	}
	if changed {
		r.d.Log("lever: agents will no longer be launched with scion's placeholder system prompt (new agents only; existing ones keep the prompt they were provisioned with)")
	}
	return nil
}

func runLoadImage(ctx context.Context, s Step, d Deps, app *config.App) error {
	loaded, load := d.ImageLoaded, d.LoadImage
	if s.TarPath != "" {
		loaded = func(ctx context.Context, ref string) bool { return d.ImageLoadedTar(ctx, ref, s.TarPath) }
		allow := func(tag string) error {
			if d.ImageTagPolicy != nil {
				if err := d.ImageTagPolicy(tag); err != nil {
					return err
				}
			}
			if app != nil {
				return app.CheckTarTag(s.Target, tag)
			}
			return nil
		}
		load = func(ctx context.Context, ref string) error {
			return d.LoadImageTar(ctx, ref, s.TarPath, allow)
		}
	}
	rep := progress.From(ctx)
	key, inputs := warmFingerprint(s)
	if loaded(ctx, s.Target) {
		// The jail holds the configured image (by image ID), so the layer copy
		// the last finished warm-up of these inputs built is still the one an
		// agent start uses.
		if d.Fingerprints != nil && d.Fingerprints.Matches(key, inputs) {
			rep.Note("already loaded and warmed")
			return ctx.Err()
		}
		rep.Note("already loaded")
	} else {
		if d.Fingerprints != nil {
			if err := d.Fingerprints.Forget(key); err != nil {
				d.Log("load-image: warning: %v", err)
			}
		}
		rep.Detail("loading the image into the jail")
		if err := load(ctx, s.Target); err != nil {
			return err
		}
		rep.Note("loaded")
		// Prune the copy a rebuilt image leaves untagged, or the grow-only jail disk
		// ratchets up. Best-effort.
		if err := d.PruneImages(ctx); err != nil {
			d.Log("load-image: pruning superseded jail images failed: %v", err)
		}
	}
	// Warm an already-loaded image too: an earlier apply may have failed before its
	// warm-up finished.
	warmed := warmImage(ctx, s.Target, d)
	// A warm-up cut short by the run stopping or an interrupt is not a finished
	// step.
	if err := ctx.Err(); err != nil {
		return err
	}
	if warmed && d.Fingerprints != nil {
		if err := d.Fingerprints.Record(key, inputs); err != nil {
			d.Log("load-image: warning: could not record the warm-up of %s (the next bring-up warms it again): %v", s.Target, err)
		}
	}
	return nil
}

// warmFingerprint is the lifecycle key and the inputs of an image's warm-up:
// the reference, the archive it ships in as the host has it now, and this
// lever (whose warm-up run may differ). "" inputs never match.
func warmFingerprint(s Step) (key, inputs string) {
	in := struct {
		Ref, Tar, Version string
		Size              int64
		ModTime           time.Time
	}{Ref: s.Target, Tar: s.TarPath, Version: cli.VersionString()}
	if s.TarPath != "" {
		fi, err := os.Stat(s.TarPath)
		if err != nil {
			return "warm-image " + s.Target, ""
		}
		in.Size, in.ModTime = fi.Size(), fi.ModTime().UTC()
	}
	return "warm-image " + s.Target, state.HashJSON(in)
}

// imageWarmTimeout bounds one image's warm-up. The measured first run of a
// 5 GB image took 44 s; the bound leaves room for a slow disk.
const imageWarmTimeout = 15 * time.Minute

// imageWarmNoticeAfter is how long a warm-up runs before the step says what it
// is waiting for. A warm image takes well under a second, so a re-apply prints
// nothing.
var imageWarmNoticeAfter = 3 * time.Second

// warmImage only warns on failure: the agent start may still succeed (a small
// image, a podman with native id-mapped mounts), and the start-manager error
// names the cause if it does not.
func warmImage(ctx context.Context, ref string, d Deps) (warmed bool) {
	wctx, cancel := context.WithTimeout(ctx, imageWarmTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.WarmImage(wctx, ref) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(imageWarmNoticeAfter):
		if rep := progress.From(ctx); rep.Active() {
			rep.Detail("warming the user-namespace layer copy (the first run of a large image can take a minute)")
		} else {
			d.Log("load-image: warming the user-namespace layer copy of %s (the first run of a large image can take a minute)", ref)
		}
		err = <-done
	}
	if err != nil && ctx.Err() == nil {
		d.Log("load-image: warning: warming %s failed: %v. The first agent start on it may stay in phase created; see the start-manager error if it does", ref, err)
	}
	return err == nil
}

func runCredential(ctx context.Context, s Step, d Deps) error {
	read := d.ReadCred
	if read == nil {
		read = defaultReadCred
	}
	tok, err := read(s.Target)
	if err != nil {
		return fmt.Errorf("reading credential %s: %w", s.Target, err)
	}
	return d.Scion.SecretSet(ctx, "CLAUDE_CODE_OAUTH_TOKEN", tok)
}

// ImageRegistry is the registry prefix scion rewrites a bare agent image name
// to. Agents lever starts run a qualified name scion never rewrites
// (imageref.Agent); a record created by an older lever keeps its bare name,
// which this value resolves to the image load-image tagged for it.
const ImageRegistry = "scionlocal"

// AgentRuntime is the container runtime the jail's scion starts agents with:
// load-image fills rootless podman's store, and the warm-up builds podman's
// layer copy.
const AgentRuntime = "podman"

func (r *run) registerProject(ctx context.Context, s Step) error {
	jp := JailPath(s.Target, r.app.Tree, r.d.JailMount)

	// Observe before anything destructive: a suspended agent record survives lever
	// stop and lever up, and its project linkage lives in this registration, so
	// tearing it down orphans the record and breaks scion resume. A query error or
	// an unsound registration (zero, duplicate or torn) falls through to the
	// destructive path: fail open, never a hard failure over an observe read.
	if ok, err := r.d.ScionProjectRegistered(ctx, jp); err == nil && ok {
		// The recorded hub endpoint can be another port's (a link made against a
		// throwaway hub), and this path skips the re-init that would rewrite it.
		if err := r.d.RepairScionHubEndpoint(ctx, jp); err != nil {
			return err
		}
		linked, err := r.d.ProjectLinked(ctx, jp)
		if err != nil {
			return fmt.Errorf("register-project: %w", err)
		}
		if !linked {
			// hub-identity registered the project with this ID and name, so the link
			// takes scion's already-linked path, which a project PAT may run. Its
			// agent sync needs a runtime broker on the project.
			if err := r.d.EnsureProjectProvider(ctx, jp); err != nil {
				return err
			}
			if err := r.hubLink(ctx, jp); err != nil {
				return err
			}
			if linked, err = r.d.ProjectLinked(ctx, jp); err != nil {
				return fmt.Errorf("register-project: %w", err)
			} else if !linked {
				return fmt.Errorf("register-project: scion hub link ran but the project is still not linked to the hub project lever recorded")
			}
		}
		return r.settleProject(ctx, jp)
	}

	// Remove a stale .scion marker file left in the tree by a previous bring-up:
	// scion init writes workspace_path only on fresh create, so with a stale marker
	// the agent mounts an empty managed config-dir copy instead of the live tree.
	//
	// The removal must go through the jail's own filesystem view. Over the VirtioFS
	// bind mount a host-side unlink is not promptly visible to the guest, and the
	// in-jail scion init then fails with "existing project marker is invalid".
	if err := r.d.RemoveJailFile(ctx, path.Join(jp, layout.ProjectMarker)); err != nil {
		return err
	}
	// Clear stale registrations first so scion init leaves exactly one per
	// workspace.
	if err := r.d.RemoveScionProjectConfigs(ctx, jp); err != nil {
		return err
	}
	if err := r.d.Scion.InitProject(ctx, jp); err != nil {
		return err
	}
	if err := r.pinHubProject(ctx, jp); err != nil {
		return err
	}
	if err := r.d.EnsureProjectProvider(ctx, jp); err != nil {
		return err
	}
	if err := r.hubLink(ctx, jp); err != nil {
		return err
	}
	return r.settleProject(ctx, jp)
}

// pinHubProject points the fresh registration at the hub's existing record of
// this project before the link. The re-init generated a new local ID, and a
// hub link under the controller PAT refuses an ID the hub does not know (a
// user token cannot register a project) instead of matching by name. With no
// record on the hub yet, the link decides as before.
func (r *run) pinHubProject(ctx context.Context, jp string) error {
	name := path.Base(jp)
	id, err := r.d.Scion.HubProjectID(ctx, name)
	if err != nil {
		return fmt.Errorf("looking up the hub's record of project %q: %w", name, err)
	}
	if id == "" {
		return nil
	}
	if err := r.d.Scion.PinHubProject(ctx, jp, id); err != nil {
		return fmt.Errorf("pointing %s at hub project %s: %w", jp, id, err)
	}
	return nil
}

// hubLinkRetryDelays are the waits before each retry of a hub link that failed
// with no_runtime_broker. Short first, so a broker that registers moments late
// costs a second; doubling reaches 31 s for a slow cold start.
var hubLinkRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// hubLink retries a 422 no_runtime_broker: the link's agent sync creates agents
// on the hub, which fails until the runtime broker registers, and the
// scion-server step's fail-soft wait gives up at 30 s while a cold Lima start
// can take longer. Only that error is retried; re-linking a linked project is
// scion's "already linked" path.
func (r *run) hubLink(ctx context.Context, jp string) error {
	err := r.d.Scion.HubLink(ctx, jp)
	for _, d := range hubLinkRetryDelays {
		if err == nil || !strings.Contains(err.Error(), "no_runtime_broker") {
			return err
		}
		r.d.Log("register-project: the hub has no runtime broker yet (no_runtime_broker); retrying the hub link in %s", d)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		err = r.d.Scion.HubLink(ctx, jp)
	}
	return err
}

// settleProject is the hub-side project state both register paths converge on:
// a runtime broker to start agents on, no cross-agent shared dir, and a role
// ceiling. All three must hold whether the registration was just made or found
// sound.
func (r *run) settleProject(ctx context.Context, jp string) error {
	projectName := path.Base(jp)
	if err := r.d.EnsureProjectProvider(ctx, jp); err != nil {
		return err
	}
	if err := r.d.StripProjectSharedDirs(ctx, projectName); err != nil {
		return err
	}
	return r.d.EnsureAgentRoleCeiling(ctx, projectName)
}

// mintManagerBootstrap is idempotent against the live broker latch, not a
// stale file: a spent latch is tolerated only when a ticket is already staged.
func (r *run) mintManagerBootstrap(ctx context.Context, s Step) error {
	// After a broker restart the latch reopens and a fresh ticket is deposited, so
	// a first apply that failed before the manager enrolled recovers on re-apply.
	m, err := r.d.MintManagerBootstrap(ctx)
	if err != nil {
		if errors.Is(err, ErrBootstrapLatched) {
			// A spent latch with no staged ticket means a stale broker from a prior run is
			// being reused, and the new manager could never enrol: fail loudly.
			staged := filepath.Join(s.Target, ".lever", "bootstrap.json")
			if _, statErr := os.Stat(staged); statErr == nil {
				return nil
			}
			return fmt.Errorf("broker /bootstrap latch already consumed but no bootstrap ticket is staged at %s; a stale broker is likely still running — run `lever down` then retry", staged)
		}
		return err
	}
	r.minted = true
	return StageBootstrapMaterial(s.Target, m)
}

// startManager is the start-manager step: the manager's one lifecycle
// (EnsureManager) with what this run knows: the ticket the mint step staged
// and whether the scion-server step saw the runtime broker online.
func (r *run) startManager(ctx context.Context, _ Step) error {
	intent := ManagerKeep
	if r.fresh {
		intent = ManagerFresh
	}
	_, err := EnsureManager(ctx, r.app, r.d, ManagerRequest{
		Intent: intent, Subject: "start-manager",
		Attach: r.attach, FreshWithoutConversation: r.freshNoConv,
		TicketStaged: r.minted, BrokerOnline: r.brokerOnline,
	})
	return err
}

// startFreshWithoutConversation replaces a resume that died because claude
// --continue found no conversation (cause wraps ErrNoConversation) with the
// delete and create of lever up --fresh, then the liveness gate again
// (onHarness starts the post-live probes). That log line is the agent's word,
// so it only starts the look: the fresh start happens only when
// Deps.ManagerConversationEmpty proves from the guest that the agent home holds
// no transcript, and the --fresh backup (Deps.BackupManagerConversation) still
// runs first. Whatever is not proven, or fails before the delete, returns cause
// with the record kept.
//
// The new session is recorded only when the skill on disk, read before the
// delete and again right before the create, is the one the manager's last
// recorded session started with (Deps.BeginUnattendedSession): the agent can
// bring this start about and can write the skill, so unlike --fresh it never
// re-baselines the session on a changed skill, and contacts stay refused until
// lever up --fresh. The failed resume may have spent this run's enrolment
// ticket, so the create re-arms the broker (ensureFreshBootstrap).
func (r *run) startFreshWithoutConversation(ctx context.Context, jp string, opts scion.StartOpts, cause error, onHarness func()) error {
	if r.d.ManagerConversationEmpty == nil {
		return cause
	}
	if perr := r.d.ManagerConversationEmpty(ctx); perr != nil {
		r.d.Log("start-manager: manager %q has no conversation to continue, but lever did not start it fresh on its own: it could not prove that the agent home holds no transcript (%s)", r.app.Name, termsafe.Sanitize(perr.Error()))
		return cause
	}
	agents, err := r.listAgentsRetry(ctx, jp)
	if err != nil {
		r.d.Log("start-manager: manager %q has no conversation to continue, but lever did not start it fresh on its own: it could not read the record (%s)", r.app.Name, termsafe.Sanitize(scion.ErrSummary(err)))
		return cause
	}
	rec := scion.FindAgent(agents, r.app.Name)
	if rec == nil {
		r.d.Log("start-manager: manager %q has no conversation to continue, and its record is gone; lever did not start it fresh on its own", r.app.Name)
		return cause
	}
	if err := r.backupBeforeDelete(ctx, jp, rec); err != nil {
		return fmt.Errorf("%w; lever went to start it fresh, but backing up its conversation failed, so the manager was NOT deleted: %w", cause, err)
	}
	// Record before anything is deleted, and after the backup (which carries the
	// last session's skill): a start the agent can bring about must not record a
	// skill it wrote.
	var sess UnattendedSession
	switch {
	case r.d.BeginUnattendedSession != nil:
		u, serr := r.d.BeginUnattendedSession(r.app.Name)
		if serr != nil {
			r.d.Log("start-manager: manager %q has no conversation to continue, but lever did not start it fresh on its own: it could not record that the new session's skill is unchecked (%s)", r.app.Name, termsafe.Sanitize(serr.Error()))
			return cause
		}
		sess = u
	case r.d.BeginSession != nil:
		r.d.Log("start-manager: manager %q has no conversation to continue, but lever did not start it fresh on its own: it cannot check the skill the new session would be recorded with", r.app.Name)
		return cause
	}
	if sess.Refused != nil {
		r.d.Log("start-manager: the fresh start lever is about to make on its own is not recorded as a new session (%s); contacts cannot post to manager %q until it starts fresh at your word (`lever up --fresh`)", termsafe.Sanitize(sess.Refused.Error()), r.app.Name)
	}
	// The resume's boot may have redeemed this run's ticket: mint anew.
	r.minted = false
	r.unattended, r.unattendedBegin, r.unattendedRefused = true, sess.Begin, sess.Refused != nil
	defer func() { r.unattended, r.unattendedBegin, r.unattendedRefused = false, nil, false }()
	if err := r.recoverDeleteAndCreate(ctx, jp, opts,
		fmt.Sprintf("start-manager: manager %q has no conversation to continue: claude --continue found none, and its agent home holds no transcript, so it was never used. Starting it FRESH, as `lever up --fresh` would (deleting record in phase %s; nothing is lost)", r.app.Name, scion.BoundedQuote(rec.Phase)),
		"a fresh start of a manager with no conversation"); err != nil {
		return err
	}
	return r.waitManagerLive(ctx, jp, true, onHarness)
}

// warnManagerClaude: boot applies the manager's claude block only at start, so
// a kept manager holds the values of its last start, removed ones included.
// Silent when they match, when nothing is configured or delivered, and when the
// read fails (the doctor row "claude settings" reports that).
func (r *run) warnManagerClaude(ctx context.Context, jp string) {
	if r.d.ReadClaudeSettings == nil {
		return
	}
	want := r.app.ManagerClaude()
	got, err := r.d.ReadClaudeSettings(ctx, jail.ContainerName(path.Base(jp), r.app.Name))
	if err != nil || got.Matches(want) {
		return
	}
	r.d.Log("start-manager: warning: manager %q runs with claude settings %s, but the config sets %s; the config's values take effect at its next start (`lever stop && lever up`). If a start does not change them, the image's lever-agent predates them: rebuild it (`make lever-image`), then recreate the manager with `lever up --fresh --keep-conversation` (it backs up the conversation and resumes the new manager on it) — `lever stop && lever up` resumes it on its old image",
		r.app.Name, got.Describe(), jail.DescribeClaude(want))
}

// ManagerTask reads the manager's boot prompt (manager.prompt_file) as a
// fresh session's task; "" when none is set.
func ManagerTask(app *config.App) (string, error) {
	p := app.ManagerPromptPath()
	if p == "" {
		return "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("reading manager prompt %s: %w", p, err)
	}
	task := strings.TrimSpace(string(b))
	// The task rides scion's argv into a tmux command capped at 16 KiB. Fail here,
	// naming the file, rather than after the broker-ready wait with a container
	// that exits "command too long".
	if err := scion.CheckTask(task); err != nil {
		return "", fmt.Errorf("manager prompt %s: %w", p, err)
	}
	return task, nil
}

// managerInstructions is read up front like the prompt and passed as content:
// the scion binary runs in the jail and never sees the host path.
func (r *run) managerInstructions() (string, error) {
	p := r.app.ManagerInstructionsPath()
	if p == "" {
		return "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("reading manager instructions %s: %w", p, err)
	}
	// Same named errors Start would raise, but naming the file, and before
	// the broker-ready wait.
	if err := scion.CheckInstructions(string(b)); err != nil {
		return "", fmt.Errorf("manager instructions %s: %w", p, err)
	}
	return string(b), nil
}

// managerStartOpts builds the manager's scion start options; in api-key mode
// it first sets the project env and the placeholder secret.
//
// LEVER_BOOTSTRAP is not set: lever-agent boot defaults to
// ./.lever/bootstrap.json relative to its CWD /workspace, which is jp, where
// mint-manager-bootstrap staged the ticket.
func (r *run) managerStartOpts(ctx context.Context, jp, task, instructions string) (scion.StartOpts, error) {
	apiKey := r.app.EffectiveManagerLLMAuth() == config.LLMAuthAPIKey
	if apiKey {
		if err := r.prepareAPIKeyMode(ctx, jp); err != nil {
			return scion.StartOpts{}, err
		}
	}
	return scion.StartOpts{
		Worker: r.app.Name, Task: task, Project: jp, Image: r.app.ManagerAgentImage(), Harness: "claude",
		// Empty omits --model. Read only on a fresh create: scion resume has no model
		// flag.
		Model: r.app.ManagerModel(),
		// Delivered on scion's stdin as inline config, never argv (see
		// scion.StartOpts.Instructions). Create-time only: a resume re-projects what
		// scion staged at the fresh create.
		Instructions: instructions,
		// manager.read_only: the protected tree paths and their ancestor pins, plus
		// /dev/kvm under nested_virt. Create-time only: a resume redispatches the
		// volumes scion stored with the record.
		Volumes: managerVolumes(jp, r.app),
		// The in-jail project tree, so the manager edits the host files in place.
		// Without it scion mounts a managed copy of the externalized config dir.
		Workspace: jp,
		// api-key: --harness-auth api-key, satisfied by the placeholder secret.
		APIKey: apiKey,
		// manager.resources, as inline config. Create-time only.
		Resources: ScionResources(r.app.Manager.Resources),
	}, nil
}

// ScionResources is an agent's configured limits in the shape scion.Start
// sends; the zero value for none. The broker's worker specs use it too.
func ScionResources(r config.Resources) scion.Resources {
	mem, cpu := r.ScionLimits()
	return scion.Resources{Memory: mem, CPU: cpu}
}

// KVMDevice is the device nested_virt gives the manager container.
const KVMDevice = "/dev/kvm"

// managerVolumes adds, under nested_virt, KVMDevice bind-mounted to itself:
// scion has no per-agent device field, and rootless podman passes a --device as
// this same bind mount (it cannot mknod and sets no device cgroup), so the
// manager gets a working /dev/kvm while the hub and the workers get none.
func managerVolumes(jp string, app *config.App) []scion.VolumeMount {
	out := managerTreeVolumes(jp, app.ManagerTreeMounts())
	out = append(out, SharedVolumes(jp, app.SharedMountsFor(app.Name))...)
	if app.NestedVirt {
		out = append(out, scion.VolumeMount{Source: KVMDevice, Target: KVMDevice})
	}
	return out
}

// SharedVolumes turns an agent's shared_folders plan into scion volumes,
// read-only unless the agent writes the folder. The broker builds a worker's
// from it too, so the two plans cannot drift.
func SharedVolumes(jp string, mounts []config.SharedMount) []scion.VolumeMount {
	out := make([]scion.VolumeMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, scion.VolumeMount{Source: path.Join(jp, m.Rel), Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return out
}

// managerTreeVolumes mounts each directory of the read_only plan over its own
// place in the workspace: read-only for an entry, read-write for an ancestor
// pin.
func managerTreeVolumes(jp string, mounts []config.TreeMount) []scion.VolumeMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]scion.VolumeMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, scion.VolumeMount{
			Source:   path.Join(jp, m.Rel),
			Target:   path.Join(scion.ContainerWorkspace, m.Rel),
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

// TreeMountGaps is how a manager container falls short of its
// manager.read_only plan, each list by tree-relative path:
//
//   - Missing: an entry with no mount; the agent writes it.
//   - Writable: an entry mounted read-write; the agent writes it.
//   - WrongSource: a mount whose source is not that directory of the in-jail
//     tree; it covers something else.
//   - Replaced: the host replaced the directory after the manager was
//     created; the mount covers the old one while a write may land in the
//     new one (live check).
//   - Elsewhere: the kernel records the mount as coming from another place
//     than the tree directory at its path: a symlink stood in the path at
//     start, or the directory was renamed since (live check).
//   - MissingPins: an entry's ancestor with no mount; the agent can rename it
//     away and recreate the protected path writable.
//   - MissingWorkerPins: a worker dir or its ancestor with no mount; the
//     manager can replace it with a link and have that worker mount a
//     protected directory read-write.
type TreeMountGaps struct {
	Missing, Writable, WrongSource, Replaced, Elsewhere, MissingPins, MissingWorkerPins []string
}

// Empty reports whether the container matches the plan.
func (g TreeMountGaps) Empty() bool {
	return len(g.Missing)+len(g.Writable)+len(g.WrongSource)+len(g.Replaced)+len(g.Elsewhere)+len(g.MissingPins)+len(g.MissingWorkerPins) == 0
}

// String words the gaps for a doctor row or an apply warning.
func (g TreeMountGaps) String() string {
	var parts []string
	add := func(paths []string, what string) {
		if len(paths) > 0 {
			parts = append(parts, strings.Join(paths, ", ")+" "+what)
		}
	}
	add(g.Missing, "not mounted read-only: the agent can write it")
	add(g.Writable, "mounted read-write: the agent can write it")
	add(g.WrongSource, "mounted from another source: the mount covers something else")
	add(g.Replaced, "covers another directory than the host has there now: it was replaced on the host after the manager was created, and the mount still covers the old one")
	add(g.Elsewhere, "mounted from another place than the tree's directory there: a symbolic link stood in its path when the container started (the runtime followed it), or it was moved since")
	add(g.MissingPins, "not pinned: the agent can rename it away and recreate a protected path writable")
	add(g.MissingWorkerPins, "(a worker dir) not pinned: the manager can replace it with a symbolic link and have that worker mount a protected directory read-write")
	return strings.Join(parts, "; ")
}

// Stale reports whether any gap is a replaced directory. The operator caused it
// by replacing a protected directory instead of editing it in place, so the fix
// text adds that rule.
func (g TreeMountGaps) Stale() bool { return len(g.Replaced)+len(g.Elsewhere) > 0 }

// ManagerTreeMountGaps compares a manager container's mounts with the read_only
// plan: every planned directory must be mounted at its place in the workspace
// from the same place in the in-jail tree jp, and an entry must be read-only. A
// pin's writability is not checked: being a mount point is what stops it being
// renamed or removed. The live check is LiveTreeMountGaps.
func ManagerTreeMountGaps(jp string, want []config.TreeMount, got []jail.Mount) TreeMountGaps {
	byTarget := make(map[string]jail.Mount, len(got))
	for _, m := range got {
		byTarget[m.Destination] = m
	}
	var g TreeMountGaps
	for _, w := range want {
		m, ok := byTarget[path.Join(scion.ContainerWorkspace, w.Rel)]
		switch {
		case !ok && w.ReadOnly:
			g.Missing = append(g.Missing, w.Rel)
		case !ok && w.WorkerPin:
			g.MissingWorkerPins = append(g.MissingWorkerPins, w.Rel)
		case !ok:
			g.MissingPins = append(g.MissingPins, w.Rel)
		case m.Source != path.Join(jp, w.Rel):
			g.WrongSource = append(g.WrongSource, w.Rel)
		case w.ReadOnly && m.RW:
			g.Writable = append(g.Writable, w.Rel)
		}
	}
	return g
}

// LiveMountChecker checks a running container's mounts from the guest.
type LiveMountChecker func(ctx context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error)

// ManagerLivePlan is the tree plan want as the live check reads it: each a tree
// directory over its own place (jail.LiveMount.SelfMount), read-only for an
// entry.
func ManagerLivePlan(jp string, want []config.TreeMount) []jail.LiveMount {
	plan := make([]jail.LiveMount, 0, len(want))
	for _, w := range want {
		plan = append(plan, jail.LiveMount{Target: path.Join(scion.ContainerWorkspace, w.Rel), Source: path.Join(jp, w.Rel), ReadOnly: w.ReadOnly, SelfMount: true})
	}
	return plan
}

// LiveTreeMountGaps runs the live half of the tree-plan check on a running
// container, from the guest side: every planned directory a mount point, an
// entry read-only, each covering the directory the host has there now. A mount
// podman lists does not prove that: after the host replaced the directory
// (rename-based deploy, rm -rf and recreate) the mount stays on the old one and
// a write lands in the new one, and an answer from inside the container would
// be the agent's word. Every failure to check is an error, so a check that did
// not run never reads as held.
func LiveTreeMountGaps(ctx context.Context, live LiveMountChecker, jp, ref string, want []config.TreeMount) (TreeMountGaps, error) {
	var g TreeMountGaps
	if len(want) == 0 {
		return g, nil
	}
	problems, err := live(ctx, ref, ManagerLivePlan(jp, want))
	if err != nil {
		return g, err
	}
	byTarget := make(map[string]config.TreeMount, len(want))
	for _, w := range want {
		byTarget[path.Join(scion.ContainerWorkspace, w.Rel)] = w
	}
	for _, p := range problems {
		w, ok := byTarget[p.Target]
		if !ok {
			return TreeMountGaps{}, fmt.Errorf("live mount check named %q, which is not in the plan", p.Target)
		}
		switch {
		case p.Reason == jail.LiveMountMissing && w.ReadOnly:
			g.Missing = append(g.Missing, w.Rel)
		case p.Reason == jail.LiveMountMissing && w.WorkerPin:
			g.MissingWorkerPins = append(g.MissingWorkerPins, w.Rel)
		case p.Reason == jail.LiveMountMissing:
			g.MissingPins = append(g.MissingPins, w.Rel)
		case p.Reason == jail.LiveMountWritable:
			g.Writable = append(g.Writable, w.Rel)
		case p.Reason == jail.LiveMountReplaced:
			g.Replaced = append(g.Replaced, w.Rel)
		case p.Reason == jail.LiveMountElsewhere:
			g.Elsewhere = append(g.Elsewhere, w.Rel)
		default:
			return TreeMountGaps{}, fmt.Errorf("live mount check gave an unknown reason for %q", w.Rel)
		}
	}
	return g, nil
}

// ManagerTreeMountsFix is the operator's way out of every gap: a fresh create,
// and for a replaced directory, editing protected directories in place from
// now on.
func ManagerTreeMountsFix(g TreeMountGaps) string {
	fix := "run `lever up --fresh --keep-conversation` to recreate the manager with the mounts and keep its conversation (plain `--fresh` starts a new one; both back the old one up first)"
	if g.Stale() {
		fix += "; from then on edit protected directories in place — replacing one on the host (rm -rf and recreate, a rename-based deploy, a git checkout that removes and re-adds it) drops the protection until the next fresh create"
	}
	return fix
}

// StaleTreeMounts is how a manager container's tree mounts outlive the config:
// scion keeps a record's volumes for life and a resume recreates the container
// from them. Each list is by tree-relative path:
//
//   - Gone: the host directory behind the mount no longer exists, or a symlink
//     now stands in its path (lever does not follow it; see treePathLinked).
//     podman cannot recreate the container, or would mount whatever the link
//     points at, so the resume is refused.
//   - DroppedReadOnly: an entry no longer in manager.read_only, still mounted
//     read-only until a fresh create.
//   - DroppedPins: a pin the plan no longer has, still mounted over itself, so
//     the directory cannot be renamed or removed from inside the container.
//
// An entry added to the config is a TreeMountGaps Missing gap instead.
type StaleTreeMounts struct {
	Gone, DroppedReadOnly, DroppedPins []string
}

// Empty reports whether the container mounts nothing the config dropped.
func (s StaleTreeMounts) Empty() bool {
	return len(s.Gone)+len(s.DroppedReadOnly)+len(s.DroppedPins) == 0
}

// The paths come from the container's inspect or the hub record, text lever
// does not control, so String bounds them.
const (
	maxStalePaths     = 10
	maxStalePathBytes = 120
)

// String quotes each path (%q escapes every control character) after a cut to
// maxStalePathBytes, and stops each list at maxStalePaths.
func (s StaleTreeMounts) String() string {
	var parts []string
	add := func(paths []string, what string) {
		if len(paths) == 0 {
			return
		}
		var safe []string
		for i, p := range paths {
			if i == maxStalePaths {
				safe = append(safe, fmt.Sprintf("and %d more", len(paths)-maxStalePaths))
				break
			}
			if len(p) > maxStalePathBytes {
				p = strings.ToValidUTF8(p[:maxStalePathBytes], "") + "…"
			}
			safe = append(safe, strconv.Quote(p))
		}
		parts = append(parts, strings.Join(safe, ", ")+" "+what)
	}
	add(s.Gone, "mounted by the manager record but no longer on the host (missing, or replaced by a symbolic link lever does not follow): podman cannot recreate the container from it, so the next resume fails or would mount what the link points at")
	add(s.DroppedReadOnly, "still mounted read-only although no longer in manager.read_only (the record keeps its mounts until a fresh create)")
	add(s.DroppedPins, "still pinned (mounted over itself) although the config no longer asks for it (the record keeps its mounts until a fresh create)")
	return strings.Join(parts, "; ")
}

// Fix is the operator's way out: recreate a gone directory or start fresh; a
// dropped mount goes only with a fresh create.
func (s StaleTreeMounts) Fix() string {
	const fresh = "run `lever up --fresh --keep-conversation` (it recreates the manager with the mounts the config names now and keeps its conversation; plain `--fresh` starts a new one; both back the old one up first)"
	if len(s.Gone) > 0 {
		return "recreate each missing directory on the host as a real directory (an empty one is enough; replace a symbolic link with one) and run the command again; or, to drop the mounts, " + fresh
	}
	return "keep these directories until the next fresh create (a missing one blocks the resume); to drop the mounts now, " + fresh
}

// ManagerStaleTreeMounts finds the tree directories lever mounted over
// themselves (treeSelfMount) that the current read_only plan no longer has, or
// whose host directory under tree is gone. Any other mount is not lever's and
// is skipped.
func ManagerStaleTreeMounts(jp, tree string, want []config.TreeMount, got []jail.Mount) StaleTreeMounts {
	planned := make(map[string]bool, len(want))
	for _, w := range want {
		planned[w.Rel] = true
	}
	var s StaleTreeMounts
	for _, m := range got {
		rel, ok := treeSelfMount(jp, m)
		if !ok {
			continue
		}
		linked, err := treePathLinked(tree, rel)
		switch {
		case linked, errors.Is(err, os.ErrNotExist):
			s.Gone = append(s.Gone, rel)
		case planned[rel]:
		case m.RW:
			s.DroppedPins = append(s.DroppedPins, rel)
		default:
			s.DroppedReadOnly = append(s.DroppedReadOnly, rel)
		}
	}
	slices.Sort(s.Gone)
	slices.Sort(s.DroppedReadOnly)
	slices.Sort(s.DroppedPins)
	return s
}

// treePathLinked reports whether any component of rel under tree is a
// symlink, using Lstat one component at a time. It never follows a link on the
// host: the tree is agent-writable, and a link could point at a path whose stat
// blocks (a macOS autofs path such as /net/<host>/x) or anywhere outside the
// tree, which a resume would then mount into the manager. The error is the
// first Lstat's (os.ErrNotExist for a missing component).
func treePathLinked(tree, rel string) (bool, error) {
	p := tree
	for _, part := range strings.Split(rel, "/") {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return false, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

// treeSelfMount reports whether m is a tree directory mounted over its own
// place in the workspace (the shape managerTreeVolumes creates), and its
// tree-relative path.
func treeSelfMount(jp string, m jail.Mount) (string, bool) {
	rel, ok := strings.CutPrefix(m.Destination, scion.ContainerWorkspace+"/")
	if !ok || rel == "" || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	return rel, m.Source == path.Join(jp, rel)
}

// checkStaleTreeMounts runs before apply keeps or resumes a manager record: a
// mount for a gone directory fails the resume inside podman with a bare statfs
// error, and a mount the config dropped stays until a fresh create.
//
// It reads the mounts off the manager's container, which a stopped record
// keeps, falling back to the record's volumes on the hub (Deps.RecordVolumes);
// with neither it says nothing. A gone directory refuses the bring-up before
// anything is acted on unless the record is kept as it is (verbNone): every
// other path, the forced resume of an error phase included, recreates the
// container. Everything else is a warning.
func (r *run) checkStaleTreeMounts(ctx context.Context, jp string, kept bool) error {
	got, ok := r.managerMounts(ctx, jp)
	if !ok {
		return nil
	}
	stale := ManagerStaleTreeMounts(jp, r.app.Tree, r.app.ManagerTreeMounts(), got)
	if stale.Empty() {
		return nil
	}
	if len(stale.Gone) > 0 && !kept {
		return fmt.Errorf("%s: manager %q cannot be resumed: %s. To fix it, %s", r.subjectName(), r.app.Name, stale, stale.Fix())
	}
	r.d.logf("%s: warning: manager %q: %s. %s", r.subjectName(), r.app.Name, stale, stale.Fix())
	return nil
}

// resumeCheckTimeout bounds one verifyResumedTreeMounts, suspend included.
const resumeCheckTimeout = 90 * time.Second

// verifyResumedTreeMounts closes what checkStaleTreeMounts cannot: that check
// reads the host tree before the resume, and the runtime resolves each mount
// source again, following symlinks, when it recreates the container. An agent
// that can write a mounted directory's parent (the manager itself during a
// forced resume, or a writer of a shared folder above a stale mount) can swap
// the directory for a link in between, and the runtime then mounts whatever
// the link names (read-write for a pin) into the manager. Nothing lever runs
// can make the runtime refuse a link, so right after the resume every
// directory lever mounted over its own place (treeSelfMount) is read back from
// the guest (CheckLiveMounts: the kernel's record, nothing run inside the
// container). A mount from elsewhere, missing at its place, or writable where
// it should be read-only, or a first process in a namespace made inside the
// container, suspends the manager and fails the bring-up: fail closed, since
// the agent may hold a path it must never see. verified is false, with no
// error, when the check could not run: the caller tries again once the manager
// is live, and warns if it still cannot. With no inspect or live check wired,
// verified is true.
func (r *run) verifyResumedTreeMounts(ctx context.Context, jp string) (verified bool, err error) {
	if r.d.InspectContainerMounts == nil || r.d.CheckLiveMounts == nil {
		// Not wired: warnManagerTreeMounts names the gap when a plan exists.
		return true, nil
	}
	// Detached from the caller's cancellation, bounded: an interrupt right after
	// the resume must not skip the check or the suspend.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeCheckTimeout)
	defer cancel()
	ref := jail.ContainerName(path.Base(jp), r.app.Name)
	got, err := r.d.InspectContainerMounts(ctx, ref)
	if err != nil {
		return false, nil
	}
	var plan []jail.LiveMount
	for _, m := range got {
		if rel, ok := treeSelfMount(jp, m); ok {
			plan = append(plan, jail.LiveMount{Target: path.Join(scion.ContainerWorkspace, rel), Source: path.Join(jp, rel), ReadOnly: !m.RW, SelfMount: true})
		}
	}
	if len(plan) == 0 {
		return true, nil
	}
	slices.SortFunc(plan, func(a, b jail.LiveMount) int { return strings.Compare(a.Target, b.Target) })
	problems, err := r.d.CheckLiveMounts(ctx, ref, plan)
	var why string
	switch {
	case errors.Is(err, jail.ErrForeignNamespace):
		why = fmt.Sprintf("%v, so lever cannot see the mounts the agent works under. Something in the container moved it there to show lever a staged view", err)
	case err != nil:
		return false, nil
	}
	var bad []string
	for _, p := range problems {
		// Elsewhere: the source was resolved through a link. Missing: the runtime also
		// resolves the destination inside the container, and /workspace is the same
		// tree, so a swapped link moves the mount point too. Writable: a read-only
		// mount is not. Replaced alone is left to warnManagerTreeMounts: it rests on
		// stable inode numbers.
		switch p.Reason {
		case jail.LiveMountElsewhere, jail.LiveMountMissing, jail.LiveMountWritable:
			bad = append(bad, strconv.Quote(strings.TrimPrefix(p.Target, scion.ContainerWorkspace+"/"))+" "+p.Reason)
		}
	}
	if len(bad) > 0 {
		why = "a tree mount is not what lever planned: " + strings.Join(bad, "; ") + ". A directory in its path was swapped (for a symbolic link, most likely) between lever's check and the container start, so the runtime may have mounted something else into the manager, at another place too"
	}
	if why == "" {
		return true, nil
	}
	suspended := "lever suspended it"
	if serr := r.d.managerScion().Suspend(ctx, r.app.Name, jp); serr != nil {
		suspended = "lever could NOT suspend it (" + scion.ErrSummary(serr) + "): stop it yourself (`lever stop`)"
	}
	return true, fmt.Errorf("%s: manager %q after its resume: %s; %s. Find and undo what changed on the host (put the real directory back), then run `lever up` again, or recreate the manager with `lever up --fresh` (it backs the conversation up first); treat what the manager could reach since as exposed, and if that includes the manager itself, do not add --keep-conversation, which carries the session you no longer trust into the new manager",
		r.subjectName(), r.app.Name, why, suspended)
}

func (r *run) warnUnverifiedResume() {
	r.d.logf("%s: warning: lever could not check from the guest where manager %q's tree mounts come from after the resume; run `lever doctor` (row \"manager read-only paths\")", r.subjectName(), r.app.Name)
}

func (r *run) managerMounts(ctx context.Context, jp string) ([]jail.Mount, bool) {
	if r.d.InspectContainerMounts != nil {
		if got, err := r.d.InspectContainerMounts(ctx, jail.ContainerName(path.Base(jp), r.app.Name)); err == nil {
			return got, true
		}
	}
	if r.d.RecordVolumes != nil {
		if got, err := r.d.RecordVolumes(ctx, path.Base(jp), r.app.Name); err == nil {
			return got, true
		}
	}
	return nil, false
}

// warnManagerTreeMounts runs after apply kept or resumed a manager:
// manager.read_only is create-time only, so an older manager may not hold the
// mounts, and a protected directory replaced on the host since the create is no
// longer covered. Never fatal (the manager works, only unprotected) but loud.
// The container is found by scion's container name, which a resume keeps, and
// it is live here, so the live check (LiveTreeMountGaps) runs too.
func (r *run) warnManagerTreeMounts(ctx context.Context, jp string) {
	want := r.app.ManagerTreeMounts()
	if len(want) == 0 {
		return
	}
	if r.d.InspectContainerMounts == nil || r.d.CheckLiveMounts == nil {
		r.d.Log("start-manager: warning: manager.read_only is create-time only and lever could not inspect manager %q's mounts; run `lever doctor` (row \"manager read-only paths\") to see whether it holds them", r.app.Name)
		return
	}
	ref := jail.ContainerName(path.Base(jp), r.app.Name)
	got, err := r.d.InspectContainerMounts(ctx, ref)
	if err != nil {
		r.d.Log("start-manager: warning: manager.read_only is create-time only and lever could not inspect manager %q's mounts (%v); run `lever doctor` to check", r.app.Name, err)
		return
	}
	gaps := ManagerTreeMountGaps(jp, want, got)
	if gaps.Empty() {
		live, err := LiveTreeMountGaps(ctx, r.d.CheckLiveMounts, jp, ref, want)
		switch {
		case errors.Is(err, jail.ErrForeignNamespace):
			r.d.Log("start-manager: warning: manager %q: %v. Something in the container moved it there to show lever a staged view of its mounts: treat the manager as compromised and recreate it with `lever up --fresh`, without --keep-conversation", r.app.Name, err)
			return
		case err != nil:
			r.d.Log("start-manager: warning: lever could not check manager %q's live mounts from the guest (%v); run `lever doctor` to check", r.app.Name, err)
			return
		}
		gaps = live
	}
	if !gaps.Empty() {
		r.d.Log("start-manager: warning: manager %q does not hold its manager.read_only or shared_folders protection: %s. To protect them, %s", r.app.Name, gaps, ManagerTreeMountsFix(gaps))
		if tools := r.toolsOnReadOnly(); len(tools) > 0 {
			r.d.Log("start-manager: the broker does not start tool(s) %s until then: each runs a program from a protected directory, which this manager could have rewritten", strings.Join(tools, ", "))
		}
	}
}

// toolsOnReadOnly names the broker tools whose program relies on
// manager.read_only, sorted; the broker holds them back while the manager lacks
// the mounts (brokerctl.ReadOnlyGuard).
func (r *run) toolsOnReadOnly() []string {
	m, err := r.app.ToolsOnReadOnly()
	if err != nil {
		return nil
	}
	tools := make([]string, 0, len(m))
	for name := range m {
		tools = append(tools, name)
	}
	slices.Sort(tools)
	return tools
}

// warnManagerNestedVirt: nested_virt's /dev/kvm volume is create-time only, so
// a manager created before it was on lacks the device, and one created while it
// was on keeps it after it is off (a resume recreates the container from the
// record's volumes). With nested_virt off it probes Lima instances only: no
// other backend can hold the device. Never fatal.
func (r *run) warnManagerNestedVirt(ctx context.Context, jp string) {
	on := r.app.NestedVirt
	if !on && r.app.Backend != config.BackendLima {
		return
	}
	if r.d.ProbeContainerDevice == nil {
		if on {
			r.d.Log("start-manager: warning: nested_virt is on and lever could not probe manager %q for /dev/kvm; run `lever doctor` (row \"nested virt\")", r.app.Name)
		}
		return
	}
	has, err := r.d.ProbeContainerDevice(ctx, jail.ContainerName(path.Base(jp), r.app.Name), KVMDevice)
	if err != nil {
		if on {
			r.d.Log("start-manager: warning: nested_virt is on and lever could not probe manager %q for /dev/kvm (%v); run `lever doctor`", r.app.Name, err)
		}
		return
	}
	switch {
	case on && !has:
		r.d.Log("start-manager: warning: nested_virt is on but manager %q has no /dev/kvm; %s", r.app.Name, NestedVirtRecreateFix)
	case !on && has:
		r.d.Log("start-manager: warning: nested_virt is off but the manager %q still has /dev/kvm; %s", r.app.Name, NestedVirtRecreateFix)
	}
}

// NestedVirtRecreateFix is how a manager gains or drops /dev/kvm: a fresh
// create (a stop and up keeps the record's volumes).
const NestedVirtRecreateFix = "recreate it to apply the change: `lever up --fresh --keep-conversation` (`lever stop` + `lever up` keeps the old devices)"

// settleManagerPhase re-observes a record in a transitional phase until it
// reaches a phase planManager can act on, disappears (nil: a concurrent
// delete; the caller then creates), or the budget runs out. Exhaustion is a
// hard error that never touches the record. A list error mid-poll counts as
// "not yet"; a phase neither transitional nor known stops the wait at once so
// planManager refuses it by name.
func (r *run) settleManagerPhase(ctx context.Context, jp string, rec *scion.Agent) (*scion.Agent, error) {
	b := r.phaseSettle
	r.d.Log("start-manager: manager %q is in phase %q (mid-transition) — waiting up to %s for scion to settle it",
		r.app.Name, rec.Phase, time.Duration(b.Attempts)*b.Interval)
	cur := rec
	var listErr error
	err := wait.For(ctx, b.spec(wait.ManagerPhaseSettle.Name), func(ctx context.Context) wait.Result {
		agents, err := r.d.managerScion().List(ctx, jp)
		if err != nil {
			listErr = err
			return wait.NotYetErr(err)
		}
		listErr = nil
		cur = scion.FindAgent(agents, r.app.Name)
		if cur == nil || !slices.Contains(transitionalPhases, cur.Phase) {
			return wait.Ready()
		}
		return wait.NotYet(cur.Phase)
	})
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, wait.ErrTimeout) {
		return nil, err
	}
	if listErr != nil {
		// The last look failed, so cur is stale: say so rather than report a phase
		// nobody observed on the final attempt.
		return nil, fmt.Errorf("start-manager: manager %q was in phase %q and could not be re-observed within %s (last error: %v); nothing was changed. Retry, or run `lever up --fresh` to discard the session and start over",
			r.app.Name, cur.Phase, time.Duration(b.Attempts)*b.Interval, listErr)
	}
	if cur.Phase == "created" {
		return nil, fmt.Errorf("start-manager: manager %q stayed in phase %q for %s and did not settle; nothing was changed. %s",
			r.app.Name, cur.Phase, time.Duration(b.Attempts)*b.Interval, createdStuckHint(r.app.ManagerAgentImage()))
	}
	return nil, fmt.Errorf("start-manager: manager %q stayed in phase %q for %s and did not settle; nothing was changed. Retry once it settles (is a `lever stop` still running?), or run `lever up --fresh` to discard the session and start over",
		r.app.Name, cur.Phase, time.Duration(b.Attempts)*b.Interval)
}

// createdStuckHint explains the usual cause of a manager stuck in phase
// created: scion's start gives up after about 30 s, and the first rootless
// start of a large image spends longer than that on podman's id-mapped layer
// copy, so every retry starts from zero. load-image warms the copy; this names
// the manual warm-up for when that failed.
func createdStuckHint(image string) string {
	return fmt.Sprintf("The usual cause is the first start of a large image: podman builds a user-namespace layer copy that takes longer than scion's ~30 s start timeout, so scion retries from zero. "+
		"Check the `load-image` lines of this apply for a warm-up warning; then, in the guest as the jail user, run `podman run --rm %s --entrypoint true %s` (it can take a few minutes) and run `lever up --fresh` (a manager in phase created has no conversation to lose). "+
		"The guest's ~/.scion/server.log shows `context canceled` on each failed start",
		jail.ScionKeepIDUserns, image)
}

// prepareAPIKeyMode sets LEVER_LLM_AUTH=api-key on the manager's project before
// start, so its pre-start hook enters api-key mode (scion projects Hub env
// before pre-start hooks run). Project scope keeps it from other agents.
//
// It also sets the placeholder ANTHROPIC_API_KEY Hub secret, projected to every
// container, which is fine since the instance is uniformly api-key. It is not a
// credential: the agent's LLM credential is its broker capability token, and
// the broker /llm overwrites the placeholder with the real key. Without it
// scion's auth resolution refuses to launch the container, and with it
// lever-agent boot, which writes the real token.
func (r *run) prepareAPIKeyMode(ctx context.Context, jp string) error {
	if err := r.d.Scion.EnvSet(ctx, jp, "LEVER_LLM_AUTH", "api-key"); err != nil {
		return fmt.Errorf("set LEVER_LLM_AUTH for manager: %w", err)
	}
	if err := r.d.Scion.SecretSet(ctx, "ANTHROPIC_API_KEY", apiKeyPlaceholder); err != nil {
		return fmt.Errorf("set placeholder ANTHROPIC_API_KEY: %w", err)
	}
	return nil
}

// observeManager lists the project's agents and returns the manager's record
// (nil when absent), after refusing to keep a record whose stored role the
// installed scion would read as full (see Deps.VerifyAgentRole).
//
// scion's verbs are state-specific, hence observe then act: start creates (over
// a stopped record its 409 "already exists" text matches AlreadyRunning, so a
// blind start false-succeeds), and resume covers suspended and stopped records
// with claude --continue.
//
// The runtime broker registers asynchronously after the Hub API, so this first
// hub call can still hit that window (on a cold VM as "deadline exceeded"); it
// rides the same bounded retry as Start and Resume. A record in a transitional
// phase is waited on first (settleManagerPhase), so the role gate and the
// caller both see a settled one.
//
// The role gate covers only the phases planManager keeps the record in. It
// refuses rather than fall into recoverDeleteAndCreate on purpose: that
// discards the conversation, and losing the session is the operator's choice,
// not a guard's.
func (r *run) observeManager(ctx context.Context, jp string) (*scion.Agent, error) {
	agents, err := r.listAgentsRetry(ctx, jp)
	if err != nil {
		return nil, fmt.Errorf("start-manager: observing agents: %w", err)
	}
	rec := scion.FindAgent(agents, r.app.Name)
	// Under --fresh the record is discarded whatever its phase or role: waiting for
	// it to settle would be pointless, and the role guard's own remedy is the
	// delete --fresh performs, so it must not block it.
	if rec != nil && !r.fresh && slices.Contains(transitionalPhases, rec.Phase) {
		if rec, err = r.settleManagerPhase(ctx, jp, rec); err != nil {
			return nil, err
		}
	}
	if rec != nil && !r.fresh {
		switch rec.Phase {
		case scion.PhaseRunning, scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError:
			if err := r.d.VerifyAgentRole(ctx, path.Base(jp), r.app.Name); err != nil {
				return nil, fmt.Errorf("start-manager: %w", err)
			}
		}
	}
	return rec, nil
}

// resumeVerb is one of planManager's two resumable verbs: scion resume for a
// suspended or stopped record, scion resume --force for an error record.
type resumeVerb struct {
	label  string // verb name in the log line and the refusal
	resume func() error
}

func resumeVerbPlain(run func() error) resumeVerb {
	return resumeVerb{label: "resume", resume: run}
}

func resumeVerbForce(run func() error) resumeVerb {
	return resumeVerb{label: "resume --force", resume: run}
}

// resumeOrRecover stages fresh bootstrap material and runs the verb through the
// runtime-broker-race retry. On failure it keeps a manager that recovered
// concurrently and otherwise refuses, leaving the record and its conversation
// in place: a failed resume says the session cannot come up now, not that it is
// worthless, and discarding it is the operator's call (lever up --fresh).
//
// ensureFreshBootstrap runs first so that a manager whose short-lived agent
// leaf expired while it was stopped (the renew sidecar cannot run then) can
// re-enrol on boot: lever-agent boot re-enrols an expired leaf only when an
// unspent ticket is staged. It is a no-op when this run already minted
// material, and an unspent ticket is harmless when the leaf is still valid
// (boot skips enrolment).
func (r *run) resumeOrRecover(ctx context.Context, jp string, v resumeVerb) error {
	if err := r.ensureFreshBootstrap(ctx); err != nil {
		return err
	}
	rerr := r.resumeClearing(ctx, jp, v.resume)
	if rerr == nil {
		return nil
	}
	if r.managerConcurrentlyRecovered(ctx, jp) {
		r.d.Log("start-manager: %s failed (%v) but the manager is now running — recovered concurrently (auto-re-enrol healer); keeping the session", v.label, rerr)
		return nil
	}
	// A hub refusal says nothing about the manager.
	return resumeError(v.label, rerr)
}

// ResumeFailed is the error for a manager resume that failed and was not a hub
// refusal: the record stays, and the operator decides. lever up's own resume
// path returns it too. Scripts tell it by exit code
// (cli.ExitManagerResumeFailed). The fixed sentences come first and the hub's
// words last: the cause can carry text an agent chose, which must never sit
// where a reader expects lever's own statement.
func ResumeFailed(verb string, err error) error {
	return cli.WithExitCode(fmt.Errorf("start-manager: %s of the manager failed. lever did NOT delete the manager: its record and its conversation are kept. "+
		"Run `lever up` again (a transient failure clears), and `lever doctor` for the cause if it does not. "+
		"`lever up --fresh` discards the session and starts a new manager. Cause: %s", verb, termsafe.Sanitize(scion.ErrSummary(err))),
		cli.ExitManagerResumeFailed)
}

// HubRefusedResume is the error for a resume the hub refused. The manager is
// not broken and is kept; `--fresh` is the wrong answer to it, so it has its
// own exit code (cli.ExitHubRefusedResume) and does not share ResumeFailed's
// wording.
func HubRefusedResume(verb string, err error) error {
	return cli.WithExitCode(fmt.Errorf("start-manager: the hub refused %s of the manager. lever kept the manager and its conversation. %s. Cause: %s",
		verb, scion.RefusalHint, termsafe.Sanitize(scion.ErrSummary(err))),
		cli.ExitHubRefusedResume)
}

// retryOnBrokerUnavailable retries action within r.brokerStart while each
// failure is the transient runtime-broker-unavailable race
// (scion.IsBrokerUnavailable). Any other error returns at once: the budget
// absorbs the registration race and must not mask real failures.
func (r *run) retryOnBrokerUnavailable(ctx context.Context, action func() error) error {
	var last error
	err := wait.For(ctx, r.brokerStart.spec(wait.BrokerUnavailable.Name), func(context.Context) wait.Result {
		last = action()
		switch {
		case last == nil:
			return wait.Ready()
		case !scion.IsBrokerUnavailable(last):
			return wait.Fatal(last)
		}
		return wait.NotYetErr(last)
	})
	if errors.Is(err, wait.ErrTimeout) {
		return last // the transient error itself
	}
	return err
}

// listAgentsRetry lists the project's agents through retryOnBrokerUnavailable.
// waitManagerLive's List deliberately does not use it: it has its own tolerance
// within the liveness budget.
func (r *run) listAgentsRetry(ctx context.Context, jp string) ([]scion.Agent, error) {
	var agents []scion.Agent
	if err := r.retryOnBrokerUnavailable(ctx, func() error {
		a, e := r.d.managerScion().List(ctx, jp)
		if e != nil {
			return e
		}
		agents = a
		return nil
	}); err != nil {
		return nil, err
	}
	return agents, nil
}

// managerConcurrentlyRecovered re-observes the manager after a failed resume.
// The broker's auto-re-enrol healer bounces lapsed agents with the same scion
// verbs, in a separate process with no coordination, so a failure here can
// mean the healer's own suspend and resume was mid-flight: a record running now
// is a success. The list is retried because a blip here is correlated with the
// failed resume; errors past the budget count as not recovered.
func (r *run) managerConcurrentlyRecovered(ctx context.Context, jp string) bool {
	agents, err := r.listAgentsRetry(ctx, jp)
	if err != nil {
		return false
	}
	if a := scion.FindAgent(agents, r.app.Name); a != nil {
		return a.Phase == scion.PhaseRunning
	}
	return false
}

// startManagerCreate runs scion start through the runtime-broker-race retry and
// treats an "already running"/"already exists" 409 as success (an idempotent
// re-apply, or a record scion's lazy hub-sync briefly read as absent) unless
// mustCreate. A created record has no agent home to reuse, so lever-agent boot
// always re-enrols: ensureFreshBootstrap makes sure the latch is armed, or the
// create would 403 and the container exit 1.
func (r *run) startManagerCreate(ctx context.Context, opts scion.StartOpts, mustCreate bool) error {
	// The second read_only host check: after a --fresh delete the manager that
	// might have held the tree writable is gone, so this is the look the create can
	// rely on.
	if err := r.app.PrepareManagerReadOnlyHost(); err != nil {
		return fmt.Errorf("start-manager: %w", err)
	}
	// shared_folders: hard links out of a folder are checked when the manager's
	// protection begins.
	if err := r.app.CheckSharedFolderLinks(); err != nil {
		return fmt.Errorf("start-manager: %w", err)
	}
	if err := r.ensureFreshBootstrap(ctx); err != nil {
		return err
	}
	var commit func() error
	switch {
	case r.unattended && r.unattendedBegin != nil:
		// After the delete, with the old container gone: the skill is read
		// again here, and only this reading can be recorded.
		c, berr := r.unattendedBegin()
		if berr != nil {
			return fmt.Errorf("start-manager: the manager record was deleted for a fresh start lever made on its own, but lever did not create the manager: it could not record that the new session's skill is unchecked (%s). Run `lever up --fresh`", termsafe.Sanitize(berr.Error()))
		}
		commit = c
	case r.unattended:
	case r.d.BeginSession != nil:
		commit = r.d.BeginSession(r.app.Name)
	}
	created := false
	err := r.retryOnBrokerUnavailable(ctx, func() error {
		startErr := r.d.managerScion().Start(ctx, opts)
		// A manager already running or existing is success, not error, but it is not a
		// fresh session, so it is not recorded as one.
		if startErr != nil && scion.AlreadyRunning(startErr) {
			if mustCreate {
				// Under --fresh nothing may exist here: the observe step missed a live record,
				// or an earlier attempt of this create left one behind and reported a timeout.
				// lever cannot tell which, so it claims neither a discard nor a fresh manager.
				return fmt.Errorf("start-manager: --fresh expected no manager record at this point, and the create answered that one exists. "+
					"lever did NOT report a fresh manager: the record may be the previous session, or one a timed-out attempt of this run left behind. "+
					"Run `lever up --fresh` again; it deletes the record it finds and creates a new manager. Cause: %s", termsafe.Sanitize(scion.ErrSummary(startErr)))
			}
			return nil
		}
		created = startErr == nil
		return startErr
	})
	if err == nil && created {
		r.managerCreated = true
	}
	if err == nil && created && commit != nil {
		if cerr := commit(); cerr != nil {
			if r.unattended && !r.unattendedRefused {
				r.d.Log("start-manager: this fresh start, which lever made on its own, is not recorded as a new session (%s); contacts cannot post to the manager until `lever up --fresh` starts it fresh", termsafe.Sanitize(cerr.Error()))
			} else {
				r.d.Log("start-manager: could not record the manager's fresh session (%v); contacts cannot post to it until the next fresh start", cerr)
			}
		}
	}
	return err
}

// recoverDeleteAndCreate logs the loud discard notice, deletes the manager
// record, and creates a fresh manager only once the record is gone. Every
// manager record delete goes through here; a failed resume refuses instead.
// A Start over an undeleted record would 409, so a delete failure is a hard
// error ("start-manager: <deleteFailReason> and delete failed: %w").
func (r *run) recoverDeleteAndCreate(ctx context.Context, jp string, opts scion.StartOpts, logMsg, deleteFailReason string) error {
	r.d.Log("%s", logMsg)
	// The delete rides the same runtime-broker-race retry: --fresh is the
	// operator's one way out of a manager that does not resume, so it must not fail
	// on a broker that was only late.
	derr := r.retryOnBrokerUnavailable(ctx, func() error { return r.d.managerScion().Delete(ctx, r.app.Name, jp) })
	// What decides is the record, never an error's words: an attempt can remove the
	// record and still report a failure, or report success with the record still
	// there. --fresh evicts a session the operator no longer trusts, so
	// "discarded" must be true before a create follows.
	agents, lerr := r.listAgentsRetry(ctx, jp)
	if lerr != nil {
		if derr != nil {
			return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
		}
		return fmt.Errorf("start-manager: %s: the delete was sent, but lever could not confirm the manager record is gone (%v); nothing was created. Run `lever up --fresh` again", deleteFailReason, lerr)
	}
	if rec := scion.FindAgent(agents, r.app.Name); rec != nil {
		if errors.Is(derr, scion.ErrDeleteUnconfirmed) {
			return fmt.Errorf("start-manager: %s: the hub accepted the delete but the manager record is still there (phase %s), so nothing was created. "+
				"The hub may still be deleting it: check `lever doctor`, then run `lever up --fresh` again", deleteFailReason, scion.BoundedQuote(rec.Phase))
		}
		if derr != nil {
			return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
		}
		return fmt.Errorf("start-manager: %s: the delete reported no error, but the manager record is still there (phase %s). The previous session was NOT discarded and nothing was created. Run `lever up --fresh` again", deleteFailReason, scion.BoundedQuote(rec.Phase))
	}
	return r.startManagerCreate(ctx, opts, true)
}

// ensureFreshBootstrap makes sure fresh, enrolable bootstrap material is staged
// before a manager start or resume: a no-op when this run minted it, otherwise
// RearmBootstrap re-arms the latch and stages it, once per run. A failed re-arm
// is a hard error: a manager without enrolable bootstrap would only crash-loop.
func (r *run) ensureFreshBootstrap(ctx context.Context) error {
	if r.minted {
		return nil
	}
	if err := r.d.RearmBootstrap(ctx); err != nil {
		return fmt.Errorf("start-manager: re-arming the broker's spent bootstrap latch: %w", err)
	}
	r.minted = true
	return nil
}

// waitManagerLive polls until the manager's record shows phase running and a
// live container (scion.ContainerLive). It backstops both false successes: a
// blind scion start's 409 text matches AlreadyRunning, and scion resume and
// start report success for a container that dies moments later (scion's
// liveness check is a single immediate poll). Trusting the observed record, not
// exit codes or error wording, is what makes start-manager's success mean
// something.
func (r *run) waitManagerLive(ctx context.Context, jp string, acted bool, onHarness func()) error {
	b := r.managerLive
	if !acted {
		// Nothing was started: the manager was already running, so the settle window
		// would prove nothing and cost every apply ten seconds. One look still refuses
		// a running record over a dead container.
		b.Settle = 0
	}
	// Only a manager this run created is attached at its pane. A resume runs claude
	// --continue, which exits a second or two after its pane comes up when there is
	// no conversation, so the gate waits for claude's session start first and that
	// exit is caught here, where start-manager can start the manager fresh. A
	// created manager's later death is caught by CheckManagerAfterDetach.
	attach := r.attach && r.managerCreated
	err := gateManagerLive(ctx, r.d, b, r.app.Name, jp, "start-manager", r.progress, gateOpts{attach: attach, onHarness: onHarness})
	return withHarnessDeathHint(ctx, r.d, err, jp, r.app.Name)
}

// observeAttempts bounds CheckManagerAfterDetach's list retries: enough to
// ride out a transient hub error, short enough that an answer arrives in
// seconds.
const observeAttempts = 3

// JailPath maps a host path under tree to its location inside the jail. It
// returns hostPath unchanged when mount is empty or hostPath is not under tree.
func JailPath(hostPath, tree, mount string) string {
	if mount == "" || tree == "" {
		return hostPath
	}
	rel, err := filepath.Rel(tree, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hostPath
	}
	if rel == "." {
		return mount
	}
	return path.Join(mount, filepath.ToSlash(rel))
}

// maxCredentialBytes caps the credential file size — a token is small; a large
// file is a sign the path points at something that isn't a credential.
const maxCredentialBytes = 64 << 10

// defaultReadCred reads a credential file, refusing group- or world-accessible
// and oversized files: defence in depth for the credential projected into agent
// containers.
func defaultReadCred(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	// Group bits count too: this is the manager's subscription LLM credential, the
	// longest-lived secret lever handles, and every other credential is held to
	// 0600 (lever doctor fails a group-readable one).
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential file %s is group- or world-accessible (%#o) — restrict it to 0600", path, info.Mode().Perm())
	}
	if info.Size() > maxCredentialBytes {
		return "", fmt.Errorf("credential file %s is %d bytes — too large to be a credential", path, info.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// HubAuthEnv is the env every hub start carries: scion's proxy auth mode with
// the JWT provider, invite-only sign-in and lever-admin as the only admin
// email. scion maps compound settings only when written without inner
// underscores (USERACCESSMODE, PUBLICKEYFILE); the underscored spellings are
// silently ignored, and an ignored user-access mode admits anyone.
// LEVER_HUB_AUTH_KEY records which key the hub read, for HubRestartReason.
func HubAuthEnv(instanceID, keyFingerprint string) map[string]string {
	return map[string]string{
		"SCION_SERVER_AUTH_MODE":                    "proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE":          "invite_only",
		"SCION_SERVER_AUTH_PROXY_PROVIDER":          "jwt",
		"SCION_SERVER_AUTH_PROXY_JWT_ALGORITHM":     hubauth.Algorithm,
		"SCION_SERVER_AUTH_PROXY_JWT_HEADER":        hubauth.Header,
		"SCION_SERVER_AUTH_PROXY_JWT_ISSUER":        hubauth.Issuer(instanceID),
		"SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE":      hubauth.Audience(instanceID),
		"SCION_SERVER_AUTH_PROXY_JWT_PUBLICKEYFILE": hubauth.GuestPublicKeyPath,
		"SCION_SERVER_HUB_ADMINEMAILS":              hubauth.AdminEmail,
		"SCION_SERVER_AUTH_DEFAULTUSERROLE":         "member",
		"LEVER_HUB_AUTH_KEY":                        keyFingerprint,
	}
}
