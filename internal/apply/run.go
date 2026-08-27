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

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
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
}

// or returns b, or def when b is the zero value.
func (b RetryBudget) or(def RetryBudget) RetryBudget {
	if b.Attempts == 0 && b.Interval == 0 {
		return def
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
var defaultManagerLiveRetry = RetryBudget{Attempts: 15, Interval: time.Second}

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
	// brokerStart and managerLive are the resolved retry budgets (Deps
	// overrides or the defaults), fixed for the run.
	brokerStart, managerLive RetryBudget
	// minted records whether THIS apply run actually minted fresh bootstrap
	// material (as opposed to the mint-manager-bootstrap step tolerating an
	// already-spent latch — e.g. an idempotent re-apply against the same broker
	// process; see ErrBootstrapLatched). start-manager's create path needs
	// exactly that "did we mint fresh material this run" signal.
	minted bool
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
// the jail (built on a JailRunner). Every func field except ReadCred is
// REQUIRED: Run refuses (Deps.check) before its first step when one is nil,
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
	// every hub start this package orders (hubServerOpts). The CLI ensures it
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
	// currently just start-manager's resume-failed recovery notice ("resume
	// failed … starting FRESH, previous session lost"), which MUST reach the
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
		//     passes no --web-assets-dir at all (hubServerOpts), and the
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
	return r.d.Scion.ServerStart(ctx, hubServerOpts(r.app, r.d.HubSessionSecret))
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
	return r.d.Scion.ServerStart(ctx, hubServerOpts(r.app, r.d.HubSessionSecret))
}

// hubServerOpts is the ONE description of how lever starts the hub for a given
// config. Both starts share it — this step's, and the restart disableHubLogin
// orders when remote access has just been turned off — because the whole point
// of that restart is to replace an argv that no longer matches the config. Two
// copies of this could disagree, and the restart would then re-apply the very
// flags it exists to drop.
func hubServerOpts(app *config.App, sessionSecret string) scion.ServerOpts {
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
// jail already holds this exact image (same ID; fail-open — ImageLoaded returns
// false on any doubt), otherwise load and then best-effort prune the superseded
// dangling image.
func runLoadImage(ctx context.Context, s Step, d Deps) error {
	if d.ImageLoaded(ctx, s.Target) {
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
		return r.d.StripProjectSharedDirs(ctx, path.Base(jp))
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
	return r.d.StripProjectSharedDirs(ctx, path.Base(jp))
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
// then act on the delta (create / no-op / resume / forced resume / loud
// recovery) and verify the container is actually live. The three unresumable
// tails share recoverDeleteAndCreate.
func (r *run) startManager(ctx context.Context, s Step) error {
	jp := JailPath(r.app.Tree, r.app.Tree, r.d.JailMount)
	// Read the prompt before any waiting: a missing or unreadable prompt file
	// is a config error and should fail fast, not after the broker-ready poll.
	task, err := r.managerTask()
	if err != nil {
		return err
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
	opts, err := r.managerStartOpts(ctx, jp, task)
	if err != nil {
		return err
	}
	rec, err := r.observeManager(ctx, jp)
	if err != nil {
		return err
	}
	if err := r.convergeManager(ctx, jp, rec, opts); err != nil {
		return err
	}
	return r.waitManagerLive(ctx, jp)
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
	return strings.TrimSpace(string(b)), nil
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
func (r *run) managerStartOpts(ctx context.Context, jp, task string) (scion.StartOpts, error) {
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
		// Workspace = the in-jail project tree, so the manager edits the real
		// host files in place (verified 2026-06-16). Without it scion mounts a
		// managed copy of the externalized config dir, not the live tree.
		Workspace: jp,
		// api-key: start with --harness-auth api-key (satisfied by the placeholder
		// secret set above); the real credential arrives in-container.
		APIKey: apiKey,
	}, nil
}

// convergeManager acts on the observed manager record (nil when absent): create,
// keep, resume, forced resume, or the loud delete+fresh recovery.
func (r *run) convergeManager(ctx context.Context, jp string, rec *scion.Agent, opts scion.StartOpts) error {
	switch {
	case rec == nil:
		return r.startManagerCreate(ctx, opts)
	case rec.Phase == scion.PhaseRunning:
		// No-op — the liveness verify in startManager still confirms the
		// container is actually up: a running RECORD with a dead container
		// must fail loudly, not silently pass.
		return nil
	case rec.Phase == scion.PhaseSuspended || rec.Phase == scion.PhaseStopped:
		// Resume rides the SAME runtime-broker-race retry as a create Start
		// (see scion.IsBrokerUnavailable's doc): on a cold VM the runtime broker may
		// not have re-registered with the hub yet, and resume hits that
		// identical transient window. Only once the retry budget is exhausted
		// (or the error is not the transient one at all) is the session
		// declared unrecoverable.
		return r.resumeOrRecover(ctx, jp, opts, resumeVerbPlain(func() error {
			return r.d.Scion.Resume(ctx, r.app.Name, jp)
		}))
	case rec.Phase == scion.PhaseError:
		// A crashed/wedged manager record. Since scion#895 (`resume
		// --force`, pin >= 68507153) the error phase IS recoverable — try
		// that first, with a fresh ticket staged (the leaf may have lapsed
		// while wedged; same rationale as the suspended branch), and only
		// discard the conversation when the forced resume itself fails.
		// Live motivation: 2026-07-31, an OrbStack VM reboot corrupted the
		// container state, resume failed, and the then-unconditional
		// delete+fresh destroyed the manager conversation (#3).
		return r.resumeOrRecover(ctx, jp, opts, resumeVerbForce(func() error {
			return r.d.Scion.ResumeForce(ctx, r.app.Name, jp)
		}))
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
		return r.recoverDeleteAndCreate(ctx, jp, opts,
			fmt.Sprintf("start-manager: manager %q in phase %q — deleting and starting FRESH (previous session lost)", r.app.Name, rec.Phase),
			fmt.Sprintf("manager in phase %q", rec.Phase))
	}
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
// The role gate only covers the phases convergeManager keeps the record in:
// rec == nil creates one, and its default branch deletes and recreates it,
// both of which stamp a role themselves. It returns rather than falling into
// recoverDeleteAndCreate on purpose. That recovery discards the conversation,
// and refusing here exists to give the operator the choice — losing the
// session is one of the two ways out, not something a guard may take on
// their behalf.
func (r *run) observeManager(ctx context.Context, jp string) (*scion.Agent, error) {
	agents, err := r.listAgentsRetry(ctx, jp)
	if err != nil {
		return nil, fmt.Errorf("start-manager: observing agents: %w", err)
	}
	rec := scion.FindAgent(agents, r.app.Name)
	if rec != nil {
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
// resume --force`. Each carries its own verbatim wording for the loud
// session-lost notice and the delete-failure clause (both take the resume
// error as their one %v).
type resumeVerb struct {
	label   string // verb name in the "recovered concurrently" log line
	lostFmt string // loud previous-session-lost line
	lostWhy string // delete-failure clause for recoverDeleteAndCreate
	resume  func() error
}

// resumeVerbPlain is the `scion resume` arm for a suspended/stopped record.
func resumeVerbPlain(run func() error) resumeVerb {
	return resumeVerb{
		label:   "resume",
		lostFmt: "start-manager: resume failed (%v) — deleting the manager record and starting FRESH (previous session lost)",
		lostWhy: "resume failed (%v)",
		resume:  run,
	}
}

// resumeVerbForce is the `scion resume --force` arm for an error record.
func resumeVerbForce(run func() error) resumeVerb {
	return resumeVerb{
		label:   "resume --force",
		lostFmt: "start-manager: manager in phase \"error\" and resume --force failed (%v) — deleting the manager record and starting FRESH (previous session lost)",
		lostWhy: "forced resume failed (%v)",
		resume:  run,
	}
}

// resumeOrRecover is the shared body of the two resumable arms. It stages
// fresh bootstrap material, runs the verb through the runtime-broker-race
// retry, and on failure either keeps a manager that recovered concurrently or
// takes the LOUD delete+fresh recovery.
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
func (r *run) resumeOrRecover(ctx context.Context, jp string, opts scion.StartOpts, v resumeVerb) error {
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
	// LOUD recovery: the conversation could not be restored. This MUST reach
	// the user — resume failing means the durable session (the whole point of
	// suspending, not stopping, at power-off; see internal/cli/host/stop.go) is about to be
	// discarded.
	return r.recoverDeleteAndCreate(ctx, jp, opts,
		fmt.Sprintf(v.lostFmt, rerr), fmt.Sprintf(v.lostWhy, rerr))
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
// lazy hub-sync can transiently read a live record as absent). Shared by the absent-record branch and the post-delete
// recovery branches above (a failed resume, or an unresumable phase, falls
// back to exactly this same create path), so all three take the identical
// retry behavior — including the bootstrap re-arm below, which is why it
// lives HERE rather than duplicated at each of the three call sites.
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
func (r *run) startManagerCreate(ctx context.Context, opts scion.StartOpts) error {
	if err := r.ensureFreshBootstrap(ctx); err != nil {
		return err
	}
	return r.retryOnBrokerUnavailable(ctx, func() error {
		startErr := r.d.Scion.Start(ctx, opts)
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
func (r *run) recoverDeleteAndCreate(ctx context.Context, jp string, opts scion.StartOpts, logMsg, deleteFailReason string) error {
	r.d.Log("%s", logMsg)
	if derr := r.d.Scion.Delete(ctx, r.app.Name, jp); derr != nil {
		return fmt.Errorf("start-manager: %s and delete failed: %w", deleteFailReason, derr)
	}
	return r.startManagerCreate(ctx, opts)
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
func (r *run) waitManagerLive(ctx context.Context, jp string) error {
	err := scion.WaitAgentLive(ctx, func(c context.Context) ([]scion.Agent, error) {
		return r.d.Scion.List(c, jp)
	}, r.app.Name, r.managerLive.Attempts, r.managerLive.Interval)
	if err == nil {
		return nil
	}
	// WaitAgentLive returns ctx.Err() unwrapped on cancellation; pass it through
	// as-is (no start-manager prefix) and prefix only the exhaustion error.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("start-manager: manager %q %w", r.app.Name, err)
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
