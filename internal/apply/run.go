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
// yet registered" error from `scion start` (the registration race), as opposed
// to a real failure.
func isBrokerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no_runtime_broker") || strings.Contains(s, "No runtime brokers available")
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
	var boot BootstrapMaterial
	for _, step := range Plan(app, PlanOpts{BrokerOnly: d.BrokerOnly}) {
		if err := runStep(ctx, app, step, d, &boot); err != nil {
			return fmt.Errorf("step %s: %w", step.Kind, err)
		}
	}
	return nil
}

func runStep(ctx context.Context, app *config.App, s Step, d Deps, boot *BootstrapMaterial) error {
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
		*boot = m
		// Deposit it as a 0600 file in the mount (the lever-agent reads it).
		dir := filepath.Join(s.Target, ".lever")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "bootstrap.json"), b, 0o600)
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
		// start CREATES (409 "already exists" over any existing record — and
		// the CLI exits 0 on that 409, so a blind start false-succeeds); resume
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
			if err := startManagerCreate(ctx, d, opts); err != nil {
				return err
			}
		case rec.Phase == "running":
			// No-op — fall through to the liveness verify below, which still
			// confirms the container is actually up: a running RECORD with a
			// dead container must fail loudly, not silently pass.
		case rec.Phase == "suspended" || rec.Phase == "stopped":
			if rerr := d.Scion.Resume(ctx, app.Name, jp); rerr != nil {
				// LOUD recovery: the conversation could not be restored. This MUST
				// reach the user — resume failing means the durable session (the
				// whole point of suspending, not stopping, at power-off; see
				// cli/stop.go) is about to be discarded.
				logf(d, "start-manager: resume failed (%v) — deleting the manager record and starting FRESH (previous session lost)", rerr)
				if derr := d.Scion.Delete(ctx, app.Name, jp); derr != nil {
					return fmt.Errorf("start-manager: resume failed (%v) and delete failed: %w", rerr, derr)
				}
				if err := startManagerCreate(ctx, d, opts); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("start-manager: manager %q in unexpected phase %q", app.Name, rec.Phase)
		}
		return waitManagerLive(ctx, d, jp, app.Name)
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// startManagerCreate runs the create-manager retry loop: `scion start` races
// the runtime-broker registration (see brokerStartAttempts) and treats an
// "already running"/"already exists" 409 as success (idempotent re-apply, or a
// create-race against a record the observe step just missed — scion's own
// lazy hub-sync can transiently read a live record as absent; see the plan's
// Evidence base). Shared by the absent-record branch and the post-delete
// recovery branch above (a failed resume falls back to exactly this same
// create path), so both take the identical retry behavior.
func startManagerCreate(ctx context.Context, d Deps, opts scion.StartOpts) error {
	var startErr error
	for attempt := 0; attempt < brokerStartAttempts; attempt++ {
		startErr = d.Scion.Start(ctx, opts)
		// Idempotent: a manager already running/existing (re-apply, or a
		// create-race the observe step missed) is success, not error.
		if startErr != nil && scion.AlreadyRunning(startErr) {
			return nil
		}
		if startErr == nil || !isBrokerUnavailable(startErr) {
			return startErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(brokerStartInterval):
		}
	}
	return startErr
}

// managerLiveAttempts/managerLiveInterval bound waitManagerLive's post-start
// poll. Package vars so tests shrink them.
var (
	managerLiveAttempts = 15
	managerLiveInterval = 1 * time.Second
)

// waitManagerLive polls d.Scion.List until slug's record shows BOTH
// Phase=="running" AND ContainerStatus=="running", or attempts run out. This
// is the backstop for both false-success classes scion's own CLI can report
// (see the plan's Evidence base): a blind `scion start` exits 0 on a 409
// "already exists" even when nothing changed, and `scion resume`/`scion
// start` can report success ("resumed") for a container that dies moments
// later. Trusting the observed record — not the CLI's own exit code/wording —
// is what makes start-manager's success meaningful.
func waitManagerLive(ctx context.Context, d Deps, jp, slug string) error {
	var lastPhase, lastContainer string
	for attempt := 0; attempt < managerLiveAttempts; attempt++ {
		agents, err := d.Scion.List(ctx, jp)
		if err != nil {
			return fmt.Errorf("start-manager: liveness check: observing agents: %w", err)
		}
		lastPhase, lastContainer = "", ""
		for _, a := range agents {
			if a.Slug == slug {
				lastPhase, lastContainer = a.Phase, a.ContainerStatus
				break
			}
		}
		if lastPhase == "running" && lastContainer == "running" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(managerLiveInterval):
		}
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
