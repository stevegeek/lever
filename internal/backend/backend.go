// Package backend defines the substrate contract every containment backend
// satisfies. Each implementation declares its own guarantees as a Profile;
// internal/backend/registry names the implemented backends and exposes the
// guarantee matrix derived from them.
//
// The plain data types the Backend interface carries live in the stdlib-only
// leaf internal/backend/types. backend never imports guest: the contract must
// not depend on one implementation's helper.
package backend

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// Profile declares what a backend actually guarantees, so the security posture
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
	// ScionSource is the host path to a scion source checkout to cross-compile
	// and install into the jail. Empty disables scion provisioning.
	ScionSource string
	// ScionVersion pins a scion module version/commit that the backend fetches
	// via the Go module system and cross-compiles into the jail. Mutually
	// exclusive with ScionSource. Empty = none.
	ScionVersion string
	// ScionBinary is a host path to an already-built linux scion binary,
	// installed as-is; it needs no Go toolchain, module cache or egress on the
	// host. Mutually exclusive with both. Empty = none.
	ScionBinary string
	// ScionWebUI also builds scion's SPA on the host and stages it into the
	// guest. Set from config.App.ScionWebAssets: the backend obeys it rather
	// than re-deriving it, so the flag passed to the hub and the staged assets
	// cannot disagree.
	ScionWebUI bool
	// ScionPrebuild, when set, is a host build of the configured scion started
	// before EnsureUp so it runs while the VM boots. It is used when the
	// guest's architecture matches; nil means the scion step builds.
	ScionPrebuild *scionbin.Prebuild
	// ClosedInternet appends a catch-all OUTPUT DROP after the per-port
	// ACCEPTs, so the jail can reach ONLY the allowed ports on the host alias.
	// Required for api-key mode: LLM traffic must flow broker→Anthropic, never
	// jail→Anthropic directly.
	ClosedInternet bool
	// Disk is the Lima guest disk size (e.g. "24GiB"). Empty selects the lima
	// package default. Ignored by OrbStack.
	Disk string
	// CPUs and Memory size the Lima guest (0 / "" = Lima default).
	// NestedVirt makes the guest /dev/kvm usable by the run user (Lima on a
	// Linux host); apply bind-mounts it into the manager container only.
	// All three are ignored by OrbStack; config load rejects them there.
	CPUs       int
	Memory     string
	NestedVirt bool
	// NestedVirtUnknown says the caller has no lever.yaml (`lever provision`):
	// EnsureUp leaves the guest's /dev/kvm setup as it is.
	NestedVirtUnknown bool
	// GuestKernel is the Lima guest kernel flavour to converge on ("hwe" or
	// "ga"). "" installs nothing and only reports a pending reboot. Lima only.
	GuestKernel string
	// GuestProfile is the Lima guest profile to converge on ("small" or
	// "standard"). "" leaves the guest as it is. Lima only.
	GuestProfile string
	// HWEFailStamp is the host path of the stamp kept when the HWE kernel
	// install failed in this VM (0600, keyed by the guest's machine id). While
	// it names the VM and egress is closed, bring-up warns instead of retrying
	// apt; an apply under the open posture retries and removes it on success.
	// "" keeps no stamp. Lima only.
	HWEFailStamp string
	// GuestLogDir is the host directory the Lima console and host-agent logs
	// are saved to before a VM start recreates them, and when a running VM's
	// console holds a kernel panic. "" saves none. Lima only.
	GuestLogDir string
	// Warn prints a user-facing line for advice that must not fail the
	// bring-up. Nil discards.
	Warn func(format string, args ...any)
}

// HasScion reports whether any scion mode is configured. Call sites must use
// this rather than testing the fields themselves, so a new mode cannot be
// silently skipped by a stale guard.
func (c Config) HasScion() bool {
	return c.ScionBinary != "" || c.ScionSource != "" || c.ScionVersion != ""
}

// Backend is the contract the rest of Lever drives. Implementations must make
// EnsureUp idempotent. RunUser/RunUID/HostAliasV4 are valid after EnsureUp.
//
// JailRunner is safe to call before EnsureUp and on a stopped machine: it only
// builds a runner from cached fields and does no I/O. Before EnsureUp the
// prefix defers to the jail's login user, the same user ReadRunUser resolves.
type Backend interface {
	EnsureUp(ctx context.Context, cfg Config) error
	HostToolAlias() string // how an agent reaches allowlisted host tools ("" if none)
	HostAliasV4() string   // resolved IPv4 of HostToolAlias as seen from the jail ("" if unresolved)
	MountDest() string     // path inside the jail where the project tree is bind-mounted
	RunUser() string       // the in-jail run user
	RunUID() string        // the in-jail run user's uid
	// ResolveRunUser resolves the in-machine run user/uid WITHOUT
	// provisioning: it errors if the machine is not already up. For passive
	// verbs that must never create or configure the machine.
	ResolveRunUser(ctx context.Context) error
	JailRunner() proc.Runner            // command transport into the jail
	AttachArgv(inner []string) []string // interactive TTY argv (lever up)
	LoadImage(ctx context.Context, imageRef string) error
	// ImageLoaded reports whether the jail already holds imageRef at the same
	// image ID as the host. Fail-open: false on any uncertainty, so a broken
	// check loads rather than wrongly skips.
	ImageLoaded(ctx context.Context, imageRef string) bool
	// LoadImageTar and ImageLoadedTar are the same pair for an image shipped
	// as a docker archive on the host; neither needs host docker. allowTag
	// (nil = no policy) is applied to every tag the archive carries before a
	// byte is streamed; see jail.LoadImageTar.
	LoadImageTar(ctx context.Context, imageRef, tarPath string, allowTag func(ref string) error) error
	ImageLoadedTar(ctx context.Context, imageRef, tarPath string) bool
	// PruneJailImages reclaims dangling images from the jail's grow-only
	// disk. Never touches a tagged or container-referenced image.
	PruneJailImages(ctx context.Context) error
	// WarmJailImage starts imageRef once, with `true` as its entrypoint, under
	// the user-namespace mapping scion starts agents with, so the first agent
	// start does not spend scion's start timeout on podman's id-mapped layer
	// copy. Fast when the copy already exists.
	WarmJailImage(ctx context.Context, imageRef string) error
	// InstallGuestBinary streams a host-local executable into the guest at
	// destPath as root, through the backend's root prefix.
	InstallGuestBinary(ctx context.Context, localPath, destPath string) error
	// EnsureHubAuth installs the hub's assertion public key root-owned in the
	// guest and converges the hub's auth block in ~/.scion/settings.yaml (the
	// running hub takes its auth settings from env; the file is for readers),
	// dropping any oidc_login block. It reports whether the hub must restart:
	// the key file changed or an oidc_login block was removed, both read only
	// at hub startup. See guest.EnsureHubAuth.
	EnsureHubAuth(ctx context.Context, spec types.HubAuth) (bool, error)
	// EnsureLeverTemplate creates lever's overlay agent template in the guest,
	// reporting whether it wrote anything. See guest.EnsureLeverTemplate.
	EnsureLeverTemplate(ctx context.Context) (bool, error)
	// EnsureScionTelemetry converges the top-level `telemetry:` block of the
	// guest's ~/.scion/settings.yaml: off=true writes `enabled: false`;
	// off=false removes only the exact block lever wrote. It reports whether
	// the file changed. See guest.EnsureScionTelemetry.
	EnsureScionTelemetry(ctx context.Context, off bool) (bool, error)
	Teardown(ctx context.Context) error
	// Stop powers the machine off but keeps its disk, unlike Teardown.
	// Idempotent; a stopped machine is resumed by a subsequent EnsureUp.
	Stop(ctx context.Context) error
	Profile() Profile
	// ReadScionProjectState reads scion's project-registration state from the
	// jail. Read-only; uses the machine-only guest transport, so it works
	// without EnsureUp as long as the machine is up.
	ReadScionProjectState(ctx context.Context) (types.ScionProjectState, error)
	// RemoveScionProjectConfigs removes any ~/.scion/project-configs
	// registration whose workspace_path == workspacePath, through the
	// machine-only guest transport. A no-op when none match.
	RemoveScionProjectConfigs(ctx context.Context, workspacePath string) error
	// ScionProjectRegistered reports whether workspacePath has EXACTLY ONE
	// valid scion registration: one project-configs entry for it AND the
	// in-tree marker workspacePath/.scion. Read-only, machine-only transport.
	ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error)
	// RepairScionHubEndpoint rewrites the hub endpoint recorded in the
	// registration(s) for workspacePath when it differs from endpoint. A
	// registration linked against a hub on another port keeps that endpoint,
	// since the register step skips re-init when the registration is sound.
	RepairScionHubEndpoint(ctx context.Context, workspacePath, endpoint string) error
}
