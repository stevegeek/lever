// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs-site/_guides/security-model-compromise.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
//
// The prefix/guest-delegating half of the contract (DockerHost, the jail
// transport + image ops, the guest scion/egress delegations, run-user caching)
// lives once in internal/backend/common.Base, which this embeds; only the
// OrbStack-specific verbs (version preflight, machine create/start/stop) and the
// two prefix/guest hooks are here.
package orbstack

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/exec"
)

// distro is the isolated-machine base image (arch-tagless: OrbStack selects the
// host arch).
const distro = "ubuntu"

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
	common.Base
}

func New(r exec.Runner, machine string) *OrbStack {
	o := &OrbStack{}
	o.Base = common.Base{
		R:         r,
		Machine:   machine,
		HostAlias: "host.orb.internal",
		Hooks: common.Hooks{
			JailPrefix: JailPrefix,
			Guest: func(r exec.Runner, machine string) guest.Guest {
				return guest.Guest{
					Host:       r,
					UserPrefix: []string{"orb", "-m", machine},
					RootPrefix: []string{"orb", "-u", "root", "-m", machine},
					Machine:    machine,
				}
			},
			ResolveHostAlias: func(ctx context.Context) (string, string, error) {
				return resolveHostAlias(ctx, r, machine)
			},
		},
	}
	return o
}

// Profile returns orbstack's declared guarantees. The value lives once in
// backend.Candidates (the single source of the guarantee matrix); returning it
// here keeps the runtime profile and the documented one identical.
func (o *OrbStack) Profile() backend.Profile {
	p, _ := backend.ProfileFor("orbstack")
	return p
}

func (o *OrbStack) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if cfg.ProjectTree == "" {
		return fmt.Errorf("EnsureUp: ProjectTree is required")
	}
	// Preflight: require OrbStack >= 2.1.1 for --mount support on isolated machines.
	ok, got, err := orbVersionAtLeast(ctx, o.R, 2, 1, 1)
	if err != nil {
		return fmt.Errorf("EnsureUp: orb version check: %w", err)
	}
	if !ok {
		return fmt.Errorf("lever requires OrbStack >= 2.1.1 for isolated-machine mounts; found %s", got)
	}
	if err := o.ensureMachine(ctx, cfg.ProjectTree); err != nil {
		return err
	}
	if err := o.ReadRunUser(ctx); err != nil {
		return err
	}
	if err := o.Guest().EnsureRuntimes(ctx, o.User); err != nil {
		return err
	}
	if cfg.HasScion() {
		if err := o.Guest().EnsureScion(ctx, guest.ScionSpec{
			Binary:  cfg.ScionBinary,
			Source:  cfg.ScionSource,
			Version: cfg.ScionVersion,
		}); err != nil {
			return err
		}
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
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

// ResolveRunUser resolves the in-machine run user/uid WITHOUT provisioning: it
// probes the machine's existence/state via the same read-only `orb list` check
// ensureMachine uses, and errors if the machine is absent or not running,
// rather than creating, starting, or configuring it. For passive verbs
// (attach) that need the jail transport but must never bring the machine up.
func (o *OrbStack) ResolveRunUser(ctx context.Context) error {
	res, err := o.R.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	status, found := machineStatus(res.Stdout, o.Machine)
	if !found {
		return fmt.Errorf("machine %q does not exist", o.Machine)
	}
	if !strings.EqualFold(status, "running") {
		return fmt.Errorf("machine %q is not running (status %q)", o.Machine, status)
	}
	return o.ReadRunUser(ctx)
}

// ensureMachine creates the isolated OrbStack machine if it doesn't yet exist.
// The project tree is mounted at /lever via `--mount <projectTree>:/lever`; this
// is set at CREATE time only. Changing the tree on an existing machine requires
// teardown+recreate — mounts cannot be modified on a running machine (acceptable
// limitation; document it in operator notes).
func (o *OrbStack) ensureMachine(ctx context.Context, projectTree string) error {
	res, err := o.R.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if status, found := machineStatus(res.Stdout, o.Machine); found {
		if strings.EqualFold(status, "running") {
			// Idempotent: already up (and we cannot alter the mount after
			// creation, so no action is taken here). To change the project
			// tree, call Teardown() first, then EnsureUp() again.
			return nil
		}
		// Machine exists but is powered off (e.g. after `lever stop`) — power
		// it back on so `up` resumes a halted machine rather than silently
		// no-op'ing into an unreachable jail.
		if _, err := o.R.Run(ctx, nil, "orb", "start", o.Machine); err != nil {
			return fmt.Errorf("orb start: %w", err)
		}
		return o.waitMachineReachable(ctx)
	}
	mountArg := projectTree + ":" + common.MountDest
	if _, err := o.R.Run(ctx, nil, "orb", "create", "--isolated", "--mount", mountArg, distro, o.Machine); err != nil {
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
		_, err := o.R.Run(ctx, nil, "orb", "-m", o.Machine, "true")
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
	return fmt.Errorf("machine %q not reachable after orb start: %w", o.Machine, lastErr)
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
	res, err := o.R.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if !machineListed(res.Stdout, o.Machine) {
		return nil // already gone
	}
	if _, err := o.R.Run(ctx, nil, "orb", "delete", o.Machine); err != nil {
		return fmt.Errorf("orb delete: %w", err)
	}
	return nil
}

// Stop powers the machine off but keeps its disk intact — a strictly less
// destructive operation than Teardown (which deletes the machine). Idempotent:
// a no-op if the machine is already absent; orb tolerates stopping an
// already-stopped machine, so no separate guard is needed for that case.
func (o *OrbStack) Stop(ctx context.Context) error {
	res, err := o.R.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if !machineListed(res.Stdout, o.Machine) {
		return nil // already gone; nothing to stop
	}
	if _, err := o.R.Run(ctx, nil, "orb", "stop", o.Machine); err != nil {
		return fmt.Errorf("orb stop: %w", err)
	}
	return nil
}

// JailPrefix is the argv prefix that executes inside this backend's machine as
// the given user. Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(machine, user string) []string {
	return []string{"orb", "-m", machine, "-u", user}
}

var _ backend.Backend = (*OrbStack)(nil)
