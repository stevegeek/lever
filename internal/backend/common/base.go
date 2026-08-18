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
	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/jail"
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
	Guest func(r exec.Runner, machine string) guest.Guest
	// ResolveHostAlias resolves the host tool alias's (v4, v6) as seen from inside
	// the jail. Kept per-backend because each has a dedicated exact-argv test.
	ResolveHostAlias func(ctx context.Context) (v4, v6 string, err error)
}

// Base carries the shared jail state and implements the prefix/guest-delegating
// Backend methods once. Embedders set R, Machine, HostAlias, and Hooks in their
// constructor; EnsureUp fills User/UID (via ReadRunUser) and AliasV4/AliasV6
// (via ApplyEgress).
type Base struct {
	R         exec.Runner // host runner (the prefix binary runs on the host)
	Machine   string      // jail identifier
	HostAlias string      // DNS name an agent uses to reach allowlisted host tools

	User    string // resolved in-jail run user
	UID     string // resolved in-jail run-user uid
	AliasV4 string // resolved IPv4 of HostAlias as seen from the jail
	AliasV6 string // resolved IPv6 of HostAlias as seen from the jail

	Hooks Hooks
}

// Guest returns the shared guest provisioner scoped to this machine.
func (b *Base) Guest() guest.Guest { return b.Hooks.Guest(b.R, b.Machine) }

// jailPrefix is the in-jail transport argv prefix for the current run user.
func (b *Base) jailPrefix() []string { return b.Hooks.JailPrefix(b.Machine, b.User) }

// DockerHost is the rootless Docker socket endpoint the broker drives.
func (b *Base) DockerHost() string {
	return fmt.Sprintf("unix:///run/user/%s/docker.sock", b.RunUID())
}

// HostToolAlias is how an agent reaches allowlisted host tools.
func (b *Base) HostToolAlias() string { return b.HostAlias }

// HostAliasV4 returns the resolved IPv4 of HostToolAlias as seen from the jail,
// valid after EnsureUp/ApplyEgress. Empty if not yet resolved.
func (b *Base) HostAliasV4() string { return b.AliasV4 }

// MountDest returns the path inside the jail where the project tree is bind-mounted.
func (b *Base) MountDest() string { return MountDest }

// MachineName returns the jail machine name this backend targets.
func (b *Base) MachineName() string { return b.Machine }

// RunUser returns the in-jail run user resolved by EnsureUp (valid after EnsureUp).
func (b *Base) RunUser() string { return b.User }

// RunUID returns the in-jail run-user uid resolved by EnsureUp, falling back to
// DefaultRunUID if EnsureUp has not yet been called.
func (b *Base) RunUID() string {
	if b.UID == "" {
		return DefaultRunUID
	}
	return b.UID
}

// ReadRunUser caches the in-jail run user and UID (via the guest UserPrefix +
// whoami / id -u) so the subid/linger script and the rootless Docker socket path
// work for any guest user, not a hardcoded one. Called after the machine exists
// and before guest provisioning.
func (b *Base) ReadRunUser(ctx context.Context) error {
	up := b.Guest().UserPrefix
	whoami := append(append([]string{}, up[1:]...), "whoami")
	res, err := b.R.Run(ctx, nil, up[0], whoami...)
	if err != nil {
		return fmt.Errorf("resolve run user: %w", err)
	}
	b.User = strings.TrimSpace(res.Stdout)
	idu := append(append([]string{}, up[1:]...), "id", "-u")
	res, err = b.R.Run(ctx, nil, up[0], idu...)
	if err != nil {
		return fmt.Errorf("resolve run uid: %w", err)
	}
	b.UID = strings.TrimSpace(res.Stdout)
	return nil
}

// ApplyEgress applies the LEVER_EGRESS ruleset through the guest and records the
// resolved host alias, preserving the I2 no-reopen property.
func (b *Base) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	v4, v6, rebuilt, err := b.Guest().ApplyEgress(ctx, b.Hooks.ResolveHostAlias, allowedPorts, closedInternet)
	if err != nil {
		return err
	}
	if rebuilt {
		b.AliasV4, b.AliasV6 = v4, v6
	} else {
		// I2 skip path: v6 is not authoritative here (existingClosedAlias only
		// parses v4 from the live chain) — do not clobber a prior aliasV6.
		b.AliasV4 = v4
	}
	return nil
}

// JailRunner returns the command transport into the jail (valid after EnsureUp,
// which resolves the run user).
func (b *Base) JailRunner() exec.Runner {
	return jail.New(b.R, b.jailPrefix(), b.RunUID())
}

// AttachArgv builds the host argv for an interactive in-jail command.
func (b *Base) AttachArgv(inner []string) []string {
	return jail.AttachArgv(b.jailPrefix(), b.RunUID(), inner)
}

// LoadImage streams a host docker image into the jail's rootless podman.
func (b *Base) LoadImage(ctx context.Context, imageRef string) error {
	return jail.LoadImage(ctx, b.jailPrefix(), b.RunUID(), imageRef)
}

// ImageLoaded reports whether the jail already holds imageRef at the host's
// image ID (so a re-import can be skipped). Fail-open — see the interface doc.
func (b *Base) ImageLoaded(ctx context.Context, imageRef string) bool {
	return jail.ImageLoaded(ctx, b.R, b.jailPrefix(), b.RunUID(), imageRef)
}

// PruneJailImages reclaims dangling images from the jail's rootless podman.
func (b *Base) PruneJailImages(ctx context.Context) error {
	return jail.PruneImages(ctx, b.R, b.jailPrefix(), b.RunUID())
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

// DisableHubLogin converges the guest's remote-access login path off — see
// backend.Backend.
func (b *Base) DisableHubLogin(ctx context.Context) error {
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
