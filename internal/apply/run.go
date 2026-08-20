package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// ErrBootstrapLatched is returned by MintManagerBootstrap when the broker's
// single-use /bootstrap latch is already consumed (HTTP 403). The mint step
// tolerates it (the manager already has its bootstrap from a prior apply against
// the SAME broker process). A broker RESTART reopens the latch, so mint then
// succeeds and re-deposits a fresh ticket — letting a partially-failed first
// apply recover on re-apply (vs the old skip-if-file-exists, which deadlocked).
var ErrBootstrapLatched = errors.New("broker /bootstrap latch already consumed")

// brokerStartAttempts/brokerStartInterval bound the start-manager retry that
// absorbs the runtime-broker registration race: the scion runtime broker
// registers with the hub ASYNCHRONOUSLY after the server starts, so a
// start-manager that runs too soon gets "no runtime brokers available". The hub
// itself is up (the scion-server health check passed), so this is purely a
// timing window — retry until the broker comes online. Package vars so tests run
// fast. (Only the first start races; workers start later when the broker is ready.)
var (
	brokerStartAttempts = 30
	brokerStartInterval = 1 * time.Second
)

// isBrokerUnavailable reports whether err is the transient "runtime broker not
// yet registered" condition during bring-up (the registration race), as opposed
// to a real failure. The scion workstation daemon starts its Hub API and its
// runtime broker separately: waitHubReady confirms the Hub API serves, but the
// runtime broker registers ASYNCHRONOUSLY afterward, so a call issued in that
// window fails. scion words it three ways depending on the verb and how far the
// call got before giving up:
//   - `scion start`: plural "No runtime brokers available".
//   - `scion resume`: singular "no runtime broker available" — the SAME race
//     (confirmed in scion pkg/hub/handlers_agent_create_helpers.go:354,408).
//   - either, on a cold VM: "deadline exceeded" — scion's own internal wait for
//     the broker times out and surfaces the hub timeout instead of the clean
//     message (observed live: "context deadline exceeded from the Hub during
//     start-manager", which needed a second `up` to reconcile).
//
// All must be matched or the retry never sees its own transient error as
// retryable. retryOnBrokerUnavailable is bounded and checks ctx between
// attempts, so a deadline from OUR context (a genuine timeout, not scion's
// internal one) returns promptly instead of looping the full budget.
func isBrokerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no_runtime_broker") ||
		strings.Contains(s, "No runtime brokers available") ||
		strings.Contains(s, "no runtime broker available") ||
		strings.Contains(s, "deadline exceeded")
}

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

// bootTracker threads the manager's bootstrap material through Run's steps,
// AND records whether THIS apply run actually minted fresh material (as
// opposed to the mint-manager-bootstrap step tolerating an already-spent
// latch — e.g. an idempotent re-apply against the same broker process; see
// ErrBootstrapLatched). start-manager's create path needs exactly that
// "did we mint fresh material this run" signal: relying on BootstrapMaterial's
// zero value would work today (the tolerate-latch path never assigns it), but
// that's an implicit contract worth making explicit rather than fragile.
type bootTracker struct {
	material BootstrapMaterial
	minted   bool
}

// StageBootstrapMaterial writes m as the manager's one-time enrolment ticket
// into treeDir/.lever/bootstrap.json (0600) — the path lever-agent boot reads
// by convention (see start-manager's LEVER_BOOTSTRAP comment below). This is
// the ONE staging code path, shared by the mint-manager-bootstrap step and
// start-manager's create-path re-arm (Deps.RearmBootstrap, implemented by the
// CLI, which stages directly into the tree since start-manager's Step.Target
// is the manager's slug, not the tree dir — see jailPath/Plan).
func StageBootstrapMaterial(treeDir string, m BootstrapMaterial) error {
	// treeDir is the confinement anchor: it is the mount point, the one
	// component an agent cannot replace. Everything below it is agent-writable,
	// so wire.Stage refuses to follow a symlink planted at `.lever`.
	return wire.Stage(treeDir, ".lever", m)
}

// Deps are the executor's collaborators, injected so Run is testable offline.
// JailUp/LoadImage are host-side (backend.EnsureUp, docker-save|podman-load);
// Scion runs IN the jail (built on a JailRunner).
type Deps struct {
	JailUp    func(ctx context.Context, app *config.App) error
	LoadImage func(ctx context.Context, imageRef string) error
	// ImageLoaded reports whether the jail already holds imageRef at the same
	// image ID as the host, letting the load-image step skip a redundant
	// multi-GB `docker save | podman load` re-stream. This is what stops a
	// re-apply (including the first-boot retry loop, which re-runs the WHOLE
	// plan on any step failure) from re-importing every image each time. It is
	// fail-open by construction (false on any uncertainty), so a wrong answer
	// costs a redundant load, never a wrongly-skipped one. nil ⇒ always load
	// (tests, pre-guard behavior).
	ImageLoaded func(ctx context.Context, imageRef string) bool
	// PruneImages reclaims dangling images from the jail after a load, so a
	// rebuilt image does not ratchet the grow-only jail disk up by a full image
	// size (the superseded copy goes untagged). A no-op when the load added a
	// new image. Best-effort: a prune failure is logged, not fatal to the
	// bring-up. nil ⇒ skip pruning (tests).
	PruneImages   func(ctx context.Context) error
	Scion         *scion.Client
	ReadCred      func(path string) (string, error) // nil ⇒ defaultReadCred
	JailMount     string                            // jail path where app.Tree is bind-mounted (e.g. "/lever"); "" disables translation
	StartBroker   func(ctx context.Context) error
	BrokerHealthy func(ctx context.Context) error
	// EnsureControllerPAT runs the bootstrap-token step: the whole controller-PAT
	// mint window as one injected op, keeping this package scion-agnostic (the
	// CLI wires the real throwaway-hub → mint → persist → kill → delete-dev-token
	// logic — see plan.go's bootstrap-token Step doc). It MUST be idempotent: if
	// a valid PAT is already persisted, no-op. nil ⇒ skip (dev-auth-open mode —
	// unit tests / legacy — the scion-server step still runs, just without a
	// pre-minted SCION_HUB_TOKEN to lock the real hub against).
	EnsureControllerPAT func(ctx context.Context) error
	// WaitBrokerReady blocks until the scion runtime broker is registered AND
	// online, right before start-manager acts. The workstation daemon brings up
	// its Hub API (confirmed by scion-server's waitHubReady) and its runtime
	// broker separately, so without this gate the first create/resume races the
	// broker's async registration — the flakiness that made first-boot need a
	// second `up`. The implementation is fail-soft (returns nil on timeout), so
	// it never fails the bring-up on its own; the start path's broker-unavailable
	// retry still backstops it. nil ⇒ skip the gate (tests / legacy).
	WaitBrokerReady      func(ctx context.Context, project string) error
	MintManagerBootstrap func(ctx context.Context) (BootstrapMaterial, error)
	// RearmBootstrap restarts the broker (re-arming its single-use /bootstrap
	// latch; broker CA + signing keys persist on disk so existing agent certs
	// and capability tokens survive the restart), then mints AND STAGES fresh
	// bootstrap material exactly like the mint-manager-bootstrap step. Called
	// by start-manager's create path when no fresh material was minted this
	// apply (the mint step tolerated a spent latch). nil => the create path
	// proceeds without re-arm (tests; and resume paths never need it).
	RearmBootstrap func(ctx context.Context) (BootstrapMaterial, error)
	// BrokerOnly reduces the bring-up to {jail-up, broker-up, mint-manager-bootstrap}
	// for the VM-level acceptance gate (which drives lever-agent directly and
	// never invokes scion). Default false = full bring-up (unchanged).
	BrokerOnly bool
	// EnsureHubLogin brings the GUEST half of the remote-access login path up
	// to date before the hub starts: the loopback forwarder that makes
	// lever's host-side OIDC provider look local to the hub, and the
	// `oidc_login` block in the guest's ~/.scion/settings.yaml. It reports
	// whether that configuration changed, which is the signal to restart a
	// hub that is already running — see runScionServer. It is the CLI's job
	// to make this a no-op when remote access is off. nil ⇒ skip (tests, and
	// the broker-only VM gate).
	EnsureHubLogin func(ctx context.Context) (bool, error)
	// DisableHubLogin converges the GUEST half of the login path off: stop and
	// remove the forwarder, drop the `oidc_login` block. Like StopRemoteProxy
	// it is NOT a Plan step — Run calls it whenever app.RemoteEnabled() is
	// false, so an instance that turned remote access back off does not keep
	// an unauthenticated jail→host loopback bridge alive for a feature that no
	// longer exists. nil ⇒ skip (tests, and the broker-only VM gate).
	DisableHubLogin func(ctx context.Context) error
	// EnsureAgentTemplate backs the agent-template step: put lever's overlay
	// template in front of scion's stock `default` so newly provisioned agents
	// do NOT launch with `--system-prompt '# Placeholder'`, which replaces
	// Claude Code's entire built-in system prompt. projectDir is JAIL-side —
	// the scion client behind it runs in the jail, where the host tree exists
	// only at the mount point (project scope is the only settings scope that wins) and
	// reports whether it changed anything. nil ⇒ skip (tests, and the
	// broker-only VM gate).
	//
	// Provisioning-time only, by nature: scion stages an agent's system prompt
	// once, when its home is created, and never re-stages it. So this governs
	// agents created from now on; an agent that already exists keeps whatever
	// it was provisioned with until its staged input is changed in place.
	EnsureAgentTemplate func(ctx context.Context, projectDir string) (bool, error)
	// StartRemoteProxy backs the remote-proxy step (present only when
	// app.RemoteEnabled(); see Plan): spawn — or confirm already running —
	// the daemonized `lever remote serve` proxy. nil ⇒ skip (tests, and any
	// config with remote disabled never reaches this step at all).
	StartRemoteProxy func(ctx context.Context) error
	// StopRemoteProxy tears the remote proxy down (by pidfile, tolerant of
	// an absent or stale one). It is NOT called from a Plan step — Run calls
	// it directly, unconditionally, whenever app.RemoteEnabled() is false,
	// so a proxy left running from a prior apply (remote.enabled flipped
	// back off since) converges to stopped rather than going on serving
	// traffic the config no longer authorizes. nil ⇒ skip (tests, and
	// configs that never enable remote in the first place).
	StopRemoteProxy func(ctx context.Context) error
	// RemoveJailFile removes a regular file at a jail-absolute path, through the
	// jail's own filesystem view. Used for the stale `.scion` marker so the
	// removal and the subsequent in-jail `scion init` cannot race across the
	// host/guest VirtioFS boundary (a host-side unlink is not promptly visible
	// to the guest's directory cache). Must NOT remove directories. nil ⇒ fall
	// back to a host-side remove (tests, broker-only VM gate).
	RemoveJailFile func(ctx context.Context, jailPath string) error
	// RemoveScionProjectConfigs removes any stale ~/.scion/project-configs
	// registration(s) whose workspace_path == jailWorkspacePath, BEFORE the
	// register-project step re-inits. Without this, every apply
	// mints a fresh registration via `scion init` and the old ones accumulate
	// (the `lever doctor` "duplicate registrations" finding) — this is the
	// removal counterpart to RemoveJailFile's marker-race fix above. nil ⇒
	// no-op (tests, broker-only VM gate).
	RemoveScionProjectConfigs func(ctx context.Context, jailWorkspacePath string) error
	// ScionProjectRegistered observes whether jailWorkspacePath already has
	// exactly one valid scion registration (one project-configs entry + the
	// in-tree marker present) BEFORE the register-project step
	// decides whether to run its destructive clean+init path at all. true →
	// skip marker removal, RemoveScionProjectConfigs, and `scion init`/`hub
	// link` entirely, so a re-apply does not tear down (and orphan) a
	// resumable scion agent record just to re-mint an identical registration.
	// nil, or a query error, falls through to the destructive path unchanged
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
	// still carries the mount. nil ⇒ no-op (tests). The broker-only VM gate
	// never reaches it at all: Plan filters KindRegisterProject out entirely.
	StripProjectSharedDirs func(ctx context.Context, projectName string) error
	// RepairScionHubEndpoint rewrites the hub endpoint recorded in the project's
	// scion registration when it no longer matches the real hub. Minting the
	// controller PAT `hub link`s the project against a THROWAWAY hub on its own
	// port; the register step's re-init would overwrite that, but it is skipped
	// whenever the registration is already sound, so a re-mint on an established
	// instance leaves the project pointing at a dead port. Every lever call
	// passes the endpoint explicitly, so the breakage lands only where scion runs
	// bare in the jail — `lever attach` (live failure 2026-08-11).
	//
	// nil ⇒ no-op (tests, broker-only VM gate).
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
	//
	// nil ⇒ no-op (tests, and the broker-only VM gate).
	VerifyAgentRole func(ctx context.Context, project, agent string) error
	// Log surfaces a loud, user-facing progress/warning line during apply —
	// currently just start-manager's resume-failed recovery notice ("resume
	// failed … starting FRESH, previous session lost"), which MUST reach the
	// user rather than vanish into a swallowed return value. nil ⇒ no-op
	// (tests, and any caller that doesn't need it). buildApplyDeps wires this
	// to the invoking cobra command's PrintErrf, mirroring how other user-
	// facing warnings already surface (see cli/stop.go, cli/down.go).
	Log func(format string, args ...any)
}

// logf calls d.Log if set, else no-ops. Small seam so call sites don't need a
// nil-check of their own for the (optional) Deps.Log field.
func logf(d Deps, format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// Run executes the bring-up Plan for app. jail-up/load-image are host-side; the
// rest run in the jail via Deps.Scion.
func Run(ctx context.Context, app *config.App, d Deps) error {
	var boot bootTracker
	for _, step := range Plan(app, PlanOpts{BrokerOnly: d.BrokerOnly}) {
		if err := runStep(ctx, app, step, d, &boot); err != nil {
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
		if err := stopRemoteProxyIfConfigured(ctx, d); err != nil {
			return fmt.Errorf("remote-proxy: %w", err)
		}
		// The GUEST half has to converge too, and for a sharper reason than
		// the proxy does: the login forwarder is an unauthenticated TCP bridge
		// from guest loopback — reachable from every agent's netns — to a host
		// loopback port beside lever's own broker listeners. Left running
		// after the feature is off, it keeps that port bridged into the jail
		// for whatever binds it next. See Deps.DisableHubLogin.
		if d.DisableHubLogin != nil {
			if err := d.DisableHubLogin(ctx); err != nil {
				return fmt.Errorf("hub login: %w", err)
			}
		}
	}
	return nil
}

// stopRemoteProxyIfConfigured calls Deps.StopRemoteProxy when wired; see its
// doc for why this runs outside the step loop.
func stopRemoteProxyIfConfigured(ctx context.Context, d Deps) error {
	if d.StopRemoteProxy == nil {
		return nil
	}
	return d.StopRemoteProxy(ctx)
}

// runStep is the thin dispatch over StepKind: it routes each step to its
// executor. The non-trivial case bodies live in per-kind run* helpers below,
// each closing over only (ctx, app, s, d, boot) — no hidden state. The default
// arm is a hard error so a Plan emitting an unknown kind fails loudly.
func runStep(ctx context.Context, app *config.App, s Step, d Deps, boot *bootTracker) error {
	switch s.Kind {
	case KindJailUp:
		return d.JailUp(ctx, app)
	case KindBrokerUp:
		return runBrokerUp(ctx, d)
	case KindLoadImage:
		return runLoadImage(ctx, s, d)
	case KindInitMachine:
		return d.Scion.InitMachine(ctx)
	case KindConfigRegistry:
		return d.Scion.ConfigSetGlobal(ctx, "image_registry", "scionlocal")
	case KindBootstrapToken:
		if d.EnsureControllerPAT == nil {
			return nil // dev-auth-open mode (unit tests / legacy) — skip
		}
		return d.EnsureControllerPAT(ctx)
	case KindScionServer:
		return runScionServer(ctx, app, d)
	case KindCredential:
		return runCredential(ctx, s, d)
	case KindRegisterProject:
		return runRegisterProject(ctx, app, s, d)
	case KindAgentTemplate:
		return runAgentTemplate(ctx, app, s, d)
	case KindMintManagerBootstrap:
		return runMintManagerBootstrap(ctx, s, d, boot)
	case KindStartManager:
		return stepStartManager(ctx, app, s, d, boot)
	case KindRemoteProxy:
		return runRemoteProxy(ctx, d)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// runScionServer runs the scion-server step: bring the guest's login path up
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
func runScionServer(ctx context.Context, app *config.App, d Deps) error {
	if d.EnsureHubLogin != nil {
		changed, err := d.EnsureHubLogin(ctx)
		if err != nil {
			return fmt.Errorf("hub login: %w", err)
		}
		if changed {
			logf(d, "lever: the hub's login configuration changed — restarting the hub so it is read")
			// Stop-then-start, not `scion server restart`: scion's own restart
			// refuses outright when the daemon is not running, and the hub
			// being already down is an ordinary state here — after a `lever
			// stop`, a VM reboot, or a crash. ServerStop tolerates that (see
			// its doc); ServerStart below tolerates the opposite.
			if err := d.Scion.ServerStop(ctx); err != nil {
				return fmt.Errorf("hub login: restart the hub: %w", err)
			}
		}
	}
	opts := scion.ServerOpts{
		WebPort:   8080,
		DevAuth:   false,
		EnableWeb: app.RemoteEnabled(),
	}
	if app.ScionWebAssets() {
		// Same predicate the backend used to decide whether to stage the
		// assets, so the flag can never point at a directory nothing put
		// anything in — see ServerOpts.WebAssetsDir for why that case is
		// worse than passing no flag at all.
		opts.WebAssetsDir = guest.ScionWebAssetsDir
	}
	return d.Scion.ServerStart(ctx, opts)
}

// runBrokerUp runs the broker-up step: start the host broker (+ first-party
// tools), then health-check it before the manager starts. Both Deps are
// optional (tests / dry paths).
func runBrokerUp(ctx context.Context, d Deps) error {
	if d.StartBroker == nil {
		return nil // tests / dry paths
	}
	if err := d.StartBroker(ctx); err != nil {
		return err
	}
	if d.BrokerHealthy != nil {
		return d.BrokerHealthy(ctx)
	}
	return nil
}

// runRemoteProxy runs the remote-proxy step: start (or confirm already
// running) the daemonized `lever remote serve` proxy. Only reached when Plan
// included the step, i.e. app.RemoteEnabled() — see Deps.StartRemoteProxy.
func runRemoteProxy(ctx context.Context, d Deps) error {
	if d.StartRemoteProxy == nil {
		return nil // tests / dry paths
	}
	return d.StartRemoteProxy(ctx)
}

// runAgentTemplate runs the agent-template step. Logs on change only: it is a
// provisioning-time change the operator cannot otherwise see, and silence on a
// no-op keeps a re-apply quiet.
func runAgentTemplate(ctx context.Context, app *config.App, s Step, d Deps) error {
	if d.EnsureAgentTemplate == nil {
		return nil // tests / broker-only gate
	}
	// The JAIL-side path, not the host one: the scion client behind this closure
	// runs inside the jail, where the host tree is visible only at the mount.
	changed, err := d.EnsureAgentTemplate(ctx, jailPath(s.Target, app.Tree, d.JailMount))
	if err != nil {
		return err
	}
	if changed {
		logf(d, "lever: agents will no longer be launched with scion's placeholder system prompt (new agents only; existing ones keep the prompt they were provisioned with)")
	}
	return nil
}

// runLoadImage runs the load-image step: skip the multi-GB re-import when the
// jail already holds this exact image (same ID; fail-open — ImageLoaded returns
// false on any doubt), otherwise load and then best-effort prune the superseded
// dangling image.
func runLoadImage(ctx context.Context, s Step, d Deps) error {
	if d.ImageLoaded != nil && d.ImageLoaded(ctx, s.Target) {
		return nil
	}
	if err := d.LoadImage(ctx, s.Target); err != nil {
		return err
	}
	// After a load, prune dangling images: when this load superseded a tag
	// (a rebuilt image), the old copy is now untagged and would otherwise
	// ratchet the grow-only jail disk. A no-op when the load just added a
	// brand-new image. Best-effort — a prune failure must never fail the
	// bring-up.
	if d.PruneImages != nil {
		if err := d.PruneImages(ctx); err != nil {
			logf(d, "load-image: pruning superseded jail images failed: %v", err)
		}
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

// runRegisterProject runs the register-project step: observe before doing
// anything destructive, then (only when the registration is unsound) clear the
// stale marker + project-config registration(s) and re-init + hub-link.
func runRegisterProject(ctx context.Context, app *config.App, s Step, d Deps) error {
	jp := jailPath(s.Target, app.Tree, d.JailMount)

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
	if d.ScionProjectRegistered != nil {
		if ok, err := d.ScionProjectRegistered(ctx, jp); err == nil && ok {
			// Sound registration, so nothing below runs — but the recorded hub
			// ENDPOINT can still be stale. Minting the controller PAT links the
			// project against a throwaway hub on its own port, and that link
			// survives precisely because this path skips the re-init. Repair it
			// here, where the skip happens.
			if err := repairScionHubEndpoint(ctx, d, jp); err != nil {
				return err
			}
			return stripProjectSharedDirs(ctx, d, jp)
		}
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
	// d.RemoveJailFile does. It is nil in tests and the broker-only VM gate
	// (no jail filesystem view to remove through there), so fall back to
	// the host-side remove, which still reaches the jail via the bind mount
	// (just without the same-view guarantee).
	if d.RemoveJailFile != nil {
		if err := d.RemoveJailFile(ctx, path.Join(jp, ".scion")); err != nil {
			return err
		}
	} else if err := removeStaleMarker(s.Target); err != nil {
		return err
	}
	// Clear any stale project-config registration(s) for this workspace path
	// before re-init, so `scion init` mints exactly ONE registration per
	// workspace instead of leaving the previous apply's dir behind.
	if d.RemoveScionProjectConfigs != nil {
		if err := d.RemoveScionProjectConfigs(ctx, jp); err != nil {
			return err
		}
	}
	if err := d.Scion.InitProject(ctx, jp); err != nil {
		return err
	}
	if err := d.Scion.HubLink(ctx, jp); err != nil {
		return err
	}
	return stripProjectSharedDirs(ctx, d, jp)
}

// stripProjectSharedDirs declines scion's default cross-agent scratchpad mount
// for the project registered at jailWorkspacePath. The hub knows the project by
// the workspace basename — the same name ensureControllerPAT passes to `hub
// token create` — so the two stay consistent by construction.
//
// A failure is fatal to apply. The alternative is starting a manager and
// workers that share a writable directory while the operator believes they do
// not, and a silent security regression is worse than a loud bring-up failure.
func stripProjectSharedDirs(ctx context.Context, d Deps, jailWorkspacePath string) error {
	if d.StripProjectSharedDirs == nil {
		return nil
	}
	return d.StripProjectSharedDirs(ctx, path.Base(jailWorkspacePath))
}

// repairScionHubEndpoint points the project's recorded hub endpoint back at the
// real hub. A no-op when the hook is unwired (tests) or the endpoint is already
// right; see Deps.RepairScionHubEndpoint.
func repairScionHubEndpoint(ctx context.Context, d Deps, jailWorkspacePath string) error {
	if d.RepairScionHubEndpoint == nil {
		return nil
	}
	return d.RepairScionHubEndpoint(ctx, jailWorkspacePath)
}

// runMintManagerBootstrap runs the mint-manager-bootstrap step: mint the
// manager's one-time enrol ticket and stage it (0600) for lever-agent to read.
// Idempotent against the LIVE broker latch (not a stale file): a spent latch is
// tolerated only when a ticket is already staged.
func runMintManagerBootstrap(ctx context.Context, s Step, d Deps, boot *bootTracker) error {
	if d.MintManagerBootstrap == nil {
		return nil
	}
	// Idempotent (tied to the LIVE broker latch, not a stale file): mint; if the
	// latch is already consumed (same broker process as a prior apply), tolerate
	// it — the manager has its bootstrap.json from then. After a broker restart
	// the latch reopens, mint succeeds, and a fresh ticket is deposited, so a
	// partially-failed first apply (bootstrap written but manager never enrolled)
	// recovers on re-apply. (*boot is not read after this step.)
	m, err := d.MintManagerBootstrap(ctx)
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
	boot.material = m
	boot.minted = true
	// Deposit it as a 0600 file in the mount (the lever-agent reads it).
	return StageBootstrapMaterial(s.Target, m)
}

// stepStartManager runs the start-manager plan step: observe the manager
// record, then act on the delta (create / no-op / resume / forced resume /
// loud recovery) and verify the container is actually live. Split out of
// runStep's dispatch; the three unresumable tails share recoverDeleteAndCreate.
func stepStartManager(ctx context.Context, app *config.App, s Step, d Deps, boot *bootTracker) error {
	task := ""
	if p := app.ManagerPromptPath(); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading manager prompt %s: %w", p, err)
		}
		task = strings.TrimSpace(string(b))
	}
	jp := jailPath(app.Tree, app.Tree, d.JailMount)
	// Gate on runtime-broker readiness before any create/resume: the workstation
	// daemon registers its runtime broker asynchronously AFTER its Hub API comes
	// up (waitHubReady only proved the latter), so acting now would race it. This
	// is the proactive complement to the broker-unavailable retry below — wait
	// for a ready broker rather than only reacting when a call fails against a
	// not-yet-ready one. Fail-soft (never errors on timeout); the retry backstops.
	if d.WaitBrokerReady != nil {
		if err := d.WaitBrokerReady(ctx, jp); err != nil {
			return fmt.Errorf("start-manager: waiting for runtime broker: %w", err)
		}
	}
	// api-key mode: convey LEVER_LLM_AUTH=api-key to the manager container so
	// its pre-start hook enters api-key mode (the hook reads $LEVER_LLM_AUTH;
	// scion projects Hub env before pre-start hooks run). Project-scoped (the
	// manager's project = jp) so it never leaks to other agents. Set BEFORE
	// start so it is present when the container boots.
	if app.EffectiveManagerLLMAuth() == config.LLMAuthAPIKey {
		if err := d.Scion.EnvSet(ctx, jp, "LEVER_LLM_AUTH", "api-key"); err != nil {
			return fmt.Errorf("set LEVER_LLM_AUTH for manager: %w", err)
		}
		// Satisfy scion's start-time auth gate with a placeholder ANTHROPIC_API_KEY
		// (Hub secret, projected to every container — fine since the instance is
		// uniformly api-key). It is a sentinel, NOT a real credential: the agent's
		// real LLM credential is the in-container broker capability token, and
		// the broker /llm overwrites this placeholder x-api-key with the real key.
		// Without it scion's env-gather/auth-resolution refuses to launch the
		// container (and thus lever-agent boot, which writes the real token). Set
		// once here; later-started workers inherit the same Hub secret.
		if err := d.Scion.SecretSet(ctx, "ANTHROPIC_API_KEY", apiKeyPlaceholder); err != nil {
			return fmt.Errorf("set placeholder ANTHROPIC_API_KEY: %w", err)
		}
	}
	// LEVER_BOOTSTRAP reconciliation: we do NOT set
	// LEVER_BOOTSTRAP here. lever-agent boot's canonical-path default
	// (./.lever/bootstrap.json relative to CWD) suffices: scion sets
	// --workspace = jp (the in-jail project tree), and the container's CWD is
	// /workspace, so ./.lever/bootstrap.json resolves to jp/.lever/bootstrap.json —
	// exactly where mint-manager-bootstrap wrote the manager's bootstrap.json.
	// Injecting an env var would be redundant and add a scion StartOpts.Env
	// dependency that the file convention avoids.
	opts := scion.StartOpts{
		Worker: app.Name, Task: task, Project: jp, Image: app.ManagerImage(), Harness: "claude",
		// Workspace = the in-jail project tree, so the manager edits the real
		// host files in place (verified 2026-06-16). Without it scion mounts a
		// managed copy of the externalized config dir, not the live tree.
		Workspace: jp,
		// api-key: start with --harness-auth api-key (satisfied by the placeholder
		// secret set above); the real credential arrives in-container.
		APIKey: app.EffectiveManagerLLMAuth() == config.LLMAuthAPIKey,
	}
	// Observe, then act on the delta — scion's verbs are state-specific:
	// start CREATES (409 "already exists" over a stopped record; the 409
	// error TEXT matches AlreadyRunning, so a blind start false-succeeds
	// through that idempotency check — scion's own exit code is correctly
	// non-zero, verified upstream 2026-07-04); resume
	// covers suspended AND stopped records, relaunching with
	// `claude --continue` (conversation restored). Live evidence
	// 2026-07-04 (see the resume-reconciliation plan's Evidence base).
	//
	// The Hub API is up by this point in Plan() (scion-server ran first, and
	// waitHubReady confirmed it), but the runtime broker registers
	// asynchronously after it, so this FIRST call into the hub can still hit
	// the registration window — on a cold VM as a "deadline exceeded" from
	// the hub. So the observe rides the SAME bounded retry as the Start/Resume
	// below (isBrokerUnavailable): a transient broker-not-ready blip is
	// retried, and only a persistent or genuinely-different error is fatal.
	agents, lerr := listAgentsRetry(ctx, d, jp)
	if lerr != nil {
		return fmt.Errorf("start-manager: observing agents: %w", lerr)
	}
	rec := scion.FindAgent(agents, app.Name)

	// Refuse to KEEP a record whose stored role the installed scion would read
	// as `full` (see Deps.VerifyAgentRole). Only the phases below keep the
	// record: rec == nil creates one, and the default branch deletes and
	// recreates it, both of which stamp a role themselves.
	//
	// This returns rather than falling into recoverDeleteAndCreate on purpose.
	// That recovery discards the conversation, and refusing here exists to give
	// the operator the choice — losing the session is one of the two ways out,
	// not something a guard may take on their behalf.
	if rec != nil && d.VerifyAgentRole != nil {
		switch rec.Phase {
		case scion.PhaseRunning, scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError:
			if err := d.VerifyAgentRole(ctx, path.Base(jp), app.Name); err != nil {
				return fmt.Errorf("start-manager: %w", err)
			}
		}
	}

	switch {
	case rec == nil:
		if err := startManagerCreate(ctx, d, boot, opts); err != nil {
			return err
		}
	case rec.Phase == scion.PhaseRunning:
		// No-op — fall through to the liveness verify below, which still
		// confirms the container is actually up: a running RECORD with a
		// dead container must fail loudly, not silently pass.
	case rec.Phase == scion.PhaseSuspended || rec.Phase == scion.PhaseStopped:
		// Self-heal an expired mTLS leaf BEFORE resuming. A manager whose
		// short-lived agent leaf expired while the instance was down (the
		// in-container renew sidecar cannot run while stopped, so downtime
		// longer than the leaf lifetime guarantees expiry) must be able to
		// re-enrol on boot. lever-agent's boot re-enrols an expired leaf
		// (ValidCert → false), but ONLY if a fresh, unspent enrolment ticket
		// is staged — and the resume path used to stage none, so the leaf
		// stayed dead and every brokered call failed the mTLS handshake until
		// a full `lever destroy`. ensureFreshBootstrap fixes that without a
		// teardown: it is a no-op when this run already minted fresh material
		// (the normal stop→up path, where broker-up reopened the /bootstrap
		// latch and mint-manager-bootstrap already staged a ticket), and
		// re-arms + stages a fresh ticket only when the broker outlived a
		// spent latch across the manager's downtime — exactly the expired-leaf
		// case. The unspent ticket is harmless when the leaf is still valid
		// (boot's ValidCert passes and skips enrol, leaving it unredeemed).
		if err := ensureFreshBootstrap(ctx, d, boot); err != nil {
			return err
		}
		// Resume rides the SAME runtime-broker-race retry as a create Start
		// (see isBrokerUnavailable's doc): on a cold VM the runtime broker may
		// not have re-registered with the hub yet, and resume hits that
		// identical transient window. Only once the retry budget is exhausted
		// (or the error is not the transient one at all) is the session
		// declared unrecoverable.
		if rerr := retryOnBrokerUnavailable(ctx, func() error {
			return d.Scion.Resume(ctx, app.Name, jp)
		}); rerr != nil {
			if managerConcurrentlyRecovered(ctx, d, app.Name, jp) {
				logf(d, "start-manager: resume failed (%v) but the manager is now running — recovered concurrently (auto-re-enrol healer); keeping the session", rerr)
			} else {
				// LOUD recovery: the conversation could not be restored. This MUST
				// reach the user — resume failing means the durable session (the
				// whole point of suspending, not stopping, at power-off; see
				// cli/stop.go) is about to be discarded.
				if err := recoverDeleteAndCreate(ctx, d, boot, app.Name, jp, opts,
					fmt.Sprintf("start-manager: resume failed (%v) — deleting the manager record and starting FRESH (previous session lost)", rerr),
					fmt.Sprintf("resume failed (%v)", rerr)); err != nil {
					return err
				}
			}
		}
	case rec.Phase == scion.PhaseError:
		// A crashed/wedged manager record. Since scion#895 (`resume
		// --force`, pin >= 68507153) the error phase IS recoverable — try
		// that first, with a fresh ticket staged (the leaf may have lapsed
		// while wedged; same rationale as the suspended branch), and only
		// discard the conversation when the forced resume itself fails.
		// Live motivation: 2026-07-31, an OrbStack VM reboot corrupted the
		// container state, resume failed, and the then-unconditional
		// delete+fresh destroyed the manager conversation (#3).
		if err := ensureFreshBootstrap(ctx, d, boot); err != nil {
			return err
		}
		if rerr := retryOnBrokerUnavailable(ctx, func() error {
			return d.Scion.ResumeForce(ctx, app.Name, jp)
		}); rerr != nil {
			if managerConcurrentlyRecovered(ctx, d, app.Name, jp) {
				logf(d, "start-manager: resume --force failed (%v) but the manager is now running — recovered concurrently (auto-re-enrol healer); keeping the session", rerr)
			} else {
				// LOUD recovery, exactly as the failed-resume path above.
				if err := recoverDeleteAndCreate(ctx, d, boot, app.Name, jp, opts,
					fmt.Sprintf("start-manager: manager in phase \"error\" and resume --force failed (%v) — deleting the manager record and starting FRESH (previous session lost)", rerr),
					fmt.Sprintf("forced resume failed (%v)", rerr)); err != nil {
					return err
				}
			}
		}
	default:
		// Any other phase — scion's full enum also has created,
		// provisioning, cloning, starting, and stopping (see
		// pkg/agent/state/state.go) — is not resumable: `scion resume` is
		// documented for suspended/stopped records only (`--force` only
		// recovers "error", handled above), and `scion list`'s
		// JSON phase field is the canonical (and only) signal we have, so we
		// cannot be cleverer here without more scion verbs (e.g. there is no
		// "wait for starting to settle" verb to poll instead). A record
		// caught mid-transition by an
		// interrupted prior `lever up` (phase "starting"/"created"/…) must
		// still let `up` converge, so this takes the SAME loud delete+fresh
		// recovery as a failed resume, rather than hard-failing (bricking)
		// the apply with no path forward but a hard `lever destroy`.
		if err := recoverDeleteAndCreate(ctx, d, boot, app.Name, jp, opts,
			fmt.Sprintf("start-manager: manager %q in phase %q — deleting and starting FRESH (previous session lost)", app.Name, rec.Phase),
			fmt.Sprintf("manager in phase %q", rec.Phase)); err != nil {
			return err
		}
	}
	return waitManagerLive(ctx, d, jp, app.Name)
}

// retryOnBrokerUnavailable runs action up to brokerStartAttempts times,
// waiting brokerStartInterval between attempts, for as long as each failure is
// the transient runtime-broker-unavailable race (isBrokerUnavailable). A nil
// result, or any non-transient error, returns immediately — the retry budget
// exists purely to absorb the registration race, not to mask real failures.
// Shared by startManagerCreate's Start retry and start-manager's Resume retry:
// `scion resume` hits the identical runtime-broker race as `scion start` (see
// isBrokerUnavailable's doc), so both need the same absorbing retry.
func retryOnBrokerUnavailable(ctx context.Context, action func() error) error {
	var err error
	for attempt := 0; attempt < brokerStartAttempts; attempt++ {
		err = action()
		if err == nil || !isBrokerUnavailable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(brokerStartInterval):
		}
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
func listAgentsRetry(ctx context.Context, d Deps, jp string) ([]scion.Agent, error) {
	var agents []scion.Agent
	if err := retryOnBrokerUnavailable(ctx, func() error {
		a, e := d.Scion.List(ctx, jp)
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

// startManagerCreate runs the create-manager retry loop: `scion start` races
// the runtime-broker registration (see brokerStartAttempts) and treats an
// "already running"/"already exists" 409 as success (idempotent re-apply, or a
// create-race against a record the observe step just missed — scion's own
// lazy hub-sync can transiently read a live record as absent; see the plan's
// Evidence base). Shared by the absent-record branch and the post-delete
// recovery branches above (a failed resume, or an unresumable phase, falls
// back to exactly this same create path), so all three take the identical
// retry behavior — including the bootstrap re-arm below, which is why it
// lives HERE rather than duplicated at each of the three call sites.
//
// A freshly-created scion agent record has no agent home to reuse (unlike
// resume, which restores an existing one — see the resume-reconciliation
// plan's Evidence base), so lever-agent boot ALWAYS re-enrols after a create.
// If the broker's single-use /bootstrap latch was already consumed by an
// earlier apply against this same broker process (mint-manager-bootstrap
// tolerated ErrBootstrapLatched — see its doc — leaving boot.minted false),
// a plain create is guaranteed to 403 and the container exits 1. So: before
// Start, ensure this apply run has fresh, enrolable material — either it was
// already minted earlier in this same run (boot.minted, e.g.
// mint-manager-bootstrap succeeded outright, or an earlier create in this
// same Run already re-armed), or d.RearmBootstrap mints one now.
// managerConcurrentlyRecovered re-observes the manager record after a FAILED
// resume, before the loud delete+fresh recovery destroys the session. The
// broker's auto-re-enrol healer (#22) lives in the broker daemon — started by
// the broker-up step, i.e. BEFORE start-manager runs — and it bounces lapsed
// agents via the same scion verbs this step uses, in a separate process with
// no coordination. So a resume failure here can mean "the healer's own
// suspend/resume was mid-flight", not "unrecoverable" — and deleting on it
// would destroy the exact conversation both recovery paths exist to save.
// Only a record that is NOT running on re-observation justifies the delete.
// The observe rides retryOnBrokerUnavailable: the resume just failed against
// this same runtime, so a transient blip here is CORRELATED with that failure
// — an unretried List would undermine the re-observe with a false negative
// one level up. (Errors that survive the retry budget count as not-recovered:
// fail toward the loud path, which at least tells the user what it is about
// to do.)
func managerConcurrentlyRecovered(ctx context.Context, d Deps, name, jp string) bool {
	agents, err := listAgentsRetry(ctx, d, jp)
	if err != nil {
		return false
	}
	if a := scion.FindAgent(agents, name); a != nil {
		return a.Phase == scion.PhaseRunning
	}
	return false
}

func startManagerCreate(ctx context.Context, d Deps, boot *bootTracker, opts scion.StartOpts) error {
	if err := ensureFreshBootstrap(ctx, d, boot); err != nil {
		return err
	}
	return retryOnBrokerUnavailable(ctx, func() error {
		startErr := d.Scion.Start(ctx, opts)
		// Idempotent: a manager already running/existing (re-apply, or a
		// create-race the observe step missed) is success, not error.
		if startErr != nil && scion.AlreadyRunning(startErr) {
			return nil
		}
		return startErr
	})
}

// recoverDeleteAndCreate performs the LOUD delete+fresh recovery shared by
// start-manager's three unresumable tails (a failed resume, a failed forced
// resume, and any non-resumable phase): emit the caller's loud
// previous-session-lost notice, delete the stale record, and — only if the
// delete succeeds — fall back to startManagerCreate's fresh create.
//
// The wording differs per tail and is preserved verbatim: logMsg is the
// fully-formed loud line, and deleteFailReason is that tail's clause for the
// hard delete-failure error ("start-manager: <reason> and delete failed: %w"),
// which surfaces BOTH the original failure (baked into the clause) and the
// delete failure — there is no safe fallback, since a fresh Start over an
// undeleted, un-resumable record would just 409 again.
func recoverDeleteAndCreate(ctx context.Context, d Deps, boot *bootTracker, name, jp string, opts scion.StartOpts, logMsg, deleteFailReason string) error {
	logf(d, "%s", logMsg)
	if derr := d.Scion.Delete(ctx, name, jp); derr != nil {
		return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
	}
	return startManagerCreate(ctx, d, boot, opts)
}

// ensureFreshBootstrap guarantees fresh, enrolable bootstrap material exists
// before a manager Start OR Resume. If this apply run already minted fresh
// material (boot.minted), it's a no-op. Otherwise, when d.RearmBootstrap is
// set, it re-arms the broker's spent latch and mints+stages fresh material
// (recording it into *boot so a SECOND create in the same Run — e.g. a
// failed-resume recovery that immediately re-creates — does not re-arm
// twice). d.RearmBootstrap == nil is tolerated (tests, and the broker-only VM
// acceptance gate, which never reaches start-manager at all): the create path
// proceeds unguarded, matching pre-fix behavior. A non-nil RearmBootstrap that
// itself fails is a hard error — a create without enrolable bootstrap is
// guaranteed to 403, so failing loudly now is strictly better than booting a
// manager doomed to crash-loop.
func ensureFreshBootstrap(ctx context.Context, d Deps, boot *bootTracker) error {
	if boot.minted || d.RearmBootstrap == nil {
		return nil
	}
	m, err := d.RearmBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("start-manager: re-arming the broker's spent bootstrap latch: %w", err)
	}
	boot.material = m
	boot.minted = true
	return nil
}

// managerLiveAttempts/managerLiveInterval bound waitManagerLive's post-start
// poll. Package vars so tests shrink them.
var (
	managerLiveAttempts = 15
	managerLiveInterval = 1 * time.Second
)

// waitManagerLive polls d.Scion.List until slug's record shows BOTH
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
func waitManagerLive(ctx context.Context, d Deps, jp, slug string) error {
	err := scion.WaitAgentLive(ctx, func(c context.Context) ([]scion.Agent, error) {
		return d.Scion.List(c, jp)
	}, slug, managerLiveAttempts, managerLiveInterval)
	if err == nil {
		return nil
	}
	// WaitAgentLive returns ctx.Err() unwrapped on cancellation; pass it through
	// as-is (no start-manager prefix) and prefix only the exhaustion error.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("start-manager: manager %q %w", slug, err)
}

// removeStaleMarker removes a `.scion` MARKER FILE at dir (left by a prior
// bring-up; it persists in the bind-mounted tree across jail teardown). It
// leaves a `.scion` DIRECTORY untouched — that's an in-repo git-mode project,
// not a stale directory marker. Absent `.scion` is a no-op.
func removeStaleMarker(dir string) error {
	p := filepath.Join(dir, ".scion")
	info, err := os.Lstat(p)
	if err != nil {
		return nil // nothing there (or unreadable) — fine
	}
	if info.IsDir() {
		return nil // in-repo project marker dir — leave it
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("removing stale .scion marker %s: %w", p, err)
	}
	return nil
}

// jailPath maps a host path under tree to its location inside the jail (mount + suffix).
// Returns hostPath unchanged when mount=="" or hostPath is not under tree.
func jailPath(hostPath, tree, mount string) string {
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
// the credential projected into agent containers; see security-model-config-trust.md §5.
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
