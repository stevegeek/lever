// Package common holds the machinery shared by every "reach the guest via a
// host-side argv prefix" backend (orbstack, lima). Embed Base and set its
// Hooks in the constructor: Base implements the prefix/guest-delegating half of
// the backend.Backend contract once, so a new prefix-reached backend supplies
// only the parts that genuinely differ (version preflight, machine create/
// start/stop, and the two prefix/guest hooks). See internal/backend for the
// contract these compose and internal/backend/{orbstack,lima} for the embedders.
package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

const (
	// MountDest is the path inside the jail where the project tree is bind-
	// mounted (via `orb create --mount <host>:/lever` or the lima template).
	// Agents work exclusively within this directory; no host home is visible.
	MountDest = "/lever"
	// DefaultRunUID is the fallback UID used for the rootless Docker socket path
	// (/run/user/<uid>/docker.sock) before EnsureUp has resolved the real UID.
	// Both macOS hosts and the OrbStack/Lima guest users map to 501 by default,
	// so DockerHost() is sensible even before EnsureUp per the interface's "valid
	// after EnsureUp" contract.
	DefaultRunUID = "501"
)

// Hooks inject the per-backend variance the shared Base cannot derive from its
// own state. Both are set once at construction and are pure functions of the
// backend's runner/machine (no captured mutable state), so Base stays the sole
// owner of the run user/uid and resolved aliases.
type Hooks struct {
	// JailPrefix returns the in-jail transport argv prefix. orbstack's depends on
	// the resolved run user (`orb -m <m> -u <user>`); lima's is static
	// (`limactl shell <vm>`), hence a hook rather than embedding-dispatch.
	JailPrefix func(machine, runUser string) []string
	// Guest returns the guest provisioner scoped to (r, machine); its User/Root
	// prefixes differ per backend (orb has a distinct `-u root` form; lima appends
	// `sudo`).
	Guest func(r proc.Runner, machine string) guest.Guest
	// ResolveHostAlias resolves the host tool alias's (v4, v6) as seen from inside
	// the jail. Kept per-backend because each has a dedicated exact-argv test.
	ResolveHostAlias func(ctx context.Context) (v4, v6 string, err error)
}

// Options is the caller-supplied part of a backend's construction — what the
// registry decides once and every embedder passes through to NewBase.
type Options struct {
	// ForceHostNetwork is the host-side debugging escape hatch
	// (jail.ForceHostNetworkEnv) that makes every in-jail scion command fall
	// back to a shared netns. The registry reads it from the environment;
	// Base never consults the environment itself.
	ForceHostNetwork bool
}

// Config is what a Base needs from its embedder, set once at construction.
type Config struct {
	Runner    proc.Runner // host runner (the prefix binary runs on the host)
	Machine   string      // jail identifier
	HostAlias string      // DNS name an agent uses to reach allowlisted host tools
	Hooks     Hooks
	Options   Options
}

// Base carries the shared jail state and implements the prefix/guest-delegating
// Backend methods once. Embedders build it with NewBase; EnsureUp fills the run
// user/uid (via ReadRunUser) and the resolved aliases (via ApplyEgress). The
// resolved state is unexported so only those two paths can change it.
type Base struct {
	r         proc.Runner
	machine   string
	hostAlias string
	hooks     Hooks
	opts      Options

	user    string // resolved in-jail run user
	uid     string // resolved in-jail run-user uid
	aliasV4 string // resolved IPv4 of hostAlias as seen from the jail
}

// NewBase builds the shared half of a prefix-reached backend.
func NewBase(cfg Config) Base {
	return Base{r: cfg.Runner, machine: cfg.Machine, hostAlias: cfg.HostAlias, hooks: cfg.Hooks, opts: cfg.Options}
}

// Runner returns the host runner the backend was built with.
func (b *Base) Runner() proc.Runner { return b.r }

// Machine returns the jail machine name this backend targets.
func (b *Base) Machine() string { return b.machine }

// Guest returns the shared guest provisioner scoped to this machine.
func (b *Base) Guest() guest.Guest { return b.hooks.Guest(b.r, b.machine) }

// jailPrefix is the in-jail transport argv prefix for the current run user.
func (b *Base) jailPrefix() []string { return b.hooks.JailPrefix(b.machine, b.user) }

// DockerHost is the rootless Docker socket endpoint the broker drives.
func (b *Base) DockerHost() string {
	return fmt.Sprintf("unix:///run/user/%s/docker.sock", b.RunUID())
}

// HostToolAlias is how an agent reaches allowlisted host tools.
func (b *Base) HostToolAlias() string { return b.hostAlias }

// HostAliasV4 returns the resolved IPv4 of HostToolAlias as seen from the jail,
// valid after EnsureUp/ApplyEgress. Empty if not yet resolved.
func (b *Base) HostAliasV4() string { return b.aliasV4 }

// MountDest returns the path inside the jail where the project tree is bind-mounted.
func (b *Base) MountDest() string { return MountDest }

// RunUser returns the in-jail run user resolved by EnsureUp (valid after EnsureUp).
func (b *Base) RunUser() string { return b.user }

// RunUID returns the in-jail run-user uid resolved by EnsureUp, falling back to
// DefaultRunUID if EnsureUp has not yet been called.
func (b *Base) RunUID() string {
	if b.uid == "" {
		return DefaultRunUID
	}
	return b.uid
}

// ReadRunUser caches the in-jail run user and UID (via the guest UserPrefix +
// whoami / id -u) so the subid/linger script and the rootless Docker socket path
// work for any guest user, not a hardcoded one. Called after the machine exists
// and before guest provisioning.
func (b *Base) ReadRunUser(ctx context.Context) error {
	g := b.Guest()
	res, err := g.UserRun(ctx, "whoami")
	if err != nil {
		return fmt.Errorf("resolve run user: %w", err)
	}
	b.user = strings.TrimSpace(res.Stdout)
	res, err = g.UserRun(ctx, "id", "-u")
	if err != nil {
		return fmt.Errorf("resolve run uid: %w", err)
	}
	b.uid = strings.TrimSpace(res.Stdout)
	return nil
}

// Provision is the shared tail of every embedder's EnsureUp, run once the
// machine exists: resolve the run user, install the guest runtimes, install
// scion when cfg asks for one, then apply egress. One copy rather than one
// per backend: the ScionSpec literal drifted between the two once (ScionBinary
// was added to both literals while the guard around them was updated in
// neither — see backend.Config.HasScion).
func (b *Base) Provision(ctx context.Context, cfg backend.Config) error {
	if err := b.ReadRunUser(ctx); err != nil {
		return err
	}
	if err := b.Guest().EnsureRuntimes(ctx, b.RunUser()); err != nil {
		return err
	}
	if cfg.HasScion() {
		if err := b.Guest().EnsureScion(ctx, guest.ScionSpec{
			Binary:  cfg.ScionBinary,
			Source:  cfg.ScionSource,
			Version: cfg.ScionVersion,
			WebUI:   cfg.ScionWebUI,
		}); err != nil {
			return err
		}
	}
	return b.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
}

// ApplyEgress applies the LEVER_EGRESS ruleset through the guest and records the
// resolved host alias, preserving the I2 no-reopen property. Called by each
// embedder's EnsureUp as its last step; it is not part of the Backend
// contract because nothing outside a backend drives it. Only the IPv4 alias
// is kept: nothing reads the v6 one back, and the I2 skip path cannot supply
// it (existingClosedAlias parses only v4 from the live chain).
func (b *Base) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	v4, _, _, err := b.Guest().ApplyEgress(ctx, b.hooks.ResolveHostAlias, allowedPorts, closedInternet)
	if err != nil {
		return err
	}
	b.aliasV4 = v4
	return nil
}

// JailRunner returns the command transport into the jail (valid after EnsureUp,
// which resolves the run user).
func (b *Base) JailRunner() proc.Runner {
	return b.jail()
}

// jail builds the transport for the current run user.
func (b *Base) jail() *jail.Runner {
	return jail.New(jail.Config{
		Host:             b.r,
		Prefix:           b.jailPrefix(),
		UID:              b.RunUID(),
		ForceHostNetwork: b.opts.ForceHostNetwork,
	})
}

// AttachArgv builds the host argv for an interactive in-jail command.
func (b *Base) AttachArgv(inner []string) []string {
	return b.jail().AttachArgv(inner)
}

// LoadImage streams a host docker image into the jail's rootless podman.
func (b *Base) LoadImage(ctx context.Context, imageRef string) error {
	return jail.LoadImage(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef)
}

// ImageLoaded reports whether the jail already holds imageRef at the host's
// image ID (so a re-import can be skipped). Fail-open — see the interface doc.
func (b *Base) ImageLoaded(ctx context.Context, imageRef string) bool {
	return jail.ImageLoaded(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef)
}

// PruneJailImages reclaims dangling images from the jail's rootless podman.
func (b *Base) PruneJailImages(ctx context.Context) error {
	return jail.PruneImages(ctx, b.r, b.jailPrefix(), b.RunUID())
}

// InstallGuestBinary streams a host-local executable into the machine at
// destPath as root, via the shared guest provisioner (RootPrefix).
func (b *Base) InstallGuestBinary(ctx context.Context, localPath, destPath string) error {
	return b.Guest().InstallRootBinary(ctx, localPath, destPath)
}

// EnsureHubLogin provisions the guest half of the remote-access login path
// (loopback forwarder + the hub's oidc_login block), reporting whether the
// hub's configuration changed — see backend.Backend.
func (b *Base) EnsureHubLogin(ctx context.Context, spec backend.HubLogin) (bool, error) {
	return b.Guest().EnsureHubLogin(ctx, spec)
}

// EnsureLeverTemplate creates lever's overlay agent template in the guest —
// see backend.Backend.
func (b *Base) EnsureLeverTemplate(ctx context.Context) (bool, error) {
	return b.Guest().EnsureLeverTemplate(ctx)
}

// DisableHubLogin converges the guest's remote-access login path off,
// reporting whether the hub's configuration changed — see backend.Backend.
func (b *Base) DisableHubLogin(ctx context.Context) (bool, error) {
	return b.Guest().DisableHubLogin(ctx)
}

// ReadScionProjectState reads scion's registration state from the machine for
// `lever doctor` (in-tree marker + ~/.scion/project-configs). Read-only via the
// machine-only guest prefix, so it needs no EnsureUp.
func (b *Base) ReadScionProjectState(ctx context.Context) (backend.ScionProjectState, error) {
	return b.Guest().ReadScionProjectState(ctx, MountDest)
}

// RemoveScionProjectConfigs removes stale scion project-config registrations for
// wp from the machine, via the machine-only guest prefix.
func (b *Base) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	return b.Guest().RemoveScionProjectConfigs(ctx, wp)
}

// RepairScionHubEndpoint rewrites a stale hub endpoint in the project-config
// registration(s) for wp — see backend.Backend for why one can be stale.
func (b *Base) RepairScionHubEndpoint(ctx context.Context, wp, endpoint string) error {
	return b.Guest().RepairScionHubEndpoint(ctx, wp, endpoint)
}

// ScionProjectRegistered reports whether workspacePath already has exactly one
// valid scion registration, via the machine-only guest prefix. Read-only, no
// EnsureUp.
func (b *Base) ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error) {
	return b.Guest().ScionProjectRegistered(ctx, workspacePath)
}
