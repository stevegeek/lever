// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs-site/_guides/security-model.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
package orbstack

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/guest"
	"github.com/lever-to/lever/internal/egress"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/jail"
)

const (
	distro = "ubuntu"
	// mountDest is the path inside the isolated machine where the project tree
	// is bind-mounted via `orb create --mount <host>:/lever`. Agents work
	// exclusively within this directory; no host home is visible.
	mountDest = "/lever"
	// defaultRunUID is the fallback UID used for the rootless Docker socket path
	// (/run/user/<uid>/docker.sock) before EnsureUp has resolved the real UID.
	// OrbStack maps the host user to 501 by default, so DockerHost() is sensible
	// even before EnsureUp, per the interface's "valid after EnsureUp" contract.
	defaultRunUID = "501"
)

// orbVersionRe matches "Version: 2.2.1 (2020100)" lines from `orb version`.
var orbVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)\.(\d+)`)

type OrbStack struct {
	r       exec.Runner
	machine string
	aliasV4 string
	aliasV6 string
	runUser string // resolved via `orb -m <machine> whoami`
	runUID  string // resolved via `orb -m <machine> id -u`
}

func New(r exec.Runner, machine string) *OrbStack { return &OrbStack{r: r, machine: machine} }

// Profile returns orbstack's declared guarantees. The value lives once in
// backend.Candidates (the single source of the guarantee matrix); returning it
// here keeps the runtime profile and the documented one identical.
func (o *OrbStack) Profile() backend.Profile {
	p, _ := backend.ProfileFor("orbstack")
	return p
}

func (o *OrbStack) DockerHost() string {
	uid := o.runUID
	if uid == "" {
		uid = defaultRunUID
	}
	return fmt.Sprintf("unix:///run/user/%s/docker.sock", uid)
}
func (o *OrbStack) HostToolAlias() string { return "host.orb.internal" }

// HostAliasV4 returns the resolved IPv4 of host.orb.internal as seen from the
// jail, valid after EnsureUp/ApplyEgress. Empty if not yet resolved. Used to mint
// the broker cert with an IP SAN and to build IP-based broker URLs so agents
// reach the broker without DNS under closed-internet egress.
func (o *OrbStack) HostAliasV4() string { return o.aliasV4 }

func (o *OrbStack) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if cfg.ProjectTree == "" {
		return fmt.Errorf("EnsureUp: ProjectTree is required")
	}
	// Preflight: require OrbStack >= 2.1.1 for --mount support on isolated machines.
	ok, got, err := orbVersionAtLeast(ctx, o.r, 2, 1, 1)
	if err != nil {
		return fmt.Errorf("EnsureUp: orb version check: %w", err)
	}
	if !ok {
		return fmt.Errorf("lever requires OrbStack >= 2.1.1 for isolated-machine mounts; found %s", got)
	}
	if err := o.ensureMachine(ctx, cfg.ProjectTree); err != nil {
		return err
	}
	if err := o.resolveRunUser(ctx); err != nil {
		return err
	}
	if err := o.guest().EnsureRuntimes(ctx, o.runUser); err != nil {
		return err
	}
	if cfg.ScionSource != "" || cfg.ScionVersion != "" {
		if err := o.guest().EnsureScion(ctx, cfg.ScionSource, cfg.ScionVersion); err != nil {
			return err
		}
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
}

// guest returns the shared guest provisioner scoped to this machine, used to
// install runtimes and scion via host-side argv prefixes. See
// internal/backend/guest for the provisioning scripts themselves.
func (o *OrbStack) guest() guest.Guest {
	return guest.Guest{
		Host:       o.r,
		UserPrefix: []string{"orb", "-m", o.machine},
		RootPrefix: []string{"orb", "-u", "root", "-m", o.machine},
		Machine:    o.machine,
	}
}

// orbVersionAtLeast runs `orb version`, parses the semver, and returns whether
// it is >= (major, minor, patch). got is the raw version string on success or
// the raw output on parse failure.
func orbVersionAtLeast(ctx context.Context, r exec.Runner, major, minor, patch int) (ok bool, got string, err error) {
	res, err := r.Run(ctx, nil, "orb", "version")
	if err != nil {
		return false, "", fmt.Errorf("orb version: %w", err)
	}
	m := orbVersionRe.FindStringSubmatch(res.Stdout)
	if m == nil {
		return false, strings.TrimSpace(res.Stdout), fmt.Errorf("orb version: could not parse version from %q", strings.TrimSpace(res.Stdout))
	}
	// m[1],m[2],m[3] are guaranteed digits by the regex.
	vMaj, _ := strconv.Atoi(m[1])
	vMin, _ := strconv.Atoi(m[2])
	vPat, _ := strconv.Atoi(m[3])
	got = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])

	switch {
	case vMaj > major:
		return true, got, nil
	case vMaj < major:
		return false, got, nil
	// vMaj == major
	case vMin > minor:
		return true, got, nil
	case vMin < minor:
		return false, got, nil
	// vMin == minor
	default:
		return vPat >= patch, got, nil
	}
}

// resolveRunUser caches the in-machine run user and UID so the subid/linger
// script and the rootless Docker socket path work for any OrbStack user, not a
// hardcoded one. Called after ensureMachine (the machine must exist) and before
// ensureRuntimes.
func (o *OrbStack) resolveRunUser(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "-m", o.machine, "whoami")
	if err != nil {
		return fmt.Errorf("resolve run user: %w", err)
	}
	o.runUser = strings.TrimSpace(res.Stdout)
	res, err = o.r.Run(ctx, nil, "orb", "-m", o.machine, "id", "-u")
	if err != nil {
		return fmt.Errorf("resolve run uid: %w", err)
	}
	o.runUID = strings.TrimSpace(res.Stdout)
	return nil
}

// ensureMachine creates the isolated OrbStack machine if it doesn't yet exist.
// The project tree is mounted at /lever via `--mount <projectTree>:/lever`; this
// is set at CREATE time only. Changing the tree on an existing machine requires
// teardown+recreate — mounts cannot be modified on a running machine (acceptable
// limitation; document it in operator notes).
func (o *OrbStack) ensureMachine(ctx context.Context, projectTree string) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if machineListed(res.Stdout, o.machine) {
		// Idempotent: machine already exists; we cannot alter the mount after
		// creation, so no action is taken here. To change the project tree,
		// call Teardown() first, then EnsureUp() again.
		return nil
	}
	mountArg := projectTree + ":" + mountDest
	if _, err := o.r.Run(ctx, nil, "orb", "create", "--isolated", "--mount", mountArg, distro, o.machine); err != nil {
		return fmt.Errorf("orb create: %w", err)
	}
	return nil
}

func machineListed(stdout, name string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true
		}
	}
	return false
}

// Teardown deletes the jail machine. Idempotent: a no-op if the machine is
// already absent.
func (o *OrbStack) Teardown(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if !machineListed(res.Stdout, o.machine) {
		return nil // already gone
	}
	if _, err := o.r.Run(ctx, nil, "orb", "delete", o.machine); err != nil {
		return fmt.Errorf("orb delete: %w", err)
	}
	return nil
}

func (o *OrbStack) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	// I2 — never briefly open egress under a running closed instance. If the closed
	// posture is ALREADY active (LEVER_EGRESS has the catch-all DROP), a running
	// jailed agent depends on it; flushing+rebuilding would leave the chain empty
	// (→ OUTPUT default ACCEPT) for the resolve+rebuild window. The ruleset is a
	// pure function of (alias, ports, closed) and is unchanged on a normal
	// re-apply, so leave the working chain intact and skip the rebuild. (A fresh
	// machine, the open/subscription posture, or a genuine egress-config change all
	// have no active DROP and fall through to a full rebuild — and a fresh apply has
	// no running container to leak. Changing egress config on a live closed instance
	// requires `lever down` + `up`, not a re-apply.)
	if closedInternet {
		if alias, ok := o.existingClosedAlias(ctx); ok {
			o.aliasV4 = alias
			return nil
		}
	}
	// Reset the dedicated chain FIRST: this makes re-apply idempotent (no rule
	// accumulation) and — critically — flushing the chain removes any prior
	// catch-all DROP, restoring DNS so the host-alias re-resolve below works even
	// when a previous closed-egress posture is still in place.
	if err := o.resetEgressChain(ctx); err != nil {
		return err
	}
	v4, v6, err := resolveHostAlias(ctx, o.r, o.machine)
	if err != nil {
		return err
	}
	o.aliasV4, o.aliasV6 = v4, v6
	for _, rule := range egress.BuildRules(v4, v6, allowedPorts, closedInternet) {
		args := append([]string{"-u", "root", "-m", o.machine, rule.Family.Binary()}, rule.Args...)
		if _, err := o.r.Run(ctx, nil, "orb", args...); err != nil {
			return fmt.Errorf("apply %s: %w", rule.Render(), err)
		}
	}
	return nil
}

// existingClosedAlias returns the v4 host alias already encoded in an ACTIVE
// closed LEVER_EGRESS chain (i.e. the chain contains the catch-all DROP), so
// ApplyEgress can skip the flush+rebuild that would briefly open egress (I2).
// Returns ("", false) when the chain is absent, open (no catch-all DROP), or
// unreadable — the caller then (re)builds the chain normally. The alias is read
// from the per-port ACCEPT rule (`-d <ip>/32 … --dport … -j ACCEPT`), so we never
// need DNS (which the active DROP blocks anyway).
func (o *OrbStack) existingClosedAlias(ctx context.Context) (string, bool) {
	res, err := o.r.Run(ctx, nil, "orb", "-u", "root", "-m", o.machine, "iptables", "-S", egress.Chain)
	if err != nil {
		return "", false // chain absent (fresh machine) or unreadable
	}
	out := res.Stdout + res.Stderr
	if !strings.Contains(out, "-A "+egress.Chain+" -j DROP") {
		return "", false // not in the closed posture (no catch-all DROP)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "--dport") || !strings.Contains(line, "-j ACCEPT") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-d" && i+1 < len(fields) {
				ip := strings.TrimSuffix(fields[i+1], "/32")
				if net.ParseIP(ip) != nil {
					return ip, true
				}
			}
		}
	}
	return "", false // closed but no parseable alias — rebuild to be safe
}

// resetEgressChain ensures the LEVER_EGRESS chain exists and OUTPUT jumps to it
// exactly once, then flushes it, for both address families. Idempotent: the chain
// holds ALL of lever's egress rules, so flushing wipes the prior apply's rules
// (no accumulation) without touching any non-lever OUTPUT rules, and removes the
// catch-all DROP so DNS works for the subsequent host-alias resolve.
func (o *OrbStack) resetEgressChain(ctx context.Context) error {
	for _, bin := range []string{"iptables", "ip6tables"} {
		base := []string{"-u", "root", "-m", o.machine, bin}
		run := func(args ...string) error {
			_, err := o.r.Run(ctx, nil, "orb", append(append([]string{}, base...), args...)...)
			return err
		}
		// Create the chain if absent (-N errors if it already exists — tolerate).
		_ = run("-N", egress.Chain)
		// Ensure the OUTPUT jump exists exactly once: -C checks, -A adds if missing.
		if err := run("-C", "OUTPUT", "-j", egress.Chain); err != nil {
			if err := run("-A", "OUTPUT", "-j", egress.Chain); err != nil {
				return fmt.Errorf("egress: add OUTPUT jump (%s): %w", bin, err)
			}
		}
		// Flush our chain (idempotent re-apply; restores DNS for the re-resolve).
		if err := run("-F", egress.Chain); err != nil {
			return fmt.Errorf("egress: flush %s (%s): %w", egress.Chain, bin, err)
		}
	}
	return nil
}

// MountDest returns the path inside the jail where the project tree is bind-mounted.
func (o *OrbStack) MountDest() string { return mountDest }

// MachineName returns the jail machine name this backend targets.
func (o *OrbStack) MachineName() string { return o.machine }

// RunUser returns the in-machine run user resolved by EnsureUp (valid after EnsureUp).
func (o *OrbStack) RunUser() string { return o.runUser }

// RunUID returns the in-machine run user's UID resolved by EnsureUp.
// Falls back to defaultRunUID if EnsureUp has not yet been called.
func (o *OrbStack) RunUID() string {
	if o.runUID == "" {
		return defaultRunUID
	}
	return o.runUID
}

// JailPrefix is the argv prefix that executes inside this backend's machine as
// the given user. Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(machine, user string) []string { return jail.OrbPrefix(machine, user) }

// JailRunner returns the command transport into the jail (valid after EnsureUp,
// which resolves the run user).
func (o *OrbStack) JailRunner() exec.Runner {
	return jail.New(o.r, JailPrefix(o.machine, o.runUser), o.RunUID())
}

// AttachArgv builds the host argv for an interactive in-jail command.
func (o *OrbStack) AttachArgv(inner []string) []string {
	return jail.AttachArgv(JailPrefix(o.machine, o.runUser), o.RunUID(), inner)
}

// LoadImage streams a host docker image into the jail's rootless podman.
func (o *OrbStack) LoadImage(ctx context.Context, imageRef string) error {
	return jail.LoadImage(ctx, JailPrefix(o.machine, o.runUser), o.RunUID(), imageRef)
}

var _ backend.Backend = (*OrbStack)(nil)
