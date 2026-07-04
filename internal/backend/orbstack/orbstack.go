// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs-site/_guides/security-model.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
package orbstack

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/guest"
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

// orbStartProbeAttempts/orbStartProbeInterval bound the readiness wait after
// `orb start` resumes a machine that was powered off (e.g. by `lever stop`):
// OrbStack takes a moment before the guest is reachable, so ensureMachine
// cannot assume instant readiness before EnsureUp proceeds to
// resolveRunUser/guest provisioning. Package vars so tests run fast.
var (
	orbStartProbeAttempts = 30
	orbStartProbeInterval = 500 * time.Millisecond
)

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
// guest provisioning (guest.EnsureRuntimes).
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

// ResolveRunUser resolves the in-machine run user/uid WITHOUT provisioning: it
// probes the machine's existence/state via the same read-only `orb list` check
// ensureMachine uses, and errors if the machine is absent or not running,
// rather than creating, starting, or configuring it. For passive verbs
// (attach) that need the jail transport but must never bring the machine up.
func (o *OrbStack) ResolveRunUser(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	status, found := machineStatus(res.Stdout, o.machine)
	if !found {
		return fmt.Errorf("machine %q does not exist", o.machine)
	}
	if !strings.EqualFold(status, "running") {
		return fmt.Errorf("machine %q is not running (status %q)", o.machine, status)
	}
	return o.resolveRunUser(ctx)
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
	if status, found := machineStatus(res.Stdout, o.machine); found {
		if strings.EqualFold(status, "running") {
			// Idempotent: already up (and we cannot alter the mount after
			// creation, so no action is taken here). To change the project
			// tree, call Teardown() first, then EnsureUp() again.
			return nil
		}
		// Machine exists but is powered off (e.g. after `lever stop`) — power
		// it back on so `up` resumes a halted machine rather than silently
		// no-op'ing into an unreachable jail.
		if _, err := o.r.Run(ctx, nil, "orb", "start", o.machine); err != nil {
			return fmt.Errorf("orb start: %w", err)
		}
		return o.waitMachineReachable(ctx)
	}
	mountArg := projectTree + ":" + mountDest
	if _, err := o.r.Run(ctx, nil, "orb", "create", "--isolated", "--mount", mountArg, distro, o.machine); err != nil {
		return fmt.Errorf("orb create: %w", err)
	}
	return nil
}

// waitMachineReachable polls a lightweight in-machine command after `orb
// start` resumes a stopped machine, since the guest is not necessarily
// reachable the instant `orb start` returns.
func (o *OrbStack) waitMachineReachable(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < orbStartProbeAttempts; attempt++ {
		_, err := o.r.Run(ctx, nil, "orb", "-m", o.machine, "true")
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(orbStartProbeInterval):
		}
	}
	return fmt.Errorf("machine %q not reachable after orb start: %w", o.machine, lastErr)
}

func machineListed(stdout, name string) bool {
	_, found := machineStatus(stdout, name)
	return found
}

// machineStatus returns the status field (second column) of `orb list` output
// for name, and whether the machine was listed at all. Read-only: callers
// parse output from a probe (`orb list`) they already issued.
func machineStatus(stdout, name string) (status string, found bool) {
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != name {
			continue
		}
		if len(f) > 1 {
			return f[1], true
		}
		return "", true
	}
	return "", false
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

// Stop powers the machine off but keeps its disk intact — a strictly less
// destructive operation than Teardown (which deletes the machine). Idempotent:
// a no-op if the machine is already absent; orb tolerates stopping an
// already-stopped machine, so no separate guard is needed for that case.
func (o *OrbStack) Stop(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if !machineListed(res.Stdout, o.machine) {
		return nil // already gone; nothing to stop
	}
	if _, err := o.r.Run(ctx, nil, "orb", "stop", o.machine); err != nil {
		return fmt.Errorf("orb stop: %w", err)
	}
	return nil
}

func (o *OrbStack) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	v4, v6, rebuilt, err := o.guest().ApplyEgress(ctx,
		func(ctx context.Context) (string, string, error) { return resolveHostAlias(ctx, o.r, o.machine) },
		allowedPorts, closedInternet)
	if err != nil {
		return err
	}
	if rebuilt {
		o.aliasV4, o.aliasV6 = v4, v6
	} else {
		// I2 skip path: v6 is not authoritative here (existingClosedAlias only
		// parses v4 from the live chain) — do not clobber a prior aliasV6.
		o.aliasV4 = v4
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

// InstallGuestBinary streams a host-local executable into the machine at
// destPath as root, via the shared guest provisioner (RootPrefix =
// `orb -u root -m <machine>`).
func (o *OrbStack) InstallGuestBinary(ctx context.Context, localPath, destPath string) error {
	return o.guest().InstallRootBinary(ctx, localPath, destPath)
}

// ReadScionProjectState reads scion's registration state from the machine for
// `lever doctor` (in-tree marker + ~/.scion/project-configs). Read-only via the
// machine-only guest prefix, so it needs no EnsureUp.
func (o *OrbStack) ReadScionProjectState(ctx context.Context) (backend.ScionProjectState, error) {
	return o.guest().ReadScionProjectState(ctx, mountDest)
}

// RemoveScionProjectConfigs removes stale scion project-config registrations
// for wp from the machine, via the machine-only guest prefix.
func (o *OrbStack) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	return o.guest().RemoveScionProjectConfigs(ctx, wp)
}

var _ backend.Backend = (*OrbStack)(nil)
