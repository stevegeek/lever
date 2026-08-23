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
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/retry"
)

// distro is the isolated-machine base image (arch-tagless: OrbStack selects the
// host arch).
const distro = "ubuntu"

// orbVersionRe matches "Version: 2.2.1 (2020100)" lines from `orb version`.
var orbVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)\.(\d+)`)

// startProbeAttempts/startProbeInterval bound the readiness wait after
// `orb start` resumes a machine that was powered off (e.g. by `lever stop`):
// OrbStack takes a moment before the guest is reachable, so ensureMachine
// cannot assume instant readiness before EnsureUp proceeds to
// resolveRunUser/guest provisioning.
const (
	startProbeAttempts = 30
	startProbeInterval = 500 * time.Millisecond
)

type OrbStack struct {
	common.Base

	// probeAttempts/probeInterval bound waitMachineReachable; New sets the
	// defaults and tests shrink them on the instance.
	probeAttempts int
	probeInterval time.Duration
}

func New(r proc.Runner, machine string) *OrbStack {
	o := &OrbStack{probeAttempts: startProbeAttempts, probeInterval: startProbeInterval}
	o.Base = common.NewBase(common.Config{
		Runner:    r,
		Machine:   machine,
		HostAlias: "host.orb.internal",
		Hooks: common.Hooks{
			JailPrefix: JailPrefix,
			Guest: func(r proc.Runner, machine string) guest.Guest {
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
	})
	return o
}

// Profile is orbstack's declared guarantees — the one value the runtime
// Profile() method, the registry's guarantee matrix and `lever backends` all
// report, so they cannot disagree.
var Profile = backend.Profile{
	Name:             "orbstack",
	SeparateKernel:   false, // shares the one OrbStack VM kernel across manager+workers
	FSBoundedBy:      "isolated machine: no host files + project tree mounted at /lever",
	EgressEnforcedAt: "jail netns iptables/ip6tables",
	VersionFragile:   true, // depends on OrbStack --isolated behaviours
}

func (o *OrbStack) Profile() backend.Profile { return Profile }

func (o *OrbStack) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if cfg.ProjectTree == "" {
		return fmt.Errorf("EnsureUp: ProjectTree is required")
	}
	// Preflight: require OrbStack >= 2.1.1 for --mount support on isolated machines.
	ok, got, err := common.VersionAtLeast(ctx, o.Runner(), []string{"orb", "version"}, orbVersionRe, 2, 1, 1)
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
	if err := o.Guest().EnsureRuntimes(ctx, o.RunUser()); err != nil {
		return err
	}
	if cfg.HasScion() {
		if err := o.Guest().EnsureScion(ctx, guest.ScionSpec{
			Binary:  cfg.ScionBinary,
			Source:  cfg.ScionSource,
			Version: cfg.ScionVersion,
			WebUI:   cfg.ScionWebUI,
		}); err != nil {
			return err
		}
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
}

// ResolveRunUser resolves the in-machine run user/uid WITHOUT provisioning: it
// probes the machine's existence/state via the same read-only `orb list` check
// ensureMachine uses, and errors if the machine is absent or not running,
// rather than creating, starting, or configuring it. For passive verbs
// (attach) that need the jail transport but must never bring the machine up.
func (o *OrbStack) ResolveRunUser(ctx context.Context) error {
	status, found, err := o.machineStatus(ctx)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("machine %q does not exist", o.Machine())
	}
	if !strings.EqualFold(status, "running") {
		return fmt.Errorf("machine %q is not running (status %q)", o.Machine(), status)
	}
	return o.ReadRunUser(ctx)
}

// ensureMachine creates the isolated OrbStack machine if it doesn't yet exist.
// The project tree is mounted at /lever via `--mount <projectTree>:/lever`; this
// is set at CREATE time only. Changing the tree on an existing machine requires
// teardown+recreate — mounts cannot be modified on a running machine (acceptable
// limitation; document it in operator notes).
func (o *OrbStack) ensureMachine(ctx context.Context, projectTree string) error {
	status, found, err := o.machineStatus(ctx)
	if err != nil {
		return err
	}
	if found {
		if strings.EqualFold(status, "running") {
			// Idempotent: already up (and we cannot alter the mount after
			// creation, so no action is taken here). To change the project
			// tree, call Teardown() first, then EnsureUp() again.
			return nil
		}
		// Machine exists but is powered off (e.g. after `lever stop`) — power
		// it back on so `up` resumes a halted machine rather than silently
		// no-op'ing into an unreachable jail.
		if _, err := o.Runner().Run(ctx, nil, "orb", "start", o.Machine()); err != nil {
			return fmt.Errorf("orb start: %w", err)
		}
		return o.waitMachineReachable(ctx)
	}
	mountArg := projectTree + ":" + common.MountDest
	if _, err := o.Runner().Run(ctx, nil, "orb", "create", "--isolated", "--mount", mountArg, distro, o.Machine()); err != nil {
		return fmt.Errorf("orb create: %w", err)
	}
	return nil
}

// waitMachineReachable polls a lightweight in-machine command after `orb
// start` resumes a stopped machine, since the guest is not necessarily
// reachable the instant `orb start` returns.
func (o *OrbStack) waitMachineReachable(ctx context.Context) error {
	var lastErr error
	err := retry.Until(ctx, o.probeAttempts, o.probeInterval, func() (bool, error) {
		_, lastErr = o.Runner().Run(ctx, nil, "orb", "-m", o.Machine(), "true")
		return lastErr == nil, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return fmt.Errorf("machine %q not reachable after orb start: %w", o.Machine(), lastErr)
	}
	return err
}

// machineStatus probes `orb list` (read-only) and returns this machine's
// status field and whether it was listed at all. The one place every verb
// that needs to know "does it exist / is it running" asks.
func (o *OrbStack) machineStatus(ctx context.Context) (status string, found bool, err error) {
	res, err := o.Runner().Run(ctx, nil, "orb", "list")
	if err != nil {
		return "", false, fmt.Errorf("orb list: %w", err)
	}
	status, found = parseMachineStatus(res.Stdout, o.Machine())
	return status, found, nil
}

// parseMachineStatus returns the status field (second column) of `orb list`
// output for name, and whether the machine was listed at all.
func parseMachineStatus(stdout, name string) (status string, found bool) {
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
	return o.orbIfPresent(ctx, "delete")
}

// Stop powers the machine off but keeps its disk intact — a strictly less
// destructive operation than Teardown (which deletes the machine). Idempotent:
// a no-op if the machine is already absent; orb tolerates stopping an
// already-stopped machine, so no separate guard is needed for that case.
func (o *OrbStack) Stop(ctx context.Context) error {
	return o.orbIfPresent(ctx, "stop")
}

// orbIfPresent runs `orb <verb> <machine>` when the machine exists and is a
// no-op when it is already gone. Shared by Stop and Teardown.
func (o *OrbStack) orbIfPresent(ctx context.Context, verb string) error {
	_, found, err := o.machineStatus(ctx)
	if err != nil {
		return err
	}
	if !found {
		return nil // already gone
	}
	if _, err := o.Runner().Run(ctx, nil, "orb", verb, o.Machine()); err != nil {
		return fmt.Errorf("orb %s: %w", verb, err)
	}
	return nil
}

// JailPrefix is the argv prefix that executes inside this backend's machine as
// the given user. Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(machine, user string) []string {
	return []string{"orb", "-m", machine, "-u", user}
}

var _ backend.Backend = (*OrbStack)(nil)
