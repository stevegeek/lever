// Package backend defines the substrate contract every containment backend
// satisfies. The declared backends and their guarantees live in candidates.go
// (the single source of the guarantee matrix); construction is in
// internal/backend/registry.
package backend

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/exec"
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
	// ClosedInternet appends a catch-all OUTPUT DROP after the per-port ACCEPTs,
	// so the jail can reach ONLY the broker port on the host alias. Required for
	// api-key mode: LLM traffic must flow broker→Anthropic, not
	// jail→Anthropic directly. False (open posture) is the default for
	// subscription mode where the agent reaches Anthropic directly.
	ClosedInternet bool
}

// Backend is the contract the rest of Lever drives. Implementations must make
// EnsureUp idempotent. RunUser/RunUID/HostAliasV4/JailRunner are valid after
// EnsureUp (constructors may return sensible defaults before).
type Backend interface {
	EnsureUp(ctx context.Context, cfg Config) error
	DockerHost() string    // endpoint the broker drives (valid after EnsureUp)
	HostToolAlias() string // how an agent reaches allowlisted host tools ("" if none)
	HostAliasV4() string   // resolved IPv4 of HostToolAlias as seen from the jail ("" if unresolved)
	MountDest() string     // path inside the jail where the project tree is bind-mounted
	MachineName() string   // the jail identifier this backend targets
	RunUser() string       // the in-jail run user
	RunUID() string        // the in-jail run user's uid
	// ResolveRunUser resolves the in-machine run user/uid WITHOUT provisioning:
	// it errors if the machine is not already up. For passive verbs (attach) that
	// need the jail transport but must never create or configure the machine.
	ResolveRunUser(ctx context.Context) error
	JailRunner() exec.Runner            // command transport into the jail
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
	ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error
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
}
