// Package backend defines the substrate contract every containment backend
// satisfies. The declared backends and their guarantees live in candidates.go
// (the single source of the guarantee matrix); construction is in
// internal/backend/registry.
//
// # Dependency direction
//
// This package is the CONTRACT, so it owns every data type the Backend
// interface names — including HubLogin (hublogin.go) and ScionProjectState
// (scionstate.go), which the guest helper (internal/backend/guest) fills in
// and consumes. guest imports backend for those types; backend never imports
// guest. The types could live in guest only if backend imported guest, which
// would make the contract depend on one implementation's helper — the wrong
// direction, not merely a cycle to dodge. common (the shared implementation
// base) imports both, as an implementation should.
package backend

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/proc"
)

// Profile DECLARES what a backend actually guarantees, so the security posture
// is explicit per backend rather than assumed.
type Profile struct {
	Name             string
	SeparateKernel   bool   // own kernel (VM) vs shared host-VM kernel
	FSBoundedBy      string // mechanism, e.g. "no-host-home + single bind mount"
	EgressEnforcedAt string // e.g. "jail netns iptables"
	VersionFragile   bool   // depends on vendor behaviours that may change
}

func (p Profile) Summary() string {
	return fmt.Sprintf("%s [separate-kernel=%t fs=%s egress=%s version-fragile=%t]",
		p.Name, p.SeparateKernel, p.FSBoundedBy, p.EgressEnforcedAt, p.VersionFragile)
}

// Config is the instance-supplied input to bring a jail up.
type Config struct {
	MachineName  string // jail identifier
	ProjectTree  string // host path bind-mounted as the ONLY visible tree
	AllowedPorts []int  // host-loopback tool ports to allow via the host alias
	// ScionSource is the host path to a scion source checkout to cross-compile and
	// install into the jail. Empty disables scion provisioning (back-compat).
	ScionSource string
	// ScionVersion pins a scion module version/commit that the backend fetches
	// via the Go module system and cross-compiles into the jail (no vendored
	// source tree). Mutually exclusive with ScionSource. Empty = none.
	ScionVersion string
	// ScionBinary is a host path to an already-built linux scion binary,
	// installed into the guest as-is. Unlike ScionSource and ScionVersion it
	// needs no Go toolchain, module cache or egress on the machine hosting the
	// jail. Mutually exclusive with both. Empty = none.
	ScionBinary string
	// ScionWebUI additionally builds scion's SPA on the host and stages it into
	// the guest, so a `version:`/`source:` scion can serve the web UI. Off by
	// default: the assets need node+npm on the host and are dead weight for an
	// instance that never serves a UI. Set from config.App.ScionWebAssets — the
	// backend obeys it rather than re-deriving it, so the flag lever passes to
	// the hub and the assets lever staged cannot disagree.
	ScionWebUI bool
	// ClosedInternet appends a catch-all OUTPUT DROP after the per-port ACCEPTs,
	// so the jail can reach ONLY the broker port on the host alias. Required for
	// api-key mode: LLM traffic must flow broker→Anthropic, not
	// jail→Anthropic directly. False (open posture) is the default for
	// subscription mode where the agent reaches Anthropic directly.
	ClosedInternet bool
	// Disk is the Lima guest disk size (e.g. "24GiB"). Empty selects the
	// lima package default. Ignored by backends that manage their own disk
	// (OrbStack).
	Disk string
}

// HasScion reports whether any scion mode is configured.
//
// A predicate, not a hand-written condition at each call site: the three fields
// are checked in two backends, and when ScionBinary was added the struct literal
// was updated while the guard around it was not — so binary mode silently never
// ran. One place to change is the fix for that class.
func (c Config) HasScion() bool {
	return c.ScionBinary != "" || c.ScionSource != "" || c.ScionVersion != ""
}

// Backend is the contract the rest of Lever drives. Implementations must make
// EnsureUp idempotent. RunUser/RunUID/HostAliasV4/JailRunner are valid after
// EnsureUp (constructors may return sensible defaults before).
//
// JailRunner is the exception: it is safe to call BEFORE EnsureUp and on a
// stopped machine, because it only builds a runner from cached fields and does
// no I/O. Before EnsureUp the run user is unresolved, so the prefix defers to
// the jail's login user — the same user ReadRunUser resolves. buildApplyDeps
// and doctor both rely on that.
type Backend interface {
	EnsureUp(ctx context.Context, cfg Config) error
	DockerHost() string    // endpoint the broker drives (valid after EnsureUp)
	HostToolAlias() string // how an agent reaches allowlisted host tools ("" if none)
	HostAliasV4() string   // resolved IPv4 of HostToolAlias as seen from the jail ("" if unresolved)
	MountDest() string     // path inside the jail where the project tree is bind-mounted
	RunUser() string       // the in-jail run user
	RunUID() string        // the in-jail run user's uid
	// ResolveRunUser resolves the in-machine run user/uid WITHOUT provisioning:
	// it errors if the machine is not already up. For passive verbs (attach) that
	// need the jail transport but must never create or configure the machine.
	ResolveRunUser(ctx context.Context) error
	JailRunner() proc.Runner            // command transport into the jail
	AttachArgv(inner []string) []string // interactive TTY argv (lever up)
	LoadImage(ctx context.Context, imageRef string) error
	// ImageLoaded reports whether the jail already holds imageRef at the same
	// image ID as the host, so apply can skip a redundant multi-GB re-import.
	// Fail-open: false on any uncertainty (a not-yet-loaded or rebuilt image, or
	// an inspect failure) so a broken check loads rather than wrongly skips.
	ImageLoaded(ctx context.Context, imageRef string) bool
	// PruneJailImages reclaims dangling (untagged, unreferenced) images from the
	// jail's container store — the layers a rebuilt tag orphans on the grow-only
	// jail disk. Never touches a tagged or container-referenced image.
	PruneJailImages(ctx context.Context) error
	// InstallGuestBinary streams a host-local executable into the guest at
	// destPath as root (used by the acceptance gate to place lever-agent). The
	// transport is the backend's root prefix, so callers stay backend-agnostic.
	InstallGuestBinary(ctx context.Context, localPath, destPath string) error
	// EnsureHubLogin provisions the guest half of the remote-access login
	// path: the loopback forwarder that lets the hub see lever's host-side
	// OIDC provider as a LOCAL issuer (scion refuses to start on any other
	// kind), and the `oidc_login` block in the guest's ~/.scion/settings.yaml.
	//
	// It reports whether the hub's configuration changed. scion reads that
	// file once, at startup, so a change means a running hub is still serving
	// the old configuration and the caller must restart it — while an
	// unchanged one restarts nothing, which is what stops a re-apply from
	// bouncing the hub and every agent's connection to it.
	EnsureHubLogin(ctx context.Context, spec HubLogin) (bool, error)
	// EnsureLeverTemplate creates lever's overlay agent template in the guest,
	// reporting whether it wrote anything. The overlay exists to suppress
	// scion's stock placeholder system prompt, which would otherwise REPLACE
	// Claude Code's built-in one for every agent provisioned from the default
	// template. See guest.EnsureLeverTemplate for the full reasoning.
	EnsureLeverTemplate(ctx context.Context) (bool, error)
	// DisableHubLogin converges that provisioning OFF: it stops and removes
	// the guest-side forwarder and drops the `oidc_login` block. Called
	// whenever remote access is not enabled, so an instance that turned it
	// back off does not keep an unauthenticated jail→host loopback bridge
	// running for a feature that is gone. Idempotent, and cheap when there is
	// nothing to remove.
	//
	// Like EnsureHubLogin it reports whether the HUB's configuration changed —
	// here, whether the `oidc_login` block was still there to remove. A hub
	// that is already running read that block at startup and is still
	// advertising the login, so the caller restarts it. The forwarder is not
	// part of the answer: no hub ever reads it, and a restart is too expensive
	// to spend on a change the hub cannot see. See guest.DisableHubLogin.
	DisableHubLogin(ctx context.Context) (bool, error)
	Teardown(ctx context.Context) error
	// Stop powers the machine off but keeps its disk intact — distinct from
	// Teardown, which deletes the machine. Idempotent: a no-op if the machine
	// is already absent, and harmless if it is already stopped. A stopped
	// machine is resumed (not recreated) by a subsequent EnsureUp.
	Stop(ctx context.Context) error
	Profile() Profile
	// ReadScionProjectState reads scion's project-registration state from the
	// jail (the in-tree marker + ~/.scion/project-configs entries) for `lever
	// doctor`. Read-only; uses the machine-only guest transport, so it works
	// without EnsureUp as long as the jail machine is up.
	ReadScionProjectState(ctx context.Context) (ScionProjectState, error)
	// RemoveScionProjectConfigs removes any stale ~/.scion/project-configs
	// registration(s) whose workspace_path == workspacePath, through the
	// machine-only guest transport. A no-op when none match. Called before
	// `scion init` in the register-project apply step so each
	// apply leaves exactly one registration per workspace instead of
	// accumulating a duplicate every run (the `lever doctor` "duplicate
	// registrations" finding).
	RemoveScionProjectConfigs(ctx context.Context, workspacePath string) error
	// ScionProjectRegistered reports whether workspacePath already has EXACTLY
	// ONE valid scion registration: one ~/.scion/project-configs entry whose
	// workspace_path == workspacePath AND the in-tree marker
	// (workspacePath/.scion) present. Read-only, machine-only guest transport
	// (no EnsureUp needed) — same pattern as ReadScionProjectState. The
	// register-project apply step uses this to skip its
	// destructive clean+init path when the registration is already sound, so a
	// re-apply no longer tears down a resumable scion agent record just to
	// re-mint an identical registration.
	ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error)
	// RepairScionHubEndpoint rewrites the hub endpoint recorded in the
	// project-config registration(s) for workspacePath when it differs from
	// endpoint. Minting the controller PAT links the project against a
	// THROWAWAY hub on its own port, and the register step skips its re-init
	// when the registration is already sound — so a re-mint would otherwise
	// leave the project pointing at a hub that no longer exists. Only lever's
	// own calls pass an explicit endpoint, so the damage lands on anything
	// running scion bare in the jail (`lever attach` was the live case).
	RepairScionHubEndpoint(ctx context.Context, workspacePath, endpoint string) error
}
