// Package common holds the machinery shared by every backend that reaches the
// guest via a host-side argv prefix (orbstack, lima). Embed Base and set its
// Hooks in the constructor; the embedder supplies only what differs (version
// preflight, machine lifecycle, and the hooks).
package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
)

const (
	// MountDest is the path inside the jail where the project tree is
	// bind-mounted. No host home is visible.
	MountDest = "/lever"
	// DefaultRunUID is the fallback run-user UID before EnsureUp has resolved
	// the real one. It matches an OrbStack guest user (501) but NOT a Lima one
	// (1000): every command that reaches into the jail must call EnsureUp or
	// ResolveRunUser first, or its XDG_RUNTIME_DIR is wrong on Lima.
	DefaultRunUID = "501"
)

// Hooks inject the per-backend variance. They are set once at construction and
// must capture no mutable state, so Base stays the sole owner of the run
// user/uid and resolved aliases.
type Hooks struct {
	// JailPrefix returns the in-jail transport argv prefix.
	JailPrefix func(machine, runUser string) []string
	// Guest returns the guest provisioner scoped to (r, machine).
	Guest func(r proc.Runner, machine string) guest.Guest
	// ResolveHostAlias resolves the host tool alias's (v4, v6) as seen from
	// inside the jail.
	ResolveHostAlias func(ctx context.Context) (v4, v6 string, err error)
	// DNSForward returns the guest resolver's DNAT targets on the host alias,
	// ACCEPTed in the open posture so DNS survives the alias DROP. nil when
	// the backend's resolver path is not routed through the alias.
	DNSForward func(ctx context.Context, aliasV4 string) ([]egress.DNSForward, error)
	// DNSProbe computes DNSForward's answer from the warm-path probe. nil with
	// a DNSForward set means the open posture always takes the full egress
	// path.
	DNSProbe *guest.DNSProbe
}

// Options is the caller-supplied part of a backend's construction.
type Options struct {
	// ForceHostNetwork is the debugging escape hatch (jail.ForceHostNetworkEnv)
	// that makes every in-jail scion command use a shared netns. Base never
	// consults the environment itself.
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

// Base carries the shared jail state and implements the shared Backend
// methods. The resolved run user/uid and aliases are unexported so only
// ReadRunUser, Provision and ApplyEgress can change them.
type Base struct {
	r         proc.Runner
	machine   string
	hostAlias string
	hooks     Hooks
	opts      Options

	user    string // resolved in-jail run user
	uid     string // resolved in-jail run-user uid
	aliasV4 string // resolved IPv4 of hostAlias as seen from the jail

	// warm is the probe of the last Provision when its warm path held; nil
	// otherwise. See WarmSnapshot.
	warm *guest.Snapshot
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

func (b *Base) jailPrefix() []string { return b.hooks.JailPrefix(b.machine, b.user) }

// HostToolAlias is how an agent reaches allowlisted host tools.
func (b *Base) HostToolAlias() string { return b.hostAlias }

// HostAliasV4 is valid after EnsureUp; empty if not yet resolved.
func (b *Base) HostAliasV4() string { return b.aliasV4 }

func (b *Base) MountDest() string { return MountDest }

// RunUser is valid after EnsureUp.
func (b *Base) RunUser() string { return b.user }

// RunUID falls back to DefaultRunUID before EnsureUp.
func (b *Base) RunUID() string {
	if b.uid == "" {
		return DefaultRunUID
	}
	return b.uid
}

// ReadRunUser caches the in-jail run user and UID. Call it after the machine
// exists and before guest provisioning.
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
// scion when cfg asks for one, then apply egress.
//
// It starts with the warm-path probe (guest.Probe). When the provisioning
// stamp matches, the runtimes are left alone and the egress chains are
// VERIFIED against the exact ruleset and left alone only when they match.
// When the stamp does not match (a new guest, a reboot, changed provisioning
// scripts) or the probe fails, every step takes its full path, and a
// successful provisioning writes the stamp.
func (b *Base) Provision(ctx context.Context, cfg backend.Config) error {
	rep := progress.From(ctx)
	g := b.Guest()
	b.warm = nil
	s := rep.Start("guest state")
	snap, perr := g.Probe(ctx, b.probeSpec(cfg))
	if perr != nil {
		snap = nil
		s.Note("unread; provisioning in full")
	}
	s.Done(nil)

	s = rep.Start("packages and runtimes")
	var err error
	if user, uid, ok := snap.RunUser(); ok {
		b.user, b.uid = user, uid
	} else {
		snap = nil
		err = b.ReadRunUser(ctx)
	}
	if err == nil {
		key := guest.ProvisionStampKey(b.RunUser())
		if snap != nil && snap.Provisioned(key) {
			s.Note("already provisioned")
		} else {
			// Something may change below the probe now: nothing else of
			// it is used.
			snap = nil
			err = g.EnsureRuntimes(ctx, b.RunUser())
			if err == nil {
				// Best-effort: a missing stamp costs the next bring-up a
				// full provisioning, nothing more.
				_ = g.WriteProvisionStamp(ctx, key)
			}
		}
	}
	s.Done(err)
	if err != nil {
		return err
	}
	if cfg.HasScion() {
		s = rep.Start(scionStepLabel(cfg))
		err := g.EnsureScionFrom(ctx, guest.ScionSpec{
			Binary:   cfg.ScionBinary,
			Source:   cfg.ScionSource,
			Version:  cfg.ScionVersion,
			WebUI:    cfg.ScionWebUI,
			Prebuilt: cfg.ScionPrebuild,
		}, snap)
		s.Done(err)
		if err != nil {
			return err
		}
	}
	s = rep.Start("egress firewall")
	if cfg.HasScion() {
		// The scion step can take minutes (a build): judge the chains as
		// they are now, not as the probe found them before it.
		g.RefreshEgress(ctx, snap)
	}
	if v4, ok := snap.VerifyEgress(cfg.AllowedPorts, cfg.ClosedInternet, b.hooks.DNSForward != nil, b.hooks.DNSProbe); ok {
		b.aliasV4 = v4
		s.Note("verified, unchanged")
		s.Done(nil)
	} else {
		err = b.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
		s.Done(err)
		if err != nil {
			return err
		}
	}
	b.warm = snap
	return nil
}

// probeSpec looks up the alias in the open posture only: the closed chain
// blocks DNS, and its alias is read from the chain instead.
func (b *Base) probeSpec(cfg backend.Config) guest.ProbeSpec {
	var spec guest.ProbeSpec
	if !cfg.ClosedInternet {
		spec.HostAlias = b.hostAlias
		if p := b.hooks.DNSProbe; p != nil && b.hooks.DNSForward != nil {
			spec.DNSChain, spec.ResolvConf = p.NATChain, p.ResolvConf
		}
	}
	if cfg.HasScion() {
		spec.HashPaths = []string{guest.ScionProbePath}
	}
	return spec
}

// WarmSnapshot is the probe of the last Provision when its warm path held,
// for an embedder's own later steps; nil when Provision took the full path,
// in which case those steps must take it too.
func (b *Base) WarmSnapshot() *guest.Snapshot { return b.warm }

func scionStepLabel(cfg backend.Config) string {
	switch {
	case cfg.ScionBinary != "":
		return "scion (prebuilt binary)"
	case len(cfg.ScionVersion) > 12:
		return "scion " + cfg.ScionVersion[:12]
	case cfg.ScionVersion != "":
		return "scion " + cfg.ScionVersion
	}
	return "scion"
}

// ApplyEgress applies the LEVER_EGRESS ruleset through the guest and records
// the resolved host alias, preserving the I2 no-reopen property. Only the IPv4
// alias is kept: the I2 skip path parses only v4 from the live chain.
func (b *Base) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	v4, _, _, err := b.Guest().ApplyEgress(ctx, b.hooks.ResolveHostAlias, b.hooks.DNSForward, allowedPorts, closedInternet)
	if err != nil {
		return err
	}
	b.aliasV4 = v4
	return nil
}

// JailRunner returns the command transport into the jail; see
// backend.Backend for its use before EnsureUp.
func (b *Base) JailRunner() proc.Runner {
	return b.jail()
}

func (b *Base) jail() *jail.Runner {
	return jail.New(jail.Config{
		Host:             b.r,
		Prefix:           b.jailPrefix(),
		UID:              b.RunUID(),
		ForceHostNetwork: b.opts.ForceHostNetwork,
	})
}

func (b *Base) AttachArgv(inner []string) []string {
	return b.jail().AttachArgv(inner)
}

func (b *Base) LoadImage(ctx context.Context, imageRef string) error {
	return jail.LoadImage(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef)
}

// ImageLoaded is fail-open; see backend.Backend.
func (b *Base) ImageLoaded(ctx context.Context, imageRef string) bool {
	return jail.ImageLoaded(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef)
}

func (b *Base) LoadImageTar(ctx context.Context, imageRef, tarPath string, allowTag func(ref string) error) error {
	return jail.LoadImageTar(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef, tarPath, allowTag)
}

// ImageLoadedTar is fail-open like ImageLoaded.
func (b *Base) ImageLoadedTar(ctx context.Context, imageRef, tarPath string) bool {
	return jail.ImageLoadedTar(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef, tarPath)
}

func (b *Base) PruneJailImages(ctx context.Context) error {
	return jail.PruneImages(ctx, b.r, b.jailPrefix(), b.RunUID())
}

func (b *Base) WarmJailImage(ctx context.Context, imageRef string) error {
	return jail.WarmImage(ctx, b.r, b.jailPrefix(), b.RunUID(), imageRef)
}

func (b *Base) InstallGuestBinary(ctx context.Context, localPath, destPath string) error {
	return b.Guest().InstallRootBinary(ctx, localPath, destPath)
}

func (b *Base) EnsureHubAuth(ctx context.Context, spec types.HubAuth) (bool, error) {
	return b.Guest().EnsureHubAuth(ctx, spec)
}

func (b *Base) EnsureLeverTemplate(ctx context.Context) (bool, error) {
	return b.Guest().EnsureLeverTemplate(ctx)
}

func (b *Base) EnsureScionTelemetry(ctx context.Context, off bool) (bool, error) {
	return b.Guest().EnsureScionTelemetry(ctx, off)
}

func (b *Base) ReadScionProjectState(ctx context.Context) (types.ScionProjectState, error) {
	return b.Guest().ReadScionProjectState(ctx, MountDest)
}

func (b *Base) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	return b.Guest().RemoveScionProjectConfigs(ctx, wp)
}

func (b *Base) RepairScionHubEndpoint(ctx context.Context, wp, endpoint string) error {
	return b.Guest().RepairScionHubEndpoint(ctx, wp, endpoint)
}

func (b *Base) ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error) {
	return b.Guest().ScionProjectRegistered(ctx, workspacePath)
}
