// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs/security-model.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
package orbstack

import (
	"context"
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/egress"
	"github.com/lever-to/lever/internal/exec"
)

const (
	distro = "ubuntu"
	// defaultRunUID is the fallback UID used for the rootless Docker socket path
	// (/run/user/<uid>/docker.sock) before EnsureUp has resolved the real UID.
	// OrbStack maps the host user to 501 by default, so DockerHost() is sensible
	// even before EnsureUp, per the interface's "valid after EnsureUp" contract.
	defaultRunUID = "501"
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

func (o *OrbStack) Profile() backend.Profile {
	return backend.Profile{
		Name:             "orbstack",
		SeparateKernel:   false, // shares the OrbStack VM kernel
		FSBoundedBy:      "isolated machine: no host files by default (project-tree mount NOT yet applied)",
		EgressEnforcedAt: "jail netns iptables/ip6tables",
		VersionFragile:   true, // depends on OrbStack --isolated behaviours
	}
}

func (o *OrbStack) DockerHost() string {
	uid := o.runUID
	if uid == "" {
		uid = defaultRunUID
	}
	return fmt.Sprintf("unix:///run/user/%s/docker.sock", uid)
}
func (o *OrbStack) HostToolAlias() string { return "host.orb.internal" }

func (o *OrbStack) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if err := o.ensureMachine(ctx); err != nil {
		return err
	}
	if err := o.resolveRunUser(ctx); err != nil {
		return err
	}
	if err := o.ensureRootlessDocker(ctx); err != nil {
		return err
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts)
}

// resolveRunUser caches the in-machine run user and UID so the subid/linger
// script and the rootless Docker socket path work for any OrbStack user, not a
// hardcoded one. Called after ensureMachine (the machine must exist) and before
// ensureRootlessDocker.
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

func (o *OrbStack) ensureMachine(ctx context.Context) error {
	res, err := o.r.Run(ctx, nil, "orb", "list")
	if err != nil {
		return fmt.Errorf("orb list: %w", err)
	}
	if machineListed(res.Stdout, o.machine) {
		return nil // idempotent
	}
	// The isolated machine intentionally shares NO host files. Mounting the
	// project tree (cfg.ProjectTree) IN is NOT yet implemented: it needs the
	// verified OrbStack mount mechanism for isolated machines, tracked as the
	// first task of the next slice. That is why Config.ProjectTree is currently
	// unused here — the `--mount` is deliberately omitted until then.
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
	if err := root(fmt.Sprintf(`grep -q '^%s:' /etc/subuid || echo '%s:100000:65536' >> /etc/subuid; grep -q '^%s:' /etc/subgid || echo '%s:100000:65536' >> /etc/subgid; loginctl enable-linger %s`,
		o.runUser, o.runUser, o.runUser, o.runUser, o.runUser)); err != nil {
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

func (o *OrbStack) ApplyEgress(ctx context.Context, allowedPorts []int) error {
	v4, v6, err := resolveHostAlias(ctx, o.r, o.machine)
	if err != nil {
		return err
	}
	o.aliasV4, o.aliasV6 = v4, v6
	for _, rule := range egress.BuildRules(v4, v6, allowedPorts) {
		args := append([]string{"-u", "root", "-m", o.machine, rule.Family.Binary()}, rule.Args...)
		if _, err := o.r.Run(ctx, nil, "orb", args...); err != nil {
			return fmt.Errorf("apply %s: %w", rule.Render(), err)
		}
	}
	return nil
}

var _ backend.Backend = (*OrbStack)(nil)
