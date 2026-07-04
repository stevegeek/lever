package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
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
// fast. (Only the first start races; groves start later when the broker is ready.)
var (
	brokerStartAttempts = 30
	brokerStartInterval = 1 * time.Second
)

// isBrokerUnavailable reports whether err is the transient "runtime broker not
// yet registered" error from `scion start`/`scion resume` (the registration
// race), as opposed to a real failure. `scion resume` hits the SAME race as
// `scion start` (confirmed in scion source,
// pkg/hub/handlers_agent_create_helpers.go:354,408) but emits the singular
// "no runtime broker available" — distinct wording from start's plural "No
// runtime brokers available" — so both must be matched or a resume retry
// would never see its own transient error as retryable.
func isBrokerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no_runtime_broker") ||
		strings.Contains(s, "No runtime brokers available") ||
		strings.Contains(s, "no runtime broker available")
}

// apiKeyPlaceholder is the sentinel ANTHROPIC_API_KEY set as a Hub secret for
// api-key instances. It is NOT a real credential: it exists only to satisfy
// scion's start-time auth gate so the container (and lever-agent boot) can run.
// claude sends it as x-api-key to the broker /llm, which strips it and injects
// the real Console key host-side. Shaped like an Anthropic key (sk-ant- prefix,
// long) in case scion's auth resolution sanity-checks the format.
const apiKeyPlaceholder = "sk-ant-placeholder0lever0broker0injects0the0real0key0do0not0use000000000000000000000000"

// BootstrapMaterial is what the manager's lever-agent consumes to enrol.
type BootstrapMaterial struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

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
	dir := filepath.Join(treeDir, ".lever")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "bootstrap.json"), b, 0o600)
}

// Deps are the executor's collaborators, injected so Run is testable offline.
// JailUp/LoadImage are host-side (backend.EnsureUp, docker-save|podman-load);
// Scion runs IN the jail (built on a JailRunner).
type Deps struct {
	JailUp               func(ctx context.Context, app *config.App) error
	LoadImage            func(ctx context.Context, imageRef string) error
	Scion                *scion.Client
	ReadCred             func(path string) (string, error) // nil ⇒ defaultReadCred
	JailMount            string                            // jail path where app.Tree is bind-mounted (e.g. "/lever"); "" disables translation
	StartBroker          func(ctx context.Context) error
	BrokerHealthy        func(ctx context.Context) error
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
	// RemoveJailFile removes a regular file at a jail-absolute path, through the
	// jail's own filesystem view. Used for the stale `.scion` marker so the
	// removal and the subsequent in-jail `scion init` cannot race across the
	// host/guest VirtioFS boundary (a host-side unlink is not promptly visible
	// to the guest's directory cache). Must NOT remove directories. nil ⇒ fall
	// back to a host-side remove (tests, broker-only VM gate).
	RemoveJailFile func(ctx context.Context, jailPath string) error
	// RemoveScionProjectConfigs removes any stale ~/.scion/project-configs
	// registration(s) whose workspace_path == jailWorkspacePath, BEFORE the
	// register-manager/register-grove step re-inits. Without this, every apply
	// mints a fresh registration via `scion init` and the old ones accumulate
	// (the `lever doctor` "duplicate registrations" finding) — this is the
	// removal counterpart to RemoveJailFile's marker-race fix above. nil ⇒
	// no-op (tests, broker-only VM gate).
	RemoveScionProjectConfigs func(ctx context.Context, jailWorkspacePath string) error
	// ScionProjectRegistered observes whether jailWorkspacePath already has
	// exactly one valid scion registration (one project-configs entry + the
	// in-tree marker present) BEFORE the register-manager/register-grove step
	// decides whether to run its destructive clean+init path at all. true →
	// skip marker removal, RemoveScionProjectConfigs, and `scion init`/`hub
	// link` entirely, so a re-apply does not tear down (and orphan) a
	// resumable scion agent record just to re-mint an identical registration.
	// nil, or a query error, falls through to the destructive path unchanged
	// (fail-open — an observe failure must never turn into a hard apply
	// failure, and zero/duplicate/torn registrations legitimately need it).
	ScionProjectRegistered func(ctx context.Context, jailWorkspacePath string) (bool, error)
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
	return nil
}

func runStep(ctx context.Context, app *config.App, s Step, d Deps, boot *bootTracker) error {
	switch s.Kind {
	case "jail-up":
		return d.JailUp(ctx, app)
	case "broker-up":
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
	case "load-image":
		return d.LoadImage(ctx, s.Target)
	case "init-machine":
		return d.Scion.InitMachine(ctx)
	case "config-registry":
		return d.Scion.ConfigSetGlobal(ctx, "image_registry", "scionlocal")
	case "scion-server":
		return d.Scion.ServerStart(ctx)
	case "credential":
		read := d.ReadCred
		if read == nil {
			read = defaultReadCred
		}
		tok, err := read(s.Target)
		if err != nil {
			return fmt.Errorf("reading credential %s: %w", s.Target, err)
		}
		return d.Scion.SecretSet(ctx, "CLAUDE_CODE_OAUTH_TOKEN", tok)
	case "register-manager", "register-grove":
		jp := jailPath(s.Target, app.Tree, d.JailMount)

		// Idempotent register: observe BEFORE doing anything destructive. A
		// suspended manager (or grove) agent record survives a `lever stop` +
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
				return nil
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
		return d.Scion.HubLink(ctx, jp)
	case "mint-manager-bootstrap":
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
	case "start-manager":
		task := ""
		if p := app.ManagerPromptPath(); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("reading manager prompt %s: %w", p, err)
			}
			task = strings.TrimSpace(string(b))
		}
		jp := jailPath(app.Tree, app.Tree, d.JailMount)
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
			// once here; later-started groves inherit the same Hub secret.
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
			Grove: app.Name, Task: task, Project: jp, Image: app.Manager.Image, Harness: "claude",
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
		// 2026-07-04 (see the resume-reconciliation plan's Evidence base). The
		// hub is already up by this point in Plan() (scion-server runs before
		// start-manager), so a List error here is real, not a "hub not ready
		// yet" race — surface it as a hard step error.
		agents, lerr := d.Scion.List(ctx, jp)
		if lerr != nil {
			return fmt.Errorf("start-manager: observing agents: %w", lerr)
		}
		var rec *scion.Agent
		for i := range agents {
			if agents[i].Slug == app.Name {
				rec = &agents[i]
				break
			}
		}
		switch {
		case rec == nil:
			if err := startManagerCreate(ctx, d, boot, opts); err != nil {
				return err
			}
		case rec.Phase == "running":
			// No-op — fall through to the liveness verify below, which still
			// confirms the container is actually up: a running RECORD with a
			// dead container must fail loudly, not silently pass.
		case rec.Phase == "suspended" || rec.Phase == "stopped":
			// Resume rides the SAME runtime-broker-race retry as a create Start
			// (see isBrokerUnavailable's doc): on a cold VM the runtime broker may
			// not have re-registered with the hub yet, and resume hits that
			// identical transient window. Only once the retry budget is exhausted
			// (or the error is not the transient one at all) is the session
			// declared unrecoverable.
			if rerr := retryOnBrokerUnavailable(ctx, func() error {
				return d.Scion.Resume(ctx, app.Name, jp)
			}); rerr != nil {
				// LOUD recovery: the conversation could not be restored. This MUST
				// reach the user — resume failing means the durable session (the
				// whole point of suspending, not stopping, at power-off; see
				// cli/stop.go) is about to be discarded.
				logf(d, "start-manager: resume failed (%v) — deleting the manager record and starting FRESH (previous session lost)", rerr)
				if derr := d.Scion.Delete(ctx, app.Name, jp); derr != nil {
					return fmt.Errorf("start-manager: resume failed (%v) and delete failed: %w", rerr, derr)
				}
				if err := startManagerCreate(ctx, d, boot, opts); err != nil {
					return err
				}
			}
		default:
			// Any other phase — scion's full enum also has created,
			// provisioning, cloning, starting, stopping, and error (see
			// pkg/agent/state/state.go) — is not resumable: `scion resume` is
			// documented for suspended/stopped records only, and `scion list`'s
			// JSON phase field is the canonical (and only) signal we have, so we
			// cannot be cleverer here without more scion verbs (e.g. there is no
			// "wait for starting to settle" verb to poll instead). A crashed
			// manager (phase "error") or one caught mid-transition by an
			// interrupted prior `lever up` (phase "starting"/"created"/…) must
			// still let `up` converge, so this takes the SAME loud delete+fresh
			// recovery as a failed resume, rather than hard-failing (bricking)
			// the apply with no path forward but a hard `lever destroy`.
			logf(d, "start-manager: manager %q in phase %q — deleting and starting FRESH (previous session lost)", app.Name, rec.Phase)
			if derr := d.Scion.Delete(ctx, app.Name, jp); derr != nil {
				return fmt.Errorf("start-manager: manager in phase %q and delete failed: %w", rec.Phase, derr)
			}
			if err := startManagerCreate(ctx, d, boot, opts); err != nil {
				return err
			}
		}
		return waitManagerLive(ctx, d, jp, app.Name)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
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

// ensureFreshBootstrap guarantees fresh, enrolable bootstrap material exists
// before a create-path Start. If this apply run already minted fresh
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
// success meaningful.
// containerLive reports whether a scion `list --format json` containerStatus
// value describes a LIVE container. For a running container scion passes
// through the podman status TEXT ("Up 6 seconds", "Up About a minute"), not a
// canonical token — live-observed 2026-07-04 when the liveness gate wrongly
// failed a healthy manager by comparing == "running". Non-live values seen:
// "stopped", "Exited (1) 4 minutes ago".
func containerLive(status string) bool {
	return status == "running" || strings.HasPrefix(status, "Up")
}

func waitManagerLive(ctx context.Context, d Deps, jp, slug string) error {
	var lastPhase, lastContainer string
	var lastErr error
	for attempt := 0; attempt < managerLiveAttempts; attempt++ {
		agents, err := d.Scion.List(ctx, jp)
		if err != nil {
			// A mid-poll List blip does NOT mean the manager isn't live: by this
			// point the observe-first List already succeeded and the create/
			// resume action itself already succeeded, so the hub is demonstrably
			// up — a single error here is far more likely a transient hiccup than
			// a real failure. Consume this attempt (within the SAME overall
			// budget, not an extra one) and keep polling; only surface the error
			// if the whole budget exhausts without ever observing a live record.
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(managerLiveInterval):
			}
			continue
		}
		lastErr = nil
		lastPhase, lastContainer = "", ""
		for _, a := range agents {
			if a.Slug == slug {
				lastPhase, lastContainer = a.Phase, a.ContainerStatus
				break
			}
		}
		if lastPhase == "running" && containerLive(lastContainer) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(managerLiveInterval):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("start-manager: manager %q did not come up (last error observing agents: %w)", slug, lastErr)
	}
	return fmt.Errorf("start-manager: manager %q did not come up (last phase %q, container %q) — scion reported success but the harness is not live", slug, lastPhase, lastContainer)
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
// the credential projected into agent containers; see security-model.md §5.
func defaultReadCred(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o004 != 0 {
		return "", fmt.Errorf("credential file %s is world-readable (%#o) — restrict it to 0600", path, info.Mode().Perm())
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
