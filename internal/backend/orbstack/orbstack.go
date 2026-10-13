// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless podman + iptables egress. Rootful Docker fails
// inside an isolated machine (seccomp blocks bpf()), so rootless is required.
// The shared half lives in internal/backend/common.Base, which this embeds.
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
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/retry"
)

// distro names the release so a new machine gets the Ubuntu LTS the Lima
// guest runs, not whatever OrbStack calls the latest Ubuntu that day. Read
// only when a machine is created.
const distro = "ubuntu:resolute"

var orbVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)\.(\d+)`)

// startProbeAttempts/startProbeInterval bound the readiness wait after `orb
// start` resumes a powered-off machine: the guest is not reachable the
// instant `orb start` returns.
const (
	startProbeAttempts = 30
	startProbeInterval = 500 * time.Millisecond
)

type OrbStack struct {
	common.Base

	// Tests shrink these.
	probeAttempts int
	probeInterval time.Duration
}

func New(r proc.Runner, machine string, opts common.Options) *OrbStack {
	o := &OrbStack{probeAttempts: startProbeAttempts, probeInterval: startProbeInterval}
	o.Base = common.NewBase(common.Config{
		Runner:    r,
		Machine:   machine,
		HostAlias: "host.orb.internal",
		Options:   opts,
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

// Profile is orbstack's declared guarantees, the single value every report
// uses.
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
	// OrbStack < 2.1.1 lacks --mount support on isolated machines.
	ok, got, err := common.VersionAtLeast(ctx, o.Runner(), []string{"orb", "version"}, orbVersionRe, 2, 1, 1)
	if err != nil {
		return fmt.Errorf("EnsureUp: orb version check: %w", err)
	}
	if !ok {
		return fmt.Errorf("lever requires OrbStack >= 2.1.1 for isolated-machine mounts; found %s", got)
	}
	s := progress.From(ctx).Start("start machine")
	skip, err := o.ensureMachine(ctx, cfg.ProjectTree)
	if err == nil && skip != "" {
		s.Skip(skip)
	} else {
		s.Done(err)
	}
	if err != nil {
		return err
	}
	return o.Provision(ctx, cfg)
}

// ResolveRunUser errors if the machine is absent or not running, rather than
// creating, starting or configuring it.
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

// ensureMachine: the project tree mount is set at CREATE time only; changing
// it on an existing machine requires Teardown+EnsureUp.
func (o *OrbStack) ensureMachine(ctx context.Context, projectTree string) (skip string, err error) {
	status, found, err := o.machineStatus(ctx)
	if err != nil {
		return "", err
	}
	if found {
		if strings.EqualFold(status, "running") {
			return "already running", nil
		}
		if _, err := o.Runner().Run(ctx, nil, "orb", "start", o.Machine()); err != nil {
			return "", fmt.Errorf("orb start: %w", err)
		}
		return "", o.waitMachineReachable(ctx)
	}
	mountArg := projectTree + ":" + common.MountDest
	if _, err := o.Runner().Run(ctx, nil, "orb", "create", "--isolated", "--mount", mountArg, distro, o.Machine()); err != nil {
		return "", fmt.Errorf("orb create: %w", err)
	}
	return "", nil
}

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

func (o *OrbStack) machineStatus(ctx context.Context) (status string, found bool, err error) {
	res, err := o.Runner().Run(ctx, nil, "orb", "list")
	if err != nil {
		return "", false, fmt.Errorf("orb list: %w", err)
	}
	status, found = parseMachineStatus(res.Stdout, o.Machine())
	return status, found, nil
}

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

// Teardown deletes the jail machine. Idempotent.
func (o *OrbStack) Teardown(ctx context.Context) error {
	return o.orbIfPresent(ctx, "delete")
}

// Stop powers the machine off but keeps its disk. Idempotent: orb tolerates
// stopping an already-stopped machine.
func (o *OrbStack) Stop(ctx context.Context) error {
	return o.orbIfPresent(ctx, "stop")
}

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
// the given user.
func JailPrefix(machine, user string) []string {
	return []string{"orb", "-m", machine, "-u", user}
}

var _ backend.Backend = (*OrbStack)(nil)
