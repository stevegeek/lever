// Package orbstack implements the Backend contract on macOS via an OrbStack
// isolated machine + rootless Docker + iptables egress. See docs/security-model.md §8 for the validated commands. Rootful Docker FAILS inside an isolated machine (seccomp blocks
// bpf()); rootless is required.
package orbstack

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/egress"
	"github.com/lever-to/lever/internal/exec"
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

func (o *OrbStack) Profile() backend.Profile {
	return backend.Profile{
		Name:             "orbstack",
		SeparateKernel:   false, // shares the OrbStack VM kernel
		FSBoundedBy:      "isolated machine: no host files + project tree mounted at /lever",
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
	if err := o.ensureRuntimes(ctx); err != nil {
		return err
	}
	return o.ApplyEgress(ctx, cfg.AllowedPorts)
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

// ensureRuntimes installs prereqs + rootless Docker and rootless Podman.
// Idempotent: the rootless install script and systemctl --start are safe to re-run.
// Podman is daemonless so no service startup is needed; scion auto-prefers it over Docker.
func (o *OrbStack) ensureRuntimes(ctx context.Context) error {
	root := func(script string) error {
		_, err := o.r.Run(ctx, nil, "orb", "-u", "root", "-m", o.machine, "bash", "-lc", script)
		return err
	}
	user := func(script string) error {
		_, err := o.r.Run(ctx, nil, "orb", "-m", o.machine, "bash", "-lc", script)
		return err
	}
	if err := root(`DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq uidmap dbus-user-session fuse-overlayfs slirp4netns curl iptables podman`); err != nil {
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

var _ backend.Backend = (*OrbStack)(nil)
