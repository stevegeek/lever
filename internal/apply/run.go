package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wire"
)

// ErrBootstrapLatched is returned by MintManagerBootstrap when the broker's
// single-use /bootstrap latch is already consumed (HTTP 403). The mint step
// tolerates it (the manager already has its bootstrap from a prior apply against
// the SAME broker process). A broker RESTART reopens the latch, so mint then
// succeeds and re-deposits a fresh ticket — letting a partially-failed first
// apply recover on re-apply (vs the old skip-if-file-exists, which deadlocked).
var ErrBootstrapLatched = errors.New("broker /bootstrap latch already consumed")

// RetryBudget bounds one of apply's polls: Attempts tries, Interval apart.
// A zero value means the production default (see Deps).
type RetryBudget struct {
	Attempts int
	Interval time.Duration
	// Settle is how long a record must STAY live after it is first observed
	// live before the manager counts as up (scion.LiveBudget.Settle). Only the
	// manager liveness budget reads it.
	Settle time.Duration
}

// or fills b's unset Attempts/Interval from def; the zero value is def
// entirely. Settle is taken as given once anything is set — zero settle is a
// legitimate explicit choice (every test relies on it), so only the all-zero
// budget inherits def's.
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

// defaultBrokerStartRetry bounds the start-manager retry that absorbs the
// runtime-broker registration race: the scion runtime broker registers with
// the hub ASYNCHRONOUSLY after the server starts, so a start-manager that runs
// too soon gets "no runtime brokers available". The hub itself is up (the
// scion-server health check passed), so this is purely a timing window — retry
// until the broker comes online. (Only the first start races; workers start
// later when the broker is ready.)
var defaultBrokerStartRetry = RetryBudget{Attempts: 30, Interval: time.Second}

// defaultManagerLiveRetry bounds waitManagerLive's post-start poll.
// The 10 s settle is lever#31: scion reports the record running before the
// harness has run a line, and every harness death observed so far (a task past
// the tmux cap, `claude --continue` with no conversation) landed within three
// seconds of that. Ten seconds catches those with margin and is still
// probabilistic for a later death.
var defaultManagerLiveRetry = RetryBudget{Attempts: 15, Interval: time.Second, Settle: 10 * time.Second}

// defaultPhaseSettleRetry bounds settleManagerPhase's wait for a record caught
// mid-transition (transitionalPhases). A container start (pre-start hook,
// enrolment) or a graceful stop each take a few seconds on a warm VM and
// longer on a cold one; the budget errs long because exhausting it is a loud
// failure the operator has to act on, while a settle that lands early costs
// nothing but one more list.
var defaultPhaseSettleRetry = RetryBudget{Attempts: 60, Interval: time.Second}

// transitionalPhases are the phases of scion's record enum
// (pkg/agent/state/state.go) that a record only passes THROUGH: created,
// provisioning and cloning on the way to a first start, starting on the way
// to running, stopping on the way to suspended/stopped. convergeManager has
// no verb for any of them, and a snapshot of one says nothing about where the
// record ends up — so apply waits for it to settle (settleManagerPhase)
// rather than acting on the snapshot. Before P6 such a snapshot took the loud
// delete+fresh path, which discarded the conversation on a routine `lever up`
// that raced a `lever stop`.
//
// "resumed" is not in scion's hub phase enum (pkg/agent/state/state.go); it
// is the local runtime's interim status after `scion resume`, written to the
// record until the container's own sciontool reports "running" (16s live on
// 2026-09-11: the broker's lapse healer bounced the manager and a `lever up`
// inside that window was refused as an unknown phase). It settles on its own,
// so it is transitional here too.
var transitionalPhases = []string{"created", "provisioning", "cloning", "starting", "stopping", "resumed"}

// apiKeyPlaceholder is the sentinel ANTHROPIC_API_KEY set as a Hub secret for
// api-key instances. It is NOT a real credential: it exists only to satisfy
// scion's start-time auth gate so the container (and lever-agent boot) can run.
// claude sends it as x-api-key to the broker /llm, which strips it and injects
// the real Console key host-side. Shaped like an Anthropic key (sk-ant- prefix,
// long) in case scion's auth resolution sanity-checks the format.
const apiKeyPlaceholder = "sk-ant-placeholder0lever0broker0injects0the0real0key0do0not0use000000000000000000000000"

// BootstrapMaterial is what the manager's lever-agent consumes to enrol. It is
// an alias for wire.Bootstrap — the ONE declaration of the enrolment envelope
// (previously this type carried its own hand-maintained, "must stay identical"
// copy of the json tags). The alias keeps the apply-domain name at every call
// site while the field/tag definition lives in exactly one place.
type BootstrapMaterial = wire.Bootstrap

// run is one apply execution: the config it converges, the collaborators it
// acts through, and the one piece of cross-step state the plan carries.
// Every step executor is a method on it so the per-step signatures stop
// repeating (ctx, app, d, name, jp, ...).
type run struct {
	app *config.App
	d   Deps
	// brokerStart, managerLive and phaseSettle are the resolved retry
	// budgets (Deps overrides or the defaults), fixed for the run.
	brokerStart, managerLive, phaseSettle RetryBudget
	// minted records whether THIS apply run actually minted fresh bootstrap
	// material (as opposed to the mint-manager-bootstrap step tolerating an
	// already-spent latch — e.g. an idempotent re-apply against the same broker
	// process; see ErrBootstrapLatched). start-manager's create path needs
	// exactly that "did we mint fresh material this run" signal.
	minted bool
	// fresh is PlanOpts.Fresh: discard any present manager record at
	// start-manager (lever#33).
	fresh bool
	// managerCreated records that THIS run created the manager record (a
	// create that answered "already exists" does not count). Only a created
	// record is known to carry the manager.read_only mounts; any other is
	// probed and warned about (warnManagerTreeMounts).
	managerCreated bool
}

// StageBootstrapMaterial writes m as the manager's one-time enrolment ticket
// into treeDir/.lever/bootstrap.json (0600) — the path lever-agent boot reads
// by convention (see start-manager's LEVER_BOOTSTRAP comment below). This is
// the ONE staging code path, shared by the mint-manager-bootstrap step and
// start-manager's create-path re-arm (Deps.RearmBootstrap, implemented by the
// CLI, which stages directly into the tree since start-manager's Step.Target
// is the manager's slug, not the tree dir — see JailPath/Plan).
func StageBootstrapMaterial(treeDir string, m BootstrapMaterial) error {
	// treeDir is the confinement anchor: it is the mount point, the one
	// component an agent cannot replace. Everything below it is agent-writable,
	// so wire.Stage refuses to follow a symlink planted at `.lever`.
	return wire.Stage(treeDir, ".lever", m)
}

// Deps are the executor's collaborators, injected so Run is testable offline.
// LoadImage and friends are host-side (docker-save|podman-load); Scion runs IN
// the jail (built on a JailRunner). Every func field except ReadCred,
// ImageTagPolicy, InspectContainerMounts and ProbeContainerWritable is REQUIRED: Run refuses (Deps.check) before its first step when one is nil,
// so a wiring gap fails loudly instead of silently skipping a step. The CLI
// wires all of them (buildApplyDeps); tests fill the ones they do not assert
// on with inert implementations.
type Deps struct {
	LoadImage func(ctx context.Context, imageRef string) error
	// ImageLoaded reports whether the jail already holds imageRef at the same
	// image ID as the host, letting the load-image step skip a redundant
	// multi-GB `docker save | podman load` re-stream. This is what stops a
	// re-apply (including the first-boot retry loop, which re-runs the WHOLE
	// plan on any step failure) from re-importing every image each time. It is
	// fail-open by construction (false on any uncertainty), so a wrong answer
	// costs a redundant load, never a wrongly-skipped one.
	ImageLoaded func(ctx context.Context, imageRef string) bool
	// LoadImageTar and ImageLoadedTar are the same pair for an image that
	// ships as a docker archive (Step.TarPath, from image_tar): the load
	// streams the file into the jail and the guard compares the archive's
	// config digest with the jail's ID, so neither touches host docker
	// (lever#32). Same fail-open contract as ImageLoaded. The load takes
	// ImageTagPolicy as its allowTag.
	LoadImageTar   func(ctx context.Context, imageRef, tarPath string, allowTag func(ref string) error) error
	ImageLoadedTar func(ctx context.Context, imageRef, tarPath string) bool
	// ImageTagPolicy is applied to EVERY tag an image_tar archive carries
	// before a byte of it is streamed into the jail (R4): a docker archive
	// can hold several images and `podman load` imports all of them, so the
	// config ref alone passing security.allowed_image_registries proved
	// nothing about the rest. OPTIONAL: nil = no policy (the CLI wires
	// config.Security.ImageTagPolicy, which is nil when no allowlist is set).
	ImageTagPolicy func(ref string) error
	// PruneImages reclaims dangling images from the jail after a load, so a
	// rebuilt image does not ratchet the grow-only jail disk up by a full image
	// size (the superseded copy goes untagged). A no-op when the load added a
	// new image. Best-effort: a prune failure is logged, not fatal to the
	// bring-up.
	PruneImages func(ctx context.Context) error
	Scion       *scion.Client
	ReadCred    func(path string) (string, error) // nil ⇒ defaultReadCred
	JailMount   string                            // jail path where app.Tree is bind-mounted (e.g. "/lever"); "" disables translation
	// HubSessionSecret is the hub's session-cookie signing key, threaded into
	// every hub start this package orders (HubServerOpts). The CLI ensures it
	// host-side before Run (state.State.EnsureSessionSecret), so the same
	// key survives hub restarts and browser sessions with it. An argv-only
	// option: a hub already running keeps its old key until something restarts
	// it, and introducing the secret does NOT order that restart itself — a
	// restart drops every agent's hub connection and, with the old key random
	// and in-memory, would force the very logout it exists to prevent; the
	// next restart the hub was getting anyway adopts the key at no extra cost.
	// "" omits the flag (tests; scion falls back to a per-boot random key).
	HubSessionSecret string
	StartBroker      func(ctx context.Context) error
	BrokerHealthy    func(ctx context.Context) error
	// EnsureControllerPAT runs the bootstrap-token step: the whole controller-PAT
	// mint window as one injected op, keeping this package scion-agnostic (the
	// CLI wires the real throwaway-hub → mint → persist → kill → delete-dev-token
	// logic — see plan.go's bootstrap-token Step doc). It MUST be idempotent: if
	// a valid PAT is already persisted, no-op.
	EnsureControllerPAT func(ctx context.Context) error
	// WaitBrokerReady blocks until the scion runtime broker is registered AND
	// online, right before start-manager acts. The workstation daemon brings up
	// its Hub API (confirmed by scion-server's waitHubReady) and its runtime
	// broker separately, so without this gate the first create/resume races the
	// broker's async registration — the flakiness that made first-boot need a
	// second `up`. The implementation is fail-soft (returns nil on timeout), so
	// it never fails the bring-up on its own; the start path's broker-unavailable
	// retry still backstops it.
	WaitBrokerReady      func(ctx context.Context, project string) error
	MintManagerBootstrap func(ctx context.Context) (BootstrapMaterial, error)
	// RearmBootstrap restarts the broker (re-arming its single-use /bootstrap
	// latch; broker CA + signing keys persist on disk so existing agent certs
	// and capability tokens survive the restart), then mints AND STAGES fresh
	// bootstrap material exactly like the mint-manager-bootstrap step. Called
	// by start-manager's create path when no fresh material was minted this
	// apply (the mint step tolerated a spent latch).
	RearmBootstrap func(ctx context.Context) error
	// EnsureHubLogin brings the GUEST half of the remote-access login path up
	// to date before the hub starts: the loopback forwarder that makes
	// lever's host-side OIDC provider look local to the hub, and the
	// `oidc_login` block in the guest's ~/.scion/settings.yaml. It reports
	// whether that configuration changed, which is the signal to restart a
	// hub that is already running — see scionServer. It is the CLI's job
	// to make this a no-op when remote access is off.
	EnsureHubLogin func(ctx context.Context) (bool, error)
	// DisableHubLogin converges the GUEST half of the login path off: stop and
	// remove the forwarder, drop the `oidc_login` block. Like StopRemoteProxy
	// it is NOT a Plan step — Run calls it whenever app.RemoteEnabled() is
	// false, so an instance that turned remote access back off does not keep
	// an unauthenticated jail→host loopback bridge alive for a feature that no
	// longer exists.
	//
	// It reports "changed" on the same terms EnsureHubLogin does, and Run acts
	// on it the same way: a change means a hub that is already running was
	// started from state this call has now removed, so it is restarted. See
	// disableHubLogin for what counts as a change and why the forwarder does
	// not.
	DisableHubLogin func(ctx context.Context) (bool, error)
	// EnsureScionTelemetry converges the top-level `telemetry:` block of the
	// guest's ~/.scion/settings.yaml on config scion.telemetry (the CLI binds
	// the mode), reporting whether the file changed. It runs in the
	// scion-server step, before the hub starts, so a first bring-up creates
	// the hub and the manager with it already in place.
	//
	// A change is LOGGED, never acted on: the runtime broker reads the block at
	// every agent start, but a running container keeps the env it was created
	// with and the hub caches the block at its own startup. `lever stop &&
	// lever up` applies it everywhere; lever does not bounce a running hub or
	// manager for a telemetry posture.
	EnsureScionTelemetry func(ctx context.Context) (bool, error)
	// EnsureAgentTemplate backs the agent-template step: put lever's overlay
	// template in front of scion's stock `default` so newly provisioned agents
	// do NOT launch with `--system-prompt '# Placeholder'`, which replaces
	// Claude Code's entire built-in system prompt. projectDir is JAIL-side —
	// the scion client behind it runs in the jail, where the host tree exists
	// only at the mount point (project scope is the only settings scope that wins) and
	// reports whether it changed anything.
	//
	// Provisioning-time only, by nature: scion stages an agent's system prompt
	// once, when its home is created, and never re-stages it. So this governs
	// agents created from now on; an agent that already exists keeps whatever
	// it was provisioned with until its staged input is changed in place.
	EnsureAgentTemplate func(ctx context.Context, projectDir string) (bool, error)
	// BeginSession, when set, is called before the manager is created and
	// returns what records that session's fresh start (package sessionrec):
	// it notes the skill on disk now, and the returned commit writes the
	// record once scion created the agent. It is never called on a resume,
	// and commit never runs when scion reports the agent already existed, so
	// a resumed conversation is never recorded as fresh. The remote proxy
	// lets a contact post to an agent only after such a record. A commit
	// error is logged, not fatal: the manager is up, and without the record
	// contacts are refused (fail closed). Nil records nothing.
	BeginSession func(agent string) (commit func() error)
	// InspectContainerMounts reads a jail container's mounts (id or name),
	// jail.ContainerMounts in production. Used to warn when a manager that
	// apply kept or resumed lacks the manager.read_only mounts, which only a
	// create can add, and, before the keep or resume, to find mounts the
	// record holds that the config dropped or whose directory is gone
	// (checkStaleTreeMounts). nil ⇒ the warning says it could not check.
	InspectContainerMounts func(ctx context.Context, ref string) ([]jail.Mount, error)
	// ProbeContainerWritable asks a running container whether its user can
	// write a path (jail.ContainerPathWritable in production): the live
	// check that a read_only entry was not replaced on the host after the
	// manager was created. nil ⇒ the warning says it could not check.
	ProbeContainerWritable func(ctx context.Context, ref, target string) (bool, error)
	// ProbeContainerDevice asks a running container whether path is a
	// character device in it (jail.ContainerHasCharDevice in production).
	// Used to warn when nested_virt is on and the manager's container lacks
	// /dev/kvm (the podman drop-in reaches containers at create time). nil
	// means the warning says it could not check. Optional.
	ProbeContainerDevice func(ctx context.Context, ref, path string) (bool, error)
	// RecordVolumes reads the extra mounts the hub record of agent in
	// project was created with (its inline-config volumes, through the
	// controller's read-only agent listing), as jail mounts. It is
	// checkStaleTreeMounts' fallback when the manager has no container to
	// inspect. nil (or an error) ⇒ no fallback.
	RecordVolumes func(ctx context.Context, project, agent string) ([]jail.Mount, error)
	// AgentSession reads a running agent's session from inside its container
	// (jail.AgentProbe in production) so start-manager can heal an expired
	// hub token and a hub phase "stopped" over a harness that still runs
	// (HealAgentSession) before it converges the manager, and the workers'
	// after. nil ⇒ no heal.
	AgentSession AgentSessionProbe
	// AgentRevoked reports an agent (by slug) the broker has revoked, read
	// from the persisted revocation list, so the session heal does not reset
	// a revoked agent's hub token. nil ⇒ none known.
	AgentRevoked func(agent string) bool
	// StartRemoteProxy backs the remote-proxy step (present only when
	// app.RemoteEnabled(); see Plan): spawn — or confirm already running —
	// the daemonized `lever remote serve` proxy (a config with remote disabled
	// never reaches this step at all).
	StartRemoteProxy func(ctx context.Context) error
	// StopRemoteProxy tears the remote proxy down (by pidfile, tolerant of
	// an absent or stale one). It is NOT called from a Plan step — Run calls
	// it directly, unconditionally, whenever app.RemoteEnabled() is false,
	// so a proxy left running from a prior apply (remote.enabled flipped
	// back off since) converges to stopped rather than going on serving
	// traffic the config no longer authorizes.
	StopRemoteProxy func(ctx context.Context) error
	// RemoveJailFile removes a regular file at a jail-absolute path, through the
	// jail's own filesystem view. Used for the stale `.scion` marker so the
	// removal and the subsequent in-jail `scion init` cannot race across the
	// host/guest VirtioFS boundary (a host-side unlink is not promptly visible
	// to the guest's directory cache). Must NOT remove directories.
	RemoveJailFile func(ctx context.Context, jailPath string) error
	// RemoveScionProjectConfigs removes any stale ~/.scion/project-configs
	// registration(s) whose workspace_path == jailWorkspacePath, BEFORE the
	// register-project step re-inits. Without this, every apply
	// mints a fresh registration via `scion init` and the old ones accumulate
	// (the `lever doctor` "duplicate registrations" finding) — this is the
	// removal counterpart to RemoveJailFile's marker-race fix above.
	RemoveScionProjectConfigs func(ctx context.Context, jailWorkspacePath string) error
	// ScionProjectRegistered observes whether jailWorkspacePath already has
	// exactly one valid scion registration (one project-configs entry + the
	// in-tree marker present) BEFORE the register-project step
	// decides whether to run its destructive clean+init path at all. true →
	// skip marker removal, RemoveScionProjectConfigs, and `scion init`/`hub
	// link` entirely, so a re-apply does not tear down (and orphan) a
	// resumable scion agent record just to re-mint an identical registration.
	// A query error falls through to the destructive path unchanged
	// (fail-open — an observe failure must never turn into a hard apply
	// failure, and zero/duplicate/torn registrations legitimately need it).
	ScionProjectRegistered func(ctx context.Context, jailWorkspacePath string) (bool, error)
	// StripProjectSharedDirs removes scion's default `scratchpad` shared dir
	// from the hub's record of the named project (scion#925). The hub stamps it
	// on every NEW project and mounts it read-write into EVERY agent of that
	// project, which is a writable channel between the manager and every worker
	// — the opposite of lever's subtree isolation. lever's hub runs file/SQLite,
	// where the server-side default cannot be turned off, so per-project removal
	// is the only route. Runs on BOTH register paths (already-registered and
	// freshly re-inited), because a project created before this step existed
	// still carries the mount. The broker-only VM gate never reaches it at
	// all: Plan filters KindRegisterProject out entirely.
	//
	// A failure is fatal to apply. The alternative is starting a manager and
	// workers that share a writable directory while the operator believes
	// they do not, and a silent security regression is worse than a loud
	// bring-up failure. The hub knows the project by the workspace basename
	// — the same name ensureControllerPAT passes to `hub token create` — so
	// the two stay consistent by construction.
	StripProjectSharedDirs func(ctx context.Context, projectName string) error
	// EnsureAgentRoleCeiling writes the project's max/default agent role so the
	// HUB refuses any create above the role lever stamps — the hub-side
	// counterpart of `--role` (see hubapi.Client.EnsureAgentRoleCeiling). Runs
	// on both register paths, like the strip, because a project created before
	// this step existed has no ceiling. A failure is fatal to apply for the
	// same reason: an instance whose only role bound is lever's own stamp,
	// while the operator believes the hub enforces one, is a silent
	// regression.
	EnsureAgentRoleCeiling func(ctx context.Context, projectName string) error
	// RepairScionHubEndpoint rewrites the hub endpoint recorded in the project's
	// scion registration when it no longer matches the real hub. Minting the
	// controller PAT `hub link`s the project against a THROWAWAY hub on its own
	// port; the register step's re-init would overwrite that, but it is skipped
	// whenever the registration is already sound, so a re-mint on an established
	// instance leaves the project pointing at a dead port. Every lever call
	// passes the endpoint explicitly, so the breakage lands only where scion runs
	// bare in the jail — `lever attach` (live failure 2026-08-11).
	RepairScionHubEndpoint func(ctx context.Context, jailWorkspacePath string) error
	// VerifyAgentRole gates KEEPING an existing agent record. It returns a
	// descriptive error when the hub's record for that agent stores no
	// authorization role while the installed scion understands roles — the state
	// a pin bump across scion#1089 leaves behind, because the role is written on
	// the create path only and is immutable after.
	//
	// That combination is not benign: scion#1102 resolves an unset stored role
	// to `full` (agent create, agent lifecycle, project-secret-read) at dispatch
	// and, since scion#1101, on every token refresh. `scion resume` carries no
	// --role flag, so resuming such a record silently promotes it past the
	// ceiling every other control in lever's model assumes. Refusing is the only
	// honest answer: lever cannot repair the record either, since the hub
	// exposes no route to set a stored role.
	VerifyAgentRole func(ctx context.Context, project, agent string) error
	// Log surfaces a loud, user-facing progress/warning line during apply —
	// such as start-manager's `--fresh` discard notice, which MUST reach the
	// user rather than vanish into a swallowed return value. buildApplyDeps
	// wires this to the invoking cobra command's PrintErrf, mirroring how
	// other user-facing warnings already surface (see internal/cli/host/stop.go,
	// internal/cli/host/down.go).
	Log func(format string, args ...any)
	// BrokerStartRetry bounds the retry that absorbs scion's runtime-broker
	// registration race on manager start/resume/list; ManagerLiveRetry
	// bounds the post-start liveness poll. Zero values take the production
	// defaults (30 × 1 s and 15 × 1 s); tests shrink them.
	BrokerStartRetry RetryBudget
	ManagerLiveRetry RetryBudget
	// PhaseSettleRetry bounds the wait for a manager record observed in a
	// transitional phase (transitionalPhases) to settle into one converge
	// can act on. Zero takes the production default (60 × 1 s).
	PhaseSettleRetry RetryBudget
}

// check returns an error naming the first required collaborator left nil.
// ReadCred is the one optional field (nil ⇒ defaultReadCred).
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
		{"Scion", d.Scion == nil},
		{"StartBroker", d.StartBroker == nil},
		{"BrokerHealthy", d.BrokerHealthy == nil},
		{"EnsureControllerPAT", d.EnsureControllerPAT == nil},
		{"WaitBrokerReady", d.WaitBrokerReady == nil},
		{"MintManagerBootstrap", d.MintManagerBootstrap == nil},
		{"RearmBootstrap", d.RearmBootstrap == nil},
		{"EnsureHubLogin", d.EnsureHubLogin == nil},
		{"DisableHubLogin", d.DisableHubLogin == nil},
		{"EnsureScionTelemetry", d.EnsureScionTelemetry == nil},
		{"EnsureAgentTemplate", d.EnsureAgentTemplate == nil},
		{"StartRemoteProxy", d.StartRemoteProxy == nil},
		{"StopRemoteProxy", d.StopRemoteProxy == nil},
		{"RemoveJailFile", d.RemoveJailFile == nil},
		{"RemoveScionProjectConfigs", d.RemoveScionProjectConfigs == nil},
		{"ScionProjectRegistered", d.ScionProjectRegistered == nil},
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

// Run executes the bring-up Plan for app. load-image is host-side; the rest
// run in the jail via Deps.Scion. The jail itself is already up: the CLI
// brings it up before it can build Deps at all (buildApplyDeps needs the
// jail's run user for the JailRunner), so there is no jail-up step.
func Run(ctx context.Context, app *config.App, d Deps, opts PlanOpts) error {
	if err := d.check(); err != nil {
		return err
	}
	r := &run{app: app, d: d,
		brokerStart: d.BrokerStartRetry.or(defaultBrokerStartRetry),
		managerLive: d.ManagerLiveRetry.or(defaultManagerLiveRetry),
		phaseSettle: d.PhaseSettleRetry.or(defaultPhaseSettleRetry),
		fresh:       opts.Fresh,
	}
	// The plan is kept, not just ranged over: the converge-to-off reconciliation
	// below needs to know whether this run manages the hub at all (see
	// disableHubLogin).
	steps := Plan(app, opts)
	for _, step := range steps {
		if err := r.step(ctx, step); err != nil {
			return fmt.Errorf("step %s: %w", step.Kind, err)
		}
	}
	// Converge the remote proxy OFF when the config disables it. Plan omits
	// the remote-proxy step entirely in that case (see its RemoteEnabled
	// guard) so dry-run output never shows a start step it won't take — but
	// that also means the step loop above never reaches a proxy left running
	// from a PRIOR apply (remote.enabled since flipped back off). Reconcile
	// it here, unconditionally, so a config-off apply always converges to
	// "not running" rather than leaving a stale proxy serving traffic the
	// operator no longer intends to expose.
	if !app.RemoteEnabled() {
		if err := d.StopRemoteProxy(ctx); err != nil {
			return fmt.Errorf("remote-proxy: %w", err)
		}
		// The GUEST half has to converge too, and for a sharper reason than
		// the proxy does: the login forwarder is an unauthenticated TCP bridge
		// from guest loopback — reachable from every agent's netns — to a host
		// loopback port beside lever's own broker listeners. Left running
		// after the feature is off, it keeps that port bridged into the jail
		// for whatever binds it next. See Deps.DisableHubLogin.
		if err := r.disableHubLogin(ctx, steps); err != nil {
			return fmt.Errorf("hub login: %w", err)
		}
		// Two things this convergence deliberately does NOT undo, said here so
		// the calls above are not read as an exhaustive teardown.
		//
		// lever's overlay agent template (~/.scion/templates/lever) and the
		// project's default_template that selects it both stay. Neither is
		// remote-access state — the overlay exists to keep scion's placeholder
		// system prompt out of every agent it provisions (see
		// Deps.EnsureAgentTemplate), and that matters exactly as much with
		// remote access off.
		//
		// The SPA lever staged into the guest for the hub also stays
		// (layout.WebAssetsDir; EnsureScionWebAssets simply skips once
		// app.ScionWebAssets() is false), and that is deliberate on three
		// counts:
		//
		//   - Nothing lever starts reads it. With remote access off lever
		//     passes no --web-assets-dir at all (HubServerOpts), and the
		//     restart above takes the flag out of a running hub's argv. Note
		//     this is NOT the same as the hub having no UI: scion's
		//     workstation defaults keep the web frontend on either way (see
		//     scion.ServerOpts.EnableWeb).
		//   - It cannot go stale. stagedWebDigest compares digests, so turning
		//     remote access back on at the SAME pin reuses the staged tree and
		//     re-stages nothing, while a different pin re-stages.
		//   - What is left is small. The payload excludes vite's sourcemaps
		//     (guest.webAssetsExclude measures them at 8.2MB of 11.5MB), so
		//     this is single-digit MB of root-owned files inside the VM — not
		//     worth a delete path of its own, run on every apply of every
		//     remote-off instance, whose failure mode is 404ing a UI the hub
		//     is still serving.
	}
	return nil
}

// disableHubLogin converges the guest half of the login path off, and restarts
// the hub when that removed something the running hub was started FROM.
//
// The restart is the OFF path's half of scionServer's. Two pieces of the
// hub's remote-access state are fixed at startup and nowhere else: it reads
// `oidc_login` once, from the settings file (pkg/config/hub_config.go), and it
// takes --web-assets-dir from the argv it was started with. So a hub left
// running by an earlier apply goes on offering a login whose provider has
// gone, and goes on serving its SPA out of the directory lever staged for
// remote access — one lever stops maintaining the moment remote access is off
// — for as long as that process lives. The scion-server step cannot fix
// either: `scion server start` refuses on an already-running daemon and the
// client tolerates the refusal, which is what makes a re-apply cheap. Nothing
// short of a restart replaces the argv.
//
// Note what the restart does NOT do: it does not stop the hub serving a web
// UI. scion's workstation defaults enable the web frontend whenever the flag
// is not explicitly set, and lever depends on that — the Hub API is only on
// 8080 BECAUSE the frontend is on (see scion.ServerOpts.EnableWeb). What the
// restart takes away is the login, and --web-assets-dir with it; what the
// frontend then serves depends on the scion pin and is not this function's
// business.
//
// Gated on a real change for the reason the ON path is gated: a restart drops
// every agent's connection to the hub. An apply that finds the guest already
// converged must be silent, and DisableHubLogin reports false forever after the
// block is gone.
//
// Inferring the transition from guest state is a CHOICE, not the only option.
// scion does persist the daemon's launch argv: cmd/server_daemon.go calls
// daemon.SaveArgs, which writes <globalDir>/server-args.json, and
// --web-assets-dir is in it verbatim. Reading it would be authoritative and
// would generalise — a changed web port or assets dir is invisible to guest
// leftovers. lever does not, for two reasons: it needs a guest file read this
// path does not otherwise make, and the file is not a statement about what is
// RUNNING, since daemon.RemoveArgs has no production caller in the pinned
// module and the file therefore outlives the daemon that wrote it. Worth
// revisiting if this path ever needs the generality. (`scion server status` is
// not the alternative: its web field reports component HEALTH, 200 whether or
// not --enable-web was passed — cmd/server_daemon.go runServerStatus.)
//
// The price of that choice, stated because it is easy to assume otherwise: if
// the settings edit succeeds and the restart then FAILS, the next apply finds
// nothing left to remove, reports no change, and does not restart — so the hub
// goes on serving the removed oidc_login from memory. The repair is `lever
// stop` followed by `lever up`. The ON path has the identical residual.
//
// Skipped when this plan does not manage the hub — BrokerOnly, the VM
// acceptance gate, whose machine need not carry a scion binary at all. The
// guest still converges there; a hub left from an earlier full apply keeps its
// flags, and since the guest state that signals the change is now gone, it
// keeps them until a stop + up rather than until the next apply. That check is
// also what makes d.Scion safe to dereference below without a nil guard: the
// scion-server step ran earlier in this same Run and dereferenced it first.
func (r *run) disableHubLogin(ctx context.Context, steps []Step) error {
	changed, err := r.d.DisableHubLogin(ctx)
	if err != nil {
		return err
	}
	if !changed || !planHas(steps, KindScionServer) {
		return nil
	}
	r.d.Log("lever: remote access is off — restarting the hub so it stops offering the remote login")
	if err := r.d.Scion.ServerStop(ctx); err != nil {
		return fmt.Errorf("restart the hub: %w", err)
	}
	return r.d.Scion.ServerStart(ctx, r.hubServerOpts())
}

// planHas reports whether the plan includes a step of this kind.
func planHas(steps []Step, kind StepKind) bool {
	for _, s := range steps {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// step is the thin dispatch over StepKind: it routes each step to its
// executor. The non-trivial case bodies live in per-kind helpers below,
// each a method on run — no hidden state beyond it. The default
// arm is a hard error so a Plan emitting an unknown kind fails loudly.
func (r *run) step(ctx context.Context, s Step) error {
	switch s.Kind {
	case KindBrokerUp:
		return runBrokerUp(ctx, r.d)
	case KindLoadImage:
		return runLoadImage(ctx, s, r.d)
	case KindInitMachine:
		return r.d.Scion.InitMachine(ctx)
	case KindConfigRegistry:
		return r.d.Scion.ConfigSetGlobal(ctx, "image_registry", "scionlocal")
	case KindBootstrapToken:
		return r.d.EnsureControllerPAT(ctx)
	case KindScionServer:
		return r.scionServer(ctx)
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
		// Only reached when Plan included the step, i.e. r.app.RemoteEnabled().
		return r.d.StartRemoteProxy(ctx)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// scionServer runs the scion-server step: bring the guest's login path up
// to date, restart the hub if that changed its configuration, then start (or
// confirm) the hub.
//
// The restart is what makes the login path take effect at all: scion reads
// `oidc_login` once, at startup (pkg/config/hub_config.go), so a hub that was
// already running when lever wrote the block goes on serving a configuration
// with no login in it. It is conditional on an actual change, because a
// restart drops every agent's connection to the hub for the length of one —
// acceptable to turn remote access on, not acceptable on every apply.
// A failure anywhere after EnsureHubLogin leaves the guest forwarder
// installed and running with no hub behind it. That is deliberate, not an
// oversight: the forwarder bridges guest loopback to the login port on the
// HOST, where what binds is a lever provider inside some `lever remote serve`
// — so the half-applied state reaches either nothing (the connection is
// refused) or a provider, which hands out nothing without a code. (Two remote
// instances left on the default host port would have the forwarder reach the
// OTHER instance's provider; the consequence is a login that cannot complete,
// not an exposure — and config validation asks each instance for its own
// ports.) Tearing it down here would
// also tear down a forwarder a previous, successful apply had legitimately
// left running. The next apply converges it: EnsureHubLogin is idempotent, and
// with remote access turned off DisableHubLogin removes it outright.
func (r *run) scionServer(ctx context.Context) error {
	telemetryChanged, err := r.d.EnsureScionTelemetry(ctx)
	if err != nil {
		return fmt.Errorf("scion telemetry: %w", err)
	}
	if telemetryChanged {
		r.d.Log("lever: agent telemetry in the jail's scion settings is now %q (scion.telemetry) — agents started from here on use it; "+
			"a hub or manager that was already running keeps the old setting until `lever stop && lever up`", r.app.EffectiveScionTelemetry())
	}
	changed, err := r.d.EnsureHubLogin(ctx)
	if err != nil {
		return fmt.Errorf("hub login: %w", err)
	}
	if changed {
		r.d.Log("lever: the hub's login configuration changed — restarting the hub so it is read")
		// Stop-then-start, not `scion server restart`: scion's own restart
		// refuses outright when the daemon is not running, and the hub
		// being already down is an ordinary state here — after a `lever
		// stop`, a VM reboot, or a crash. ServerStop tolerates that (see
		// its doc); ServerStart below tolerates the opposite.
		if err := r.d.Scion.ServerStop(ctx); err != nil {
			return fmt.Errorf("hub login: restart the hub: %w", err)
		}
	}
	return r.d.Scion.ServerStart(ctx, r.hubServerOpts())
}

// hubServerOpts is HubServerOpts with the run's log as the start's progress
// sink, so a slow cold start prints that it is waiting.
func (r *run) hubServerOpts() scion.ServerOpts {
	opts := HubServerOpts(r.app, r.d.HubSessionSecret)
	opts.Progress = r.d.Log
	return opts
}

// HubServerOpts is the ONE description of how lever starts the hub for a given
// config. Every start shares it — this step's, the restart disableHubLogin
// orders when remote access has just been turned off, and the CLI's restart
// of the live hub after a failed bootstrap-token window — because the whole point
// of that restart is to replace an argv that no longer matches the config. Two
// copies of this could disagree, and the restart would then re-apply the very
// flags it exists to drop.
func HubServerOpts(app *config.App, sessionSecret string) scion.ServerOpts {
	opts := scion.ServerOpts{
		WebPort:       scion.DefaultHubPort,
		DevAuth:       false,
		EnableWeb:     app.RemoteEnabled(),
		SessionSecret: sessionSecret,
	}
	if app.ScionWebAssets() {
		// Same predicate the backend used to decide whether to stage the
		// assets, so the flag can never point at a directory nothing put
		// anything in — see ServerOpts.WebAssetsDir for why that case is
		// worse than passing no flag at all.
		opts.WebAssetsDir = layout.WebAssetsDir
	}
	return opts
}

// runBrokerUp runs the broker-up step: start the host broker (+ first-party
// tools), then health-check it before the manager starts.
func runBrokerUp(ctx context.Context, d Deps) error {
	if err := d.StartBroker(ctx); err != nil {
		return err
	}
	return d.BrokerHealthy(ctx)
}

// agentTemplate runs the agent-template step. Logs on change only: it is a
// provisioning-time change the operator cannot otherwise see, and silence on a
// no-op keeps a re-apply quiet.
func (r *run) agentTemplate(ctx context.Context, s Step) error {
	// The JAIL-side path, not the host one: the scion client behind this closure
	// runs inside the jail, where the host tree is visible only at the mount.
	changed, err := r.d.EnsureAgentTemplate(ctx, JailPath(s.Target, r.app.Tree, r.d.JailMount))
	if err != nil {
		return err
	}
	if changed {
		r.d.Log("lever: agents will no longer be launched with scion's placeholder system prompt (new agents only; existing ones keep the prompt they were provisioned with)")
	}
	return nil
}

// runLoadImage runs the load-image step: skip the multi-GB re-import when the
// jail already holds this exact image (same ID; fail-open — the guard returns
// false on any doubt), otherwise load and then best-effort prune the superseded
// dangling image. The source is the step's archive when it has one (image_tar),
// else the host docker store.
func runLoadImage(ctx context.Context, s Step, d Deps) error {
	loaded, load := d.ImageLoaded, d.LoadImage
	if s.TarPath != "" {
		loaded = func(ctx context.Context, ref string) bool { return d.ImageLoadedTar(ctx, ref, s.TarPath) }
		load = func(ctx context.Context, ref string) error {
			return d.LoadImageTar(ctx, ref, s.TarPath, d.ImageTagPolicy)
		}
	}
	if loaded(ctx, s.Target) {
		return nil
	}
	if err := load(ctx, s.Target); err != nil {
		return err
	}
	// After a load, prune dangling images: when this load superseded a tag
	// (a rebuilt image), the old copy is now untagged and would otherwise
	// ratchet the grow-only jail disk. A no-op when the load just added a
	// brand-new image. Best-effort — a prune failure must never fail the
	// bring-up.
	if err := d.PruneImages(ctx); err != nil {
		d.Log("load-image: pruning superseded jail images failed: %v", err)
	}
	return nil
}

// runCredential runs the credential step: read the manager credential file
// (defaultReadCred unless overridden) and set it as the scion secret.
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

// registerProject runs the register-project step: observe before doing
// anything destructive, then (only when the registration is unsound) clear the
// stale marker + project-config registration(s) and re-init + hub-link.
func (r *run) registerProject(ctx context.Context, s Step) error {
	jp := JailPath(s.Target, r.app.Tree, r.d.JailMount)

	// Idempotent register: observe BEFORE doing anything destructive. A
	// suspended manager (or worker) agent record survives a `lever stop` +
	// `lever up` cycle (its project linkage lives in this same
	// project-configs registration) — the marker-removal +
	// RemoveScionProjectConfigs + re-init below unconditionally tore that
	// linkage down on every apply, orphaning the record and breaking
	// `scion resume`. When the registration is already sound (exactly one
	// project-configs entry for jp AND the in-tree marker present), there
	// is nothing to fix, so skip the whole destructive path. A query
	// error, or an unsound registration (zero, duplicate, or torn), falls
	// through unchanged to the existing destructive path below — fail
	// open, never a hard apply failure over an observe read.
	if ok, err := r.d.ScionProjectRegistered(ctx, jp); err == nil && ok {
		// Sound registration, so nothing below runs — but the recorded hub
		// ENDPOINT can still be stale. Minting the controller PAT links the
		// project against a throwaway hub on its own port, and that link
		// survives precisely because this path skips the re-init. Repair it
		// here, where the skip happens.
		if err := r.d.RepairScionHubEndpoint(ctx, jp); err != nil {
			return err
		}
		return r.settleProject(ctx, path.Base(jp))
	}

	// Remove a stale `.scion` marker FILE left in the tree by a previous
	// bring-up. It survives `orb delete` (it lives in the bind-mounted tree),
	// and `scion init` writes workspace_path only on fresh-create — resolving
	// a stale marker skips it, so the agent mounts an empty managed config-dir
	// copy instead of the live tree (the in-place mount silently breaks).
	// Removing it forces a fresh, correct init.
	//
	// The tree is a VirtioFS bind mount: the host and the jail do not share
	// one filesystem view/cache, so a host-side unlink is not promptly
	// visible to the guest. Live-reproduced: removing the marker on the HOST
	// then immediately running `scion init` IN the jail failed with
	// "failed to initialize project: existing project marker is invalid:
	// open /lever/.scion: no such file or directory" — scion's guest-side
	// directory scan still saw the just-deleted marker, then the open()
	// raced the host unlink and lost. Running the identical `scion init`
	// manually in the jail moments later succeeded (same view, no race).
	// So the removal must go THROUGH the jail's own filesystem view — the
	// same view the subsequent in-jail init uses — which is what
	// r.d.RemoveJailFile does.
	if err := r.d.RemoveJailFile(ctx, path.Join(jp, layout.ProjectMarker)); err != nil {
		return err
	}
	// Clear any stale project-config registration(s) for this workspace path
	// before re-init, so `scion init` mints exactly ONE registration per
	// workspace instead of leaving the previous apply's dir behind.
	if err := r.d.RemoveScionProjectConfigs(ctx, jp); err != nil {
		return err
	}
	if err := r.d.Scion.InitProject(ctx, jp); err != nil {
		return err
	}
	if err := r.d.Scion.HubLink(ctx, jp); err != nil {
		return err
	}
	return r.settleProject(ctx, path.Base(jp))
}

// settleProject is the hub-side project state both register paths converge
// on: no cross-agent shared dir, and a role ceiling. Both are what the hub
// records for the project, so both must hold whether the registration was
// just made or found sound.
func (r *run) settleProject(ctx context.Context, projectName string) error {
	if err := r.d.StripProjectSharedDirs(ctx, projectName); err != nil {
		return err
	}
	return r.d.EnsureAgentRoleCeiling(ctx, projectName)
}

// mintManagerBootstrap runs the mint-manager-bootstrap step: mint the
// manager's one-time enrol ticket and stage it (0600) for lever-agent to read.
// Idempotent against the LIVE broker latch (not a stale file): a spent latch is
// tolerated only when a ticket is already staged.
func (r *run) mintManagerBootstrap(ctx context.Context, s Step) error {
	// Idempotent (tied to the LIVE broker latch, not a stale file): mint; if the
	// latch is already consumed (same broker process as a prior apply), tolerate
	// it — the manager has its bootstrap.json from then. After a broker restart
	// the latch reopens, mint succeeds, and a fresh ticket is deposited, so a
	// partially-failed first apply (bootstrap written but manager never enrolled)
	// recovers on re-apply. (r.minted is not read after this step.)
	m, err := r.d.MintManagerBootstrap(ctx)
	if err != nil {
		if errors.Is(err, ErrBootstrapLatched) {
			// A spent latch is only tolerable when a bootstrap ticket is already
			// staged (true idempotent re-apply against the same broker). If none
			// is staged, a stale broker from a prior run is being reused and the
			// new manager could never enrol — fail loudly instead of booting a
			// doomed manager.
			staged := filepath.Join(s.Target, ".lever", "bootstrap.json")
			if _, statErr := os.Stat(staged); statErr == nil {
				return nil
			}
			return fmt.Errorf("broker /bootstrap latch already consumed but no bootstrap ticket is staged at %s; a stale broker is likely still running — run `lever down` then retry", staged)
		}
		return err
	}
	r.minted = true
	// Deposit it as a 0600 file in the mount (the lever-agent reads it).
	return StageBootstrapMaterial(s.Target, m)
}

// startManager runs the start-manager plan step: observe the manager record,
// then act on the delta (create / no-op / resume / forced resume, or a
// refusal on a failed resume or a phase lever cannot act on) and verify the
// container is actually live. Only --fresh deletes a record
// (recoverDeleteAndCreate).
func (r *run) startManager(ctx context.Context, s Step) error {
	jp := JailPath(r.app.Tree, r.app.Tree, r.d.JailMount)
	// Read the prompt before any waiting: a missing or unreadable prompt file
	// is a config error and should fail fast, not after the broker-ready poll.
	task, err := r.managerTask()
	if err != nil {
		return err
	}
	instructions, err := r.managerInstructions()
	if err != nil {
		return err
	}
	// The read_only paths must be real directories reached through no
	// symlink BEFORE anything is acted on: under --fresh the delete runs
	// ahead of the create, and refusing only at the create would discard
	// the old session and start no new one. Checked on every path, resume
	// included — a failure there means the tree changed under a manager
	// that may hold it writable, which the operator should hear about.
	// startManagerCreate checks again right before the create: under
	// --fresh the old manager (which may hold the paths writable) is still
	// running here, and is gone only after the delete.
	if err := r.app.PrepareManagerReadOnlyHost(); err != nil {
		return fmt.Errorf("start-manager: %w", err)
	}
	// Gate on runtime-broker readiness before any create/resume: the workstation
	// daemon registers its runtime broker asynchronously AFTER its Hub API comes
	// up (waitHubReady only proved the latter), so acting now would race it. This
	// is the proactive complement to the broker-unavailable retry below — wait
	// for a ready broker rather than only reacting when a call fails against a
	// not-yet-ready one. Fail-soft (never errors on timeout); the retry backstops.
	if err := r.d.WaitBrokerReady(ctx, jp); err != nil {
		return fmt.Errorf("start-manager: waiting for runtime broker: %w", err)
	}
	opts, err := r.managerStartOpts(ctx, jp, task, instructions)
	if err != nil {
		return err
	}
	rec, err := r.observeManager(ctx, jp)
	if err != nil {
		return err
	}
	if !r.fresh {
		// Before converge: a stopped record whose claude still runs must be
		// reported running again, or the resume below restarts it FRESH.
		// The stale-mount check then reads the healed record, so a record
		// the heal turned running is kept (managerKept) and not refused.
		rec = HealAgentSession(ctx, r.d.sessionHealer(), jp, rec)
	}
	if rec != nil && !r.fresh {
		if err := r.checkStaleTreeMounts(ctx, jp, rec); err != nil {
			return err
		}
	}
	acted, err := r.convergeManager(ctx, jp, rec, opts)
	if err != nil {
		return err
	}
	if err := r.waitManagerLive(ctx, jp, acted); err != nil {
		return err
	}
	healNamedSessions(ctx, r.d, jp, workerNames(r.app))
	if !r.managerCreated {
		r.warnManagerTreeMounts(ctx, jp)
	}
	r.warnManagerNestedVirt(ctx, jp)
	return nil
}

// managerTask reads the manager's task prompt (when configured).
func (r *run) managerTask() (string, error) {
	p := r.app.ManagerPromptPath()
	if p == "" {
		return "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("reading manager prompt %s: %w", p, err)
	}
	task := strings.TrimSpace(string(b))
	// The task rides scion's argv into a tmux command capped at 16 KiB
	// (lever#30). Fail here, naming the file, rather than after the
	// broker-ready wait with a container that exits "command too long".
	if err := scion.CheckTask(task); err != nil {
		return "", fmt.Errorf("manager prompt %s: %w", p, err)
	}
	return task, nil
}

// managerInstructions reads the manager's standing instructions (when
// configured). Read up front like the prompt — a missing file is a config
// error — and passed as CONTENT, so the file only has to be readable on the
// host: the scion binary runs in the jail and never sees the path.
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

// managerStartOpts builds the `scion start` options for the manager: the task
// prompt (already read by managerTask) and, in api-key mode, the project env +
// placeholder secret the container needs before it boots.
//
// LEVER_BOOTSTRAP reconciliation: we do NOT set LEVER_BOOTSTRAP here.
// lever-agent boot's canonical-path default (./.lever/bootstrap.json relative
// to CWD) suffices: scion sets --workspace = jp (the in-jail project tree),
// and the container's CWD is /workspace, so ./.lever/bootstrap.json resolves
// to jp/.lever/bootstrap.json — exactly where mint-manager-bootstrap wrote the
// manager's bootstrap.json. Injecting an env var would be redundant and add a
// scion StartOpts.Env dependency that the file convention avoids.
func (r *run) managerStartOpts(ctx context.Context, jp, task, instructions string) (scion.StartOpts, error) {
	apiKey := r.app.EffectiveManagerLLMAuth() == config.LLMAuthAPIKey
	if apiKey {
		if err := r.prepareAPIKeyMode(ctx, jp); err != nil {
			return scion.StartOpts{}, err
		}
	}
	return scion.StartOpts{
		Worker: r.app.Name, Task: task, Project: jp, Image: r.app.ManagerImage(), Harness: "claude",
		// Empty when the config names no model, which omits --model and leaves
		// scion's default resolution alone. Only ever read on a fresh create:
		// the resume paths below take no model (scion resume has no such flag).
		Model: r.app.ManagerModel(),
		// Standing instructions, delivered on scion's stdin as inline config —
		// never argv (see scion.StartOpts.Instructions). Empty when the config
		// names no file, which sends no config at all. Create-time only, like
		// Model: a resume re-projects what scion staged at the fresh create.
		Instructions: instructions,
		// manager.read_only: the protected tree paths and their ancestor
		// pins, as inline-config volumes, plus /dev/kvm under nested_virt.
		// Nil when neither is configured. Create-time only, like
		// Instructions: a resume redispatches the volumes scion stored with
		// the record.
		Volumes: managerVolumes(jp, r.app),
		// Workspace = the in-jail project tree, so the manager edits the real
		// host files in place (verified 2026-06-16). Without it scion mounts a
		// managed copy of the externalized config dir, not the live tree.
		Workspace: jp,
		// api-key: start with --harness-auth api-key (satisfied by the placeholder
		// secret set above); the real credential arrives in-container.
		APIKey: apiKey,
	}, nil
}

// KVMDevice is the device nested_virt gives the manager container.
const KVMDevice = "/dev/kvm"

// managerVolumes is the manager's inline-config volumes: its read_only plan
// (managerTreeVolumes) and, under nested_virt, KVMDevice bind-mounted to
// itself. Scion has no per-agent device field; rootless podman passes a
// --device as this same bind mount (it cannot mknod, and sets no device
// cgroup), so the manager gets a working /dev/kvm while the hub and the
// workers get none.
func managerVolumes(jp string, app *config.App) []scion.VolumeMount {
	out := managerTreeVolumes(jp, app.ManagerTreeMounts())
	if app.NestedVirt {
		out = append(out, scion.VolumeMount{Source: KVMDevice, Target: KVMDevice})
	}
	return out
}

// managerTreeVolumes turns the manager's read_only plan into scion volumes:
// each tree directory bind-mounted over its own place in the workspace —
// source under jp (the in-jail tree, which scion mounts at /workspace),
// target under scion.ContainerWorkspace — read-only for an entry and
// read-write for an ancestor pin. The plan's order (by depth, then path) is
// kept; podman orders mounts by destination depth anyway.
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
// manager.read_only plan, each list by tree-relative path. The kinds differ
// in what they let the agent do, so String words each with its consequence:
//
//   - Missing: an entry with no mount — the agent writes it;
//   - Writable: an entry mounted read-write — the agent writes it;
//   - WrongSource: a mount whose source is not that directory of the
//     in-jail tree — it covers something else;
//   - Replaced: an entry the container can write although its read-only
//     mount is listed — the host replaced the directory after the manager
//     was created, and the mount still covers the old one (live probe);
//   - MissingPins: an entry's ancestor with no mount — the agent can
//     rename it away and recreate the protected path writable;
//   - MissingWorkerPins: a worker dir (or its ancestor) with no mount —
//     the manager can replace it with a link and have that worker mount a
//     protected directory read-write.
type TreeMountGaps struct {
	Missing, Writable, WrongSource, Replaced, MissingPins, MissingWorkerPins []string
}

// Empty reports whether the container matches the plan.
func (g TreeMountGaps) Empty() bool {
	return len(g.Missing)+len(g.Writable)+len(g.WrongSource)+len(g.Replaced)+len(g.MissingPins)+len(g.MissingWorkerPins) == 0
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
	add(g.Replaced, "writable from the container although its read-only mount is listed: the directory was replaced on the host after the manager was created, and the mount still covers the old one")
	add(g.MissingPins, "not pinned: the agent can rename it away and recreate a protected path writable")
	add(g.MissingWorkerPins, "(a worker dir) not pinned: the manager can replace it with a symbolic link and have that worker mount a protected directory read-write")
	return strings.Join(parts, "; ")
}

// Stale reports whether ANY gap is a replaced directory (others may sit
// beside it). A fresh create fixes it like the rest, but the operator
// caused it by replacing a protected directory instead of editing it in
// place, so the fix text adds that rule.
func (g TreeMountGaps) Stale() bool { return len(g.Replaced) > 0 }

// ManagerTreeMountGaps compares a manager container's mounts with the
// read_only plan: every planned directory must be mounted at its place in
// the workspace from the same place in the in-jail tree jp, and an entry
// must be read-only. A pin's writability is not checked — what makes it a
// pin is being a mount point, which cannot be renamed or removed. Shared by
// `lever doctor` and apply's keep/resume warning. The live write probe
// (ProbeReplacedEntries) is separate: it needs a running container.
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

// WritableProbe asks a running container whether its user can write an
// in-container path (jail.ContainerPathWritable in production).
type WritableProbe func(ctx context.Context, ref, target string) (bool, error)

// ProbeReplacedEntries runs the live half of the read_only check on a
// RUNNING manager container: every entry must refuse a write. A listed
// read-only mount does not prove it — after the host replaced the
// directory (rename-based deploy, rm -rf and recreate) the mount stays on
// the old directory and a write lands in the new one. Returns the entries
// the container can write; any probe that cannot run is an error, so a
// failed probe never reads as protected.
func ProbeReplacedEntries(ctx context.Context, probe WritableProbe, ref string, want []config.TreeMount) ([]string, error) {
	var replaced []string
	for _, w := range want {
		if !w.ReadOnly {
			continue
		}
		writable, err := probe(ctx, ref, path.Join(scion.ContainerWorkspace, w.Rel))
		if err != nil {
			return nil, err
		}
		if writable {
			replaced = append(replaced, w.Rel)
		}
	}
	return replaced, nil
}

// ManagerTreeMountsFix is the operator's way out of every gap: a fresh
// create (which discards the conversation, so back it up first), and for a
// replaced directory, editing protected directories in place from now on.
func ManagerTreeMountsFix(g TreeMountGaps) string {
	fix := "back up the manager's conversation first, then run `lever up --fresh` to recreate the manager with the mounts (the fresh start deletes the manager record and its conversation)"
	if g.Stale() {
		fix += "; from then on edit protected directories in place — replacing one on the host (rm -rf and recreate, a rename-based deploy, a git checkout that removes and re-adds it) drops the protection until the next fresh create"
	}
	return fix
}

// StaleTreeMounts is how a manager container's tree mounts outlive the
// config. scion keeps a record's volumes for life and a resume recreates
// the container from them, so a mount lever made at the create stays after
// the config stops asking for it, each list by tree-relative path:
//
//   - Gone: the host directory behind the mount no longer exists, or a
//     symbolic link now stands in its path (lever does not follow it on the
//     host; see treePathLinked). podman cannot recreate the container
//     ("statfs …: no such file or directory"), or would mount whatever the
//     link points at, so the resume is refused (card #157);
//   - DroppedReadOnly: an entry no longer in manager.read_only, still
//     mounted read-only — the manager cannot write it until a fresh create;
//   - DroppedPins: a pin the plan no longer has (a removed entry's
//     ancestor, a removed worker's dir), still mounted over itself — the
//     directory cannot be renamed or removed from inside the container.
//
// The reverse case, an entry ADDED to the config, is a TreeMountGaps
// Missing gap: it too needs a fresh create.
type StaleTreeMounts struct {
	Gone, DroppedReadOnly, DroppedPins []string
}

// Empty reports whether the container mounts nothing the config dropped.
func (s StaleTreeMounts) Empty() bool {
	return len(s.Gone)+len(s.DroppedReadOnly)+len(s.DroppedPins) == 0
}

// maxStalePaths and maxStalePathBytes bound what String lists: the paths
// come from the container's inspect or the hub record, text lever does not
// control.
const (
	maxStalePaths     = 10
	maxStalePathBytes = 120
)

// String words the stale mounts for a doctor row or an apply message. Each
// path is quoted (%q escapes every control character) after a cut to
// maxStalePathBytes, and each list stops at maxStalePaths with a count of
// the rest.
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

// Fix is the operator's way out: for a gone directory, recreate it (empty
// is enough) or start fresh; for a dropped mount, keep the directory until
// a fresh create, which is the only way to drop the mount.
func (s StaleTreeMounts) Fix() string {
	const fresh = "back up the manager's conversation first, then run `lever up --fresh` (the fresh start deletes the manager record and its conversation, and creates the manager with the mounts the config names now)"
	if len(s.Gone) > 0 {
		return "recreate each missing directory on the host as a real directory (an empty one is enough; replace a symbolic link with one) and run the command again; or, to drop the mounts, " + fresh
	}
	return "keep these directories until the next fresh create (a missing one blocks the resume); to drop the mounts now, " + fresh
}

// ManagerStaleTreeMounts finds, among a manager container's mounts, the
// tree directories lever mounted over themselves (source the directory in
// the in-jail tree jp, target the same place under the workspace) that the
// current read_only plan no longer has, or whose host directory (under
// tree) is gone. Any other mount — the workspace itself, scion's own — is
// not lever's and is skipped.
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

// treePathLinked walks rel under tree with Lstat, one component at a time,
// and reports whether any component is a symbolic link. It never follows a
// link on the host: the tree is agent-writable, and a link could point at a
// path whose stat blocks (a macOS autofs path such as /net/<host>/x) or at
// anything outside the tree, which a resume would then mount into the
// manager. A link anywhere on the path therefore counts as gone (a dangling
// one included); the error is the first Lstat's (os.ErrNotExist for a
// missing component).
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
// place in the workspace, the shape managerTreeVolumes creates, and its
// tree-relative path.
func treeSelfMount(jp string, m jail.Mount) (string, bool) {
	rel, ok := strings.CutPrefix(m.Destination, scion.ContainerWorkspace+"/")
	if !ok || rel == "" || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	return rel, m.Source == path.Join(jp, rel)
}

// managerKept reports whether convergeManager keeps rec as it is (no
// resume, no restart, so its container is not recreated). Every other arm
// that acts on a record resumes it — plain, or forced for the error phase —
// and a resume recreates the container from the record's volumes. One
// predicate for both, so checkStaleTreeMounts cannot drift from what
// convergeManager does.
func managerKept(rec *scion.Agent) bool { return rec.Phase == scion.PhaseRunning }

// checkStaleTreeMounts runs before apply acts on a manager record it will
// keep or resume: a mount the record holds for a directory that is gone
// fails the resume inside podman with a bare statfs error, and a mount the
// config dropped stays until a fresh create.
//
// It reads the mounts off the manager's container, which a stopped record
// keeps (scion's stop does not remove it). With no container to inspect,
// or an inspect that fails, it falls back to the record's own volumes on
// the hub (Deps.RecordVolumes: the inline-config volumes a resume recreates
// the container from); with neither it says nothing, and doctor and
// warnManagerTreeMounts report what they cannot inspect.
//
// A gone directory refuses the bring-up before anything is acted on,
// naming the fix, unless apply will keep the record as it is
// (managerKept): every other path resumes it — the forced resume of an
// error phase too, even while its container is still up — and the resume
// recreates the container. Everything else is a warning.
func (r *run) checkStaleTreeMounts(ctx context.Context, jp string, rec *scion.Agent) error {
	got, ok := r.managerMounts(ctx, jp)
	if !ok {
		return nil
	}
	stale := ManagerStaleTreeMounts(jp, r.app.Tree, r.app.ManagerTreeMounts(), got)
	if stale.Empty() {
		return nil
	}
	if len(stale.Gone) > 0 && !managerKept(rec) {
		return fmt.Errorf("start-manager: manager %q cannot be resumed: %s. To fix it, %s", r.app.Name, stale, stale.Fix())
	}
	r.d.Log("start-manager: WARNING: manager %q: %s. %s", r.app.Name, stale, stale.Fix())
	return nil
}

// managerMounts is the manager's mounts for checkStaleTreeMounts: its
// container's, or else the record's volumes. ok is false when neither can
// be read.
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

// warnManagerTreeMounts runs after apply kept or resumed a manager rather
// than creating it: manager.read_only is create-time only, so a manager
// created before the setting (or before an entry was added) does not hold
// the mounts, and nothing in a resume adds them; and a protected directory
// replaced on the host since the create is no longer covered. Never fatal —
// the manager works, it is only unprotected — but loud, naming what is
// wrong and the way to fix it. The container is found by scion's container
// name, which a resume keeps (its id changes when scion recreates the
// container); it is live here (waitManagerLive passed), so the write probe
// runs too.
func (r *run) warnManagerTreeMounts(ctx context.Context, jp string) {
	want := r.app.ManagerTreeMounts()
	if len(want) == 0 {
		return
	}
	if r.d.InspectContainerMounts == nil || r.d.ProbeContainerWritable == nil {
		r.d.Log("start-manager: WARNING: manager.read_only is create-time only and lever could not inspect manager %q's mounts; run `lever doctor` (row \"manager read-only paths\") to see whether it holds them", r.app.Name)
		return
	}
	ref := jail.ContainerName(path.Base(jp), r.app.Name)
	got, err := r.d.InspectContainerMounts(ctx, ref)
	if err != nil {
		r.d.Log("start-manager: WARNING: manager.read_only is create-time only and lever could not inspect manager %q's mounts (%v); run `lever doctor` to check", r.app.Name, err)
		return
	}
	gaps := ManagerTreeMountGaps(jp, want, got)
	if gaps.Empty() {
		replaced, err := ProbeReplacedEntries(ctx, WritableProbe(r.d.ProbeContainerWritable), ref, want)
		if err != nil {
			r.d.Log("start-manager: WARNING: lever could not probe whether manager %q can write its read-only paths (%v); run `lever doctor` to check", r.app.Name, err)
			return
		}
		gaps.Replaced = replaced
	}
	if !gaps.Empty() {
		r.d.Log("start-manager: WARNING: manager %q does not hold its manager.read_only protection: %s. To protect them, %s", r.app.Name, gaps, ManagerTreeMountsFix(gaps))
		if tools := r.toolsOnReadOnly(); len(tools) > 0 {
			r.d.Log("start-manager: the broker does not start tool(s) %s until then: each runs a program from a protected directory, which this manager could have rewritten", strings.Join(tools, ", "))
		}
	}
}

// toolsOnReadOnly names the broker tools whose program relies on
// manager.read_only (config.App.ToolsOnReadOnly), sorted; the broker holds
// them back while the manager lacks the mounts (brokerctl.ReadOnlyGuard).
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

// warnManagerNestedVirt runs after the manager is live: nested_virt's
// /dev/kvm volume is create-time only, so a manager created before the key
// was turned on lacks the device, and one created while it was on keeps the
// device after it is turned off (a resume recreates the container from the
// record's volumes). Both need a fresh create. Off, it probes Lima instances
// only: nested_virt is Lima-only, so no other manager can hold the device.
// Never fatal.
func (r *run) warnManagerNestedVirt(ctx context.Context, jp string) {
	on := r.app.NestedVirt
	if !on && r.app.Backend != config.BackendLima {
		return
	}
	if r.d.ProbeContainerDevice == nil {
		if on {
			r.d.Log("start-manager: WARNING: nested_virt is on and lever could not probe manager %q for /dev/kvm; run `lever doctor` (row \"nested virt\")", r.app.Name)
		}
		return
	}
	has, err := r.d.ProbeContainerDevice(ctx, jail.ContainerName(path.Base(jp), r.app.Name), KVMDevice)
	if err != nil {
		if on {
			r.d.Log("start-manager: WARNING: nested_virt is on and lever could not probe manager %q for /dev/kvm (%v); run `lever doctor`", r.app.Name, err)
		}
		return
	}
	switch {
	case on && !has:
		r.d.Log("start-manager: WARNING: nested_virt is on but manager %q has no /dev/kvm; %s", r.app.Name, NestedVirtRecreateFix)
	case !on && has:
		r.d.Log("start-manager: WARNING: nested_virt is off but the manager %q still has /dev/kvm; %s", r.app.Name, NestedVirtRecreateFix)
	}
}

// NestedVirtRecreateFix is how a manager gains or drops /dev/kvm: a fresh
// create, which discards the conversation (a stop and up keeps the record's
// volumes).
const NestedVirtRecreateFix = "recreate it to apply the change: back up the conversation first, then `lever up --fresh` (discards the conversation; `lever stop` + `lever up` keeps the old devices)"

// convergeManager acts on the observed manager record (nil when absent, already
// settled out of any transitional phase by observeManager): create, keep,
// resume, forced resume, the `--fresh` delete+create, or — for a phase it
// does not know — a refusal that leaves the record alone. acted reports
// whether it started or resumed anything — what decides if the liveness gate
// that follows must hold for the settle window (lever#31) or may take one look.
func (r *run) convergeManager(ctx context.Context, jp string, rec *scion.Agent, opts scion.StartOpts) (acted bool, err error) {
	switch {
	case rec == nil:
		// Under --fresh an "already exists" answer means the observe step
		// missed a live record (scion's lazy hub-sync can): that record is
		// the session the operator asked to discard, never a success.
		return true, r.startManagerCreate(ctx, opts, r.fresh)
	case r.fresh:
		// `up --fresh`: the operator asked to discard the session, whatever
		// phase the record is in. Decided HERE, after the hub is up, because
		// after `lever stop` the record is invisible until this step
		// (lever#33). Loud, like every other discard; the create that follows
		// is what puts a changed manager.image into effect (a resume keeps
		// the image the record was created with).
		return true, r.recoverDeleteAndCreate(ctx, jp, opts,
			fmt.Sprintf("start-manager: --fresh — deleting manager %q (phase %s) and starting FRESH (previous session discarded)", r.app.Name, scion.BoundedQuote(rec.Phase)),
			"--fresh delete")
	case managerKept(rec):
		// No-op — the liveness verify in startManager still confirms the
		// container is actually up: a running RECORD with a dead container
		// must fail loudly, not silently pass.
		return false, nil
	case rec.Phase == scion.PhaseSuspended || rec.Phase == scion.PhaseStopped:
		if rec.Phase == scion.PhaseStopped {
			// scion's hub resumes a stopped record with a fresh harness
			// session (it passes --continue for a suspended one only), so
			// say so: the old conversation is still in the agent home.
			r.d.Log("start-manager: manager %q is in phase stopped; scion resumes a stopped record in a new claude session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)", r.app.Name)
		}
		// Resume rides the SAME runtime-broker-race retry as a create Start
		// (see scion.IsBrokerUnavailable's doc): on a cold VM the runtime broker may
		// not have re-registered with the hub yet, and resume hits that
		// identical transient window. Only once the retry budget is exhausted
		// (or the error is not the transient one at all) is the session
		// declared unrecoverable.
		return true, r.resumeOrRecover(ctx, jp, resumeVerbPlain(func() error {
			return r.d.Scion.Resume(ctx, r.app.Name, jp)
		}))
	case rec.Phase == scion.PhaseError:
		// A crashed/wedged manager record. Since scion#895 (`resume
		// --force`, pin >= 68507153) the error phase IS recoverable — try
		// that first, with a fresh ticket staged (the leaf may have lapsed
		// while wedged; same rationale as the suspended branch). A forced
		// resume that fails keeps the record, like any failed resume.
		// Live motivation: 2026-07-31, an OrbStack VM reboot corrupted the
		// container state, resume failed, and the then-unconditional
		// delete+fresh destroyed the manager conversation (#3).
		return true, r.resumeOrRecover(ctx, jp, resumeVerbForce(func() error {
			return r.d.Scion.ResumeForce(ctx, r.app.Name, jp)
		}))
	default:
		// A transitional phase was settled (or refused) by observeManager
		// before this switch, so what lands here is a phase this lever does
		// not know at all — a newer scion enum, or a hostile hub string. There
		// is no verb to act on it, and guessing used to mean the loud
		// delete+fresh path: a plain `lever up` discarding the conversation on
		// a string it could not read (P6). Refuse instead, leaving the record
		// (and its conversation) in place; the operator's discard is `up
		// --fresh` (the arm above), which needs no phase at all.
		return false, fmt.Errorf("start-manager: manager %q is in phase %s, which lever does not recognise; nothing was changed. Retry once it settles, or run `lever up --fresh` to discard the session and start over",
			r.app.Name, scion.BoundedQuote(rec.Phase))
	}
}

// settleManagerPhase re-observes a manager record seen in a transitional
// phase (transitionalPhases) until scion moves it to a phase convergeManager
// can act on, or the record disappears (nil: a concurrent delete — the caller
// then creates), or the settle budget runs out. Exhaustion is a hard error
// naming the phase and the two ways out; it never touches the record. A
// list error mid-poll is treated as "not yet" (the next attempt re-reads),
// like waitManagerLive; a phase that is neither transitional nor known stops
// the wait at once so convergeManager's default arm refuses it by name.
func (r *run) settleManagerPhase(ctx context.Context, jp string, rec *scion.Agent) (*scion.Agent, error) {
	b := r.phaseSettle
	r.d.Log("start-manager: manager %q is in phase %q (mid-transition) — waiting up to %s for scion to settle it",
		r.app.Name, rec.Phase, time.Duration(b.Attempts)*b.Interval)
	cur := rec
	var listErr error
	err := retry.Until(ctx, b.Attempts, b.Interval, func() (bool, error) {
		agents, err := r.d.Scion.List(ctx, jp)
		if err != nil {
			listErr = err
			return false, nil
		}
		listErr = nil
		cur = scion.FindAgent(agents, r.app.Name)
		return cur == nil || !slices.Contains(transitionalPhases, cur.Phase), nil
	})
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, retry.ErrExhausted) {
		return nil, err
	}
	if listErr != nil {
		// The last look failed, so cur is stale: say so rather than report
		// a phase nobody observed on the final attempt.
		return nil, fmt.Errorf("start-manager: manager %q was in phase %q and could not be re-observed within %s (last error: %v); nothing was changed. Retry, or run `lever up --fresh` to discard the session and start over",
			r.app.Name, cur.Phase, time.Duration(b.Attempts)*b.Interval, listErr)
	}
	return nil, fmt.Errorf("start-manager: manager %q stayed in phase %q for %s and did not settle; nothing was changed. Retry once it settles (is a `lever stop` still running?), or run `lever up --fresh` to discard the session and start over",
		r.app.Name, cur.Phase, time.Duration(b.Attempts)*b.Interval)
}

// prepareAPIKeyMode conveys LEVER_LLM_AUTH=api-key to the manager container so
// its pre-start hook enters api-key mode (the hook reads $LEVER_LLM_AUTH; scion
// projects Hub env before pre-start hooks run). Project-scoped (the manager's
// project = jp) so it never leaks to other agents. Runs BEFORE start so it is
// present when the container boots.
//
// It also satisfies scion's start-time auth gate with a placeholder
// ANTHROPIC_API_KEY (Hub secret, projected to every container — fine since the
// instance is uniformly api-key). It is a sentinel, NOT a real credential: the
// agent's real LLM credential is the in-container broker capability token, and
// the broker /llm overwrites this placeholder x-api-key with the real key.
// Without it scion's env-gather/auth-resolution refuses to launch the container
// (and thus lever-agent boot, which writes the real token). Set once here;
// later-started workers inherit the same Hub secret.
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
// (nil when absent), after refusing to KEEP a record whose stored role the
// installed scion would read as `full` (see Deps.VerifyAgentRole).
//
// Observe, then act on the delta — scion's verbs are state-specific: start
// CREATES (409 "already exists" over a stopped record; the 409 error TEXT
// matches AlreadyRunning, so a blind start false-succeeds through that
// idempotency check — scion's own exit code is correctly non-zero, verified
// upstream 2026-07-04); resume covers suspended AND stopped records,
// relaunching with `claude --continue` (conversation restored). Live evidence
// 2026-07-04.
//
// The Hub API is up by this point in Plan() (scion-server ran first, and
// waitHubReady confirmed it), but the runtime broker registers asynchronously
// after it, so this FIRST call into the hub can still hit the registration
// window — on a cold VM as a "deadline exceeded" from the hub. So the observe
// rides the SAME bounded retry as the Start/Resume (scion.IsBrokerUnavailable): a
// transient broker-not-ready blip is retried, and only a persistent or
// genuinely-different error is fatal.
//
// A record in a transitional phase is first waited on (settleManagerPhase),
// so the role gate and the caller both see a settled one.
//
// The role gate only covers the phases convergeManager keeps the record in:
// rec == nil creates one, and its default branch refuses anything else. It
// returns rather than falling into recoverDeleteAndCreate on purpose. That
// recovery discards the conversation, and refusing here exists to give the
// operator the choice — losing the session is one of the two ways out, not
// something a guard may take on their behalf.
func (r *run) observeManager(ctx context.Context, jp string) (*scion.Agent, error) {
	agents, err := r.listAgentsRetry(ctx, jp)
	if err != nil {
		return nil, fmt.Errorf("start-manager: observing agents: %w", err)
	}
	rec := scion.FindAgent(agents, r.app.Name)
	// Under --fresh the record is about to be discarded whatever its phase or
	// role (convergeManager): waiting for it to settle would be pointless,
	// and the guard's own remedy for a pre-role record IS "delete the agent
	// so lever recreates it" — so refusing here would block the one verb
	// that applies that remedy (review finding on lever#33).
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

// resumeVerb is one of the two resumable arms of convergeManager: a
// suspended/stopped record via `scion resume`, an error record via `scion
// resume --force`.
type resumeVerb struct {
	label  string // verb name in the log line and the refusal
	resume func() error
}

// resumeVerbPlain is the `scion resume` arm for a suspended/stopped record.
func resumeVerbPlain(run func() error) resumeVerb {
	return resumeVerb{label: "resume", resume: run}
}

// resumeVerbForce is the `scion resume --force` arm for an error record.
func resumeVerbForce(run func() error) resumeVerb {
	return resumeVerb{label: "resume --force", resume: run}
}

// resumeOrRecover is the shared body of the two resumable arms. It stages
// fresh bootstrap material and runs the verb through the runtime-broker-race
// retry. On failure it keeps a manager that recovered concurrently, and
// otherwise REFUSES: the record and its conversation stay where they are.
//
// It used to delete the record and create a fresh manager when a resume
// failed, which discarded the conversation on any failure lever could not
// classify (lever#3: a VM reboot that corrupted the container state). A
// resume that fails says the session could not be brought up NOW; it does not
// say the session is worthless. Discarding one is the operator's decision,
// and `lever up --fresh` is how they make it.
//
// Self-heal an expired mTLS leaf BEFORE resuming. A manager whose short-lived
// agent leaf expired while the instance was down (the in-container renew
// sidecar cannot run while stopped, so downtime longer than the leaf lifetime
// guarantees expiry) must be able to re-enrol on boot. lever-agent's boot
// re-enrols an expired leaf (ValidCert → false), but ONLY if a fresh, unspent
// enrolment ticket is staged — and the resume path used to stage none, so the
// leaf stayed dead and every brokered call failed the mTLS handshake until a
// full `lever destroy`. ensureFreshBootstrap fixes that without a teardown: it
// is a no-op when this run already minted fresh material (the normal stop→up
// path, where broker-up reopened the /bootstrap latch and
// mint-manager-bootstrap already staged a ticket), and re-arms + stages a
// fresh ticket only when the broker outlived a spent latch across the
// manager's downtime — exactly the expired-leaf case. The unspent ticket is
// harmless when the leaf is still valid (boot's ValidCert passes and skips
// enrol, leaving it unredeemed).
func (r *run) resumeOrRecover(ctx context.Context, jp string, v resumeVerb) error {
	if err := r.ensureFreshBootstrap(ctx); err != nil {
		return err
	}
	rerr := r.retryOnBrokerUnavailable(ctx, v.resume)
	if rerr == nil {
		return nil
	}
	if r.managerConcurrentlyRecovered(ctx, jp) {
		r.d.Log("start-manager: %s failed (%v) but the manager is now running — recovered concurrently (auto-re-enrol healer); keeping the session", v.label, rerr)
		return nil
	}
	if scion.IsRefusedByHub(rerr) {
		// The hub refused the call, which says nothing about the manager.
		return HubRefusedResume(v.label, rerr)
	}
	return ResumeFailed(v.label, rerr)
}

// ResumeFailed is the error for a manager resume that failed and was not a
// hub refusal: the record stays, and the operator decides. `lever up`'s own
// resume path (a machine that is already running) returns it too, so both
// paths say the same thing.
//
// A script tells this case by the exit code (cli.ExitManagerResumeFailed),
// not by the text. The fixed sentences come first and the hub's words last
// ("Cause: …"): the cause can carry text an agent chose, and nothing an
// agent writes may sit where a reader expects lever's own statement.
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

// retryOnBrokerUnavailable runs action up to r.brokerStart.Attempts times,
// waiting r.brokerStart.Interval between attempts, for as long as each failure is
// the transient runtime-broker-unavailable race (scion.IsBrokerUnavailable). A nil
// result, or any non-transient error, returns immediately — the retry budget
// exists purely to absorb the registration race, not to mask real failures.
// Shared by startManagerCreate's Start retry and start-manager's Resume retry:
// `scion resume` hits the identical runtime-broker race as `scion start` (see
// scion.IsBrokerUnavailable's doc), so both need the same absorbing retry.
func (r *run) retryOnBrokerUnavailable(ctx context.Context, action func() error) error {
	var last error
	err := retry.Until(ctx, r.brokerStart.Attempts, r.brokerStart.Interval, func() (bool, error) {
		last = action()
		if last == nil {
			return true, nil
		}
		if !scion.IsBrokerUnavailable(last) {
			return false, last
		}
		return false, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return last // the transient error itself, as before the shared loop
	}
	return err
}

// listAgentsRetry lists the project's agents through the bounded
// runtime-broker-unavailable retry (retryOnBrokerUnavailable). Used by the two
// sites that observe the fleet across the async broker-registration window: the
// initial start-manager observe and the post-failed-resume re-observe (a blip
// there is correlated with the resume failure it is re-checking). NOTE:
// waitManagerLive's List is deliberately NOT routed here — it carries its own
// consume-an-attempt tolerance within its liveness budget.
func (r *run) listAgentsRetry(ctx context.Context, jp string) ([]scion.Agent, error) {
	var agents []scion.Agent
	if err := r.retryOnBrokerUnavailable(ctx, func() error {
		a, e := r.d.Scion.List(ctx, jp)
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

// managerConcurrentlyRecovered re-observes the manager record after a FAILED
// resume, before the apply ends with the resume error. The broker's
// auto-re-enrol healer (#22) lives in the broker daemon — started by the
// broker-up step, i.e. BEFORE start-manager runs — and it bounces lapsed
// agents via the same scion verbs this step uses, in a separate process with
// no coordination. So a resume failure here can mean "the healer's own
// suspend/resume was mid-flight", not "the manager is down": a record that
// is running on re-observation is a success, not an error to report.
// The observe rides retryOnBrokerUnavailable: the resume just failed against
// this same runtime, so a transient blip here is CORRELATED with that failure
// — an unretried List would undermine the re-observe with a false negative
// one level up. (Errors that survive the retry budget count as
// not-recovered: the resume error is reported and the record is kept.)
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

// startManagerCreate runs the create-manager retry loop: `scion start` races
// the runtime-broker registration (see Deps.BrokerStartRetry) and treats an
// "already running"/"already exists" 409 as success (idempotent re-apply, or a
// create-race against a record the observe step just missed — scion's own
// lazy hub-sync can transiently read a live record as absent). Shared by the absent-record branch and the create
// that follows a `--fresh` delete, so both take the identical retry
// behavior — including the bootstrap re-arm below, which is why it lives
// HERE rather than duplicated at each call site.
//
// A freshly-created scion agent record has no agent home to reuse (unlike
// resume, which restores an existing one), so lever-agent boot ALWAYS re-enrols after a create.
// If the broker's single-use /bootstrap latch was already consumed by an
// earlier apply against this same broker process (mint-manager-bootstrap
// tolerated ErrBootstrapLatched — see its doc — leaving r.minted false),
// a plain create is guaranteed to 403 and the container exits 1. So: before
// Start, ensure this apply run has fresh, enrolable material — either it was
// already minted earlier in this same run (r.minted, e.g.
// mint-manager-bootstrap succeeded outright, or an earlier create in this
// same Run already re-armed), or r.d.RearmBootstrap mints one now.
func (r *run) startManagerCreate(ctx context.Context, opts scion.StartOpts, mustCreate bool) error {
	// The second read_only host check (startManager ran the first): after a
	// --fresh delete, the manager that might have held the tree writable is
	// gone, so this is the look the create can rely on. Cheap, so it runs
	// before every create rather than only the --fresh one.
	if err := r.app.PrepareManagerReadOnlyHost(); err != nil {
		return fmt.Errorf("start-manager: %w", err)
	}
	if err := r.ensureFreshBootstrap(ctx); err != nil {
		return err
	}
	var commit func() error
	if r.d.BeginSession != nil {
		commit = r.d.BeginSession(r.app.Name)
	}
	created := false
	err := r.retryOnBrokerUnavailable(ctx, func() error {
		startErr := r.d.Scion.Start(ctx, opts)
		// Idempotent: a manager already running/existing (re-apply, or a
		// create-race the observe step missed) is success, not error. It is
		// not a fresh session, so it is not recorded as one.
		if startErr != nil && scion.AlreadyRunning(startErr) {
			if mustCreate {
				// Under `--fresh` nothing may exist at this point. An
				// existing record is the session the operator asked to
				// discard (or one something else created): never report it
				// as the fresh manager.
				// Either the observe step missed a live record, or an earlier
				// attempt of this very create left one behind and reported a
				// timeout. lever cannot tell which, so it claims neither a
				// discard nor a fresh manager.
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
			r.d.Log("start-manager: could not record the manager's fresh session (%v); contacts cannot post to it until the next fresh start", cerr)
		}
	}
	return err
}

// recoverDeleteAndCreate is the operator's explicit `--fresh`: emit the loud
// session-discarded notice, delete the record, and — only if the delete
// succeeds — create a fresh manager. Nothing else deletes a manager record
// (a failed resume refuses; see resumeOrRecover).
//
// logMsg is the fully-formed loud line, and deleteFailReason the clause for
// the hard delete-failure error ("start-manager: <reason> and delete failed:
// %w"): there is no safe fallback, since a fresh Start over an undeleted
// record would just 409.
func (r *run) recoverDeleteAndCreate(ctx context.Context, jp string, opts scion.StartOpts, logMsg, deleteFailReason string) error {
	r.d.Log("%s", logMsg)
	// The delete rides the same runtime-broker-race retry as a create or a
	// resume: on a cold VM the hub serves before its runtime broker has
	// registered, and a delete issued in that window fails the same way.
	// `up --fresh` is the operator's one way out of a manager that does not
	// resume, so it must not fail on a broker that was only late.
	derr := r.retryOnBrokerUnavailable(ctx, func() error { return r.d.Scion.Delete(ctx, r.app.Name, jp) })
	// What decides is the record, never the words of an error: an attempt
	// can remove the record and still report a failure, and a delete can
	// report success (or a "not found" about something else) with the record
	// still there. `--fresh` is how an operator evicts a session they no
	// longer trust, so "discarded" must be true before a create follows.
	agents, lerr := r.listAgentsRetry(ctx, jp)
	if lerr != nil {
		if derr != nil {
			return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
		}
		return fmt.Errorf("start-manager: %s: the delete was sent, but lever could not confirm the manager record is gone (%v); nothing was created. Run `lever up --fresh` again", deleteFailReason, lerr)
	}
	if rec := scion.FindAgent(agents, r.app.Name); rec != nil {
		if derr != nil {
			return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
		}
		return fmt.Errorf("start-manager: %s: the delete reported no error, but the manager record is still there (phase %s). The previous session was NOT discarded and nothing was created. Run `lever up --fresh` again", deleteFailReason, scion.BoundedQuote(rec.Phase))
	}
	return r.startManagerCreate(ctx, opts, true)
}

// ensureFreshBootstrap guarantees fresh, enrolable bootstrap material exists
// before a manager Start OR Resume. If this apply run already minted fresh
// material (r.minted), it's a no-op. Otherwise, when r.d.RearmBootstrap is
// set, it re-arms the broker's spent latch and mints+stages fresh material
// (recording it in r.minted so a SECOND create in the same Run — e.g. a
// failed-resume recovery that immediately re-creates — does not re-arm
// twice). A RearmBootstrap that fails is a hard error — a create without
// enrolable bootstrap is guaranteed to 403, so failing loudly now is strictly
// better than booting a manager doomed to crash-loop.
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

// waitManagerLive polls r.d.Scion.List until the manager's record shows BOTH
// Phase=="running" AND a live container, or attempts run out. This is the
// backstop for both false-success classes above this layer: a blind `scion
// start`'s 409 "already exists" error text matches the AlreadyRunning
// idempotency predicate (false success in OUR retry loop — scion's exit code
// itself is correctly non-zero), and `scion resume`/`scion start` report
// success ("resumed") for a container that dies moments later (a real scion
// race: its liveness check is a single immediate poll). Trusting the observed
// record — not CLI exit codes or error wording — is what makes start-manager's
// success meaningful. The live-container predicate is scion.ContainerLive,
// shared with the broker's worker liveness gate.
func (r *run) waitManagerLive(ctx context.Context, jp string, acted bool) error {
	b := r.managerLive
	if !acted {
		// Nothing was started or resumed: the manager was running before
		// this apply looked, so holding it for the settle window would prove
		// nothing and cost every `lever apply`/`lever reload` ten seconds.
		// One look still refuses a running RECORD over a dead container.
		b.Settle = 0
	}
	return gateManagerLive(ctx, r.d.Scion, b, r.app.Name, jp, "start-manager")
}

// gateManagerLive is the manager liveness gate every bring-up path shares: the
// start-manager step here and `lever up`'s own resume/no-op paths (WaitManagerLive
// / ObserveManagerLive), which act on the manager WITHOUT going through Run and
// used to print "is up." with no observation at all (lever#31). subject
// prefixes the exhaustion/death error; ctx errors pass through unwrapped.
func gateManagerLive(ctx context.Context, sc *scion.Client, b RetryBudget, name, project, subject string) error {
	err := scion.WaitAgentLive(ctx, func(c context.Context) ([]scion.Agent, error) {
		return sc.List(c, project)
	}, name, scion.LiveBudget{Attempts: b.Attempts, Interval: b.Interval, Settle: b.Settle})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: manager %q %w", subject, name, err)
}

// WaitManagerLive is the full gate — become live, then hold for the settle
// window — for a path that just acted on the manager (`lever up` after its own
// `scion resume`). Same List, same budget as the start-manager step.
func WaitManagerLive(ctx context.Context, d Deps, name, project string) error {
	return gateManagerLive(ctx, d.Scion, d.ManagerLiveRetry.or(defaultManagerLiveRetry), name, project, "up")
}

// observeAttempts bounds ObserveManagerLive's list retries: enough to ride
// out a transient hub error, short enough that a refusal arrives in seconds
// rather than after the fifteen the start gate allows.
const observeAttempts = 3

// ObserveManagerLive is `lever up`'s check for a manager that was already
// running when `up` looked. It started nothing, so it holds nothing: one
// observation (a failed list is retried observeAttempts times), no settle,
// and a refusal ONLY on positive evidence of a death — a missing record, a
// phase that is not running, or a container status that is present and not
// live. An EMPTY container status is not evidence either way: that column is
// refreshed by the runtime broker's heartbeat, not computed at read time, and
// a momentary blank must not turn a healthy, attachable manager into a
// refusal the morning after (lever#31 review). Every refusal names what was
// seen and the ways out.
func ObserveManagerLive(ctx context.Context, d Deps, name, project string) error {
	b := d.ManagerLiveRetry.or(defaultManagerLiveRetry)
	a, err := scion.ObserveAgent(ctx, func(c context.Context) ([]scion.Agent, error) {
		return d.Scion.List(c, project)
	}, name, observeAttempts, b.Interval)
	const remedy = "`lever attach` still tries the session; `lever doctor` shows the manager row; `lever stop` then `lever up` resumes it; `lever up --fresh` discards the conversation"
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("up: manager %q could not be observed: %w — %s", name, err, remedy)
	}
	switch {
	case a == nil:
		return fmt.Errorf("up: manager %q was running when up looked, but has no record now — %s", name, remedy)
	case a.Phase != scion.PhaseRunning:
		return fmt.Errorf("up: manager %q was running when up looked, but is in phase %s now (container %s) — %s", name, scion.BoundedQuote(a.Phase), scion.BoundedQuote(a.ContainerStatus), remedy)
	case a.ContainerStatus != "" && !scion.ContainerLive(a.ContainerStatus):
		return fmt.Errorf("up: manager %q record says running, but its container is %s — the harness died; %s", name, scion.BoundedQuote(a.ContainerStatus), remedy)
	}
	return nil
}

// JailPath maps a host path under tree to its location inside the jail (mount +
// suffix). Returns hostPath unchanged when mount=="" or hostPath is not under
// tree. Exported for the CLI's bootstrap-token step, which registers the tree
// root through the same mapping.
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

// defaultReadCred reads a credential file, refusing world-readable files (a real
// credential should be 0600) and oversized files. This is defence-in-depth for
// the credential projected into agent containers; see
// docs-site/_guides/security-model-config-trust.md §5.
func defaultReadCred(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	// Group bits count, not just world. This is the manager's LLM credential in
	// subscription mode — the longest-lived, highest-value secret lever handles
	// — and every other credential in the system (api_key_file, the controller
	// PAT, the staged bootstrap) is held to exactly 0600. `lever doctor` already
	// FAILS a group-readable credential file; accepting one here let apply read
	// it and project it into every agent container while doctor called it broken.
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
