// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs/security-model.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
package orbstack

import (
	"context"
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

const (
	distro = "ubuntu"
	// TODO(task9): resolve run-user dynamically (orb -m <machine> whoami) instead of hardcoding "leveruser"
	runUser = "leveruser"
	// runUserUID is the UID OrbStack maps for the host user; the rootless Docker
	// socket lives at /run/user/<uid>/docker.sock. Hardcoded to match the PoC
	// environment; Task 9 integration tests will force dynamic resolution.
	runUserUID = 501
	dockerSock = "unix:///run/user/501/docker.sock"
)

type OrbStack struct {
	r       exec.Runner
	machine string
	aliasV4 string
	aliasV6 string
}

func New(r exec.Runner, machine string) *OrbStack { return &OrbStack{r: r, machine: machine} }

func (o *OrbStack) Profile() backend.Profile {
	return backend.Profile{
		Name:             "orbstack",
		SeparateKernel:   false, // shares the OrbStack VM kernel
		FSBoundedBy:      "isolated machine: no host home + single --mount",
		EgressEnforcedAt: "jail netns iptables/ip6tables",
		VersionFragile:   true, // depends on OrbStack --isolated behaviours
	}
}

func (o *OrbStack) DockerHost() string    { return dockerSock }
func (o *OrbStack) HostToolAlias() string { return "host.orb.internal" }

func (o *OrbStack) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if err := o.ensureMachine(ctx); err != nil {
		return err
	}
	if err := o.ensureRootlessDocker(ctx); err != nil {
		return err
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts)
}

func (o *OrbStack) ensureMachine(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if machineListed(res.Stdout, o.machine) {
		return nil // idempotent
	}
	if _, err := o.r.Run(ctx, nil, "orb", "create", "--isolated", distro, o.machine); err != nil {
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

// ensureRootlessDocker installs prereqs + Docker rootless and starts the daemon.
// Idempotent: the rootless install script and systemctl --start are safe to re-run.
func (o *OrbStack) ensureRootlessDocker(ctx context.Context) error {
	root := func(script string) error {
		_, err := o.r.Run(ctx, nil, "orb", "-u", "root", "-m", o.machine, "bash", "-lc", script)
		return err
	}
	user := func(script string) error {
		_, err := o.r.Run(ctx, nil, "orb", "-m", o.machine, "bash", "-lc", script)
		return err
	}
	if err := root(`DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq uidmap dbus-user-session fuse-overlayfs slirp4netns curl iptables`); err != nil {
		return fmt.Errorf("apt prereqs: %w", err)
	}
	// TODO(task9): resolve run-user dynamically (orb -m <machine> whoami) instead of hardcoding "leveruser"
	if err := root(fmt.Sprintf(`grep -q '^%s:' /etc/subuid || echo '%s:100000:65536' >> /etc/subuid; grep -q '^%s:' /etc/subgid || echo '%s:100000:65536' >> /etc/subgid; loginctl enable-linger %s`,
		runUser, runUser, runUser, runUser, runUser)); err != nil {
		return fmt.Errorf("subid/linger: %w", err)
	}
	if err := user(`command -v dockerd-rootless.sh >/dev/null 2>&1 || curl -fsSL https://get.docker.com/rootless | sh`); err != nil {
		return fmt.Errorf("rootless install: %w", err)
	}
	if err := user(`export XDG_RUNTIME_DIR=/run/user/$(id -u); export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/docker.sock; systemctl --user enable --now docker 2>/dev/null || (nohup dockerd-rootless.sh >/tmp/lever-dockerd.log 2>&1 &); timeout 30 sh -c 'until docker info >/dev/null 2>&1; do sleep 1; done'`); err != nil {
		return fmt.Errorf("start rootless dockerd: %w", err)
	}
	return nil
}

// Teardown stops and removes the OrbStack machine. Idempotent: safe to call
// even when the machine does not exist.
func (o *OrbStack) Teardown(ctx context.Context) error {
	_, err := o.r.Run(ctx, nil, "orb", "delete", o.machine)
	return err
}

// ApplyEgress is implemented fully in Task 7; this stub lets EnsureUp compile
// and the Task 6 tests (which script the bash/iptables calls loosely) pass.
func (o *OrbStack) ApplyEgress(ctx context.Context, allowedPorts []int) error { return nil }
