// Package jail provides a JailRunner: an exec.Runner that executes commands
// INSIDE a jail via a backend-supplied argv prefix — e.g.
// ["orb","-m",m,"-u",u] (OrbStack) or ["limactl","shell",vm] (Lima) — followed
// by `env [-C dir] K=V… cmd args`. GNU `env` sets the jail environment (and
// cwd via -C) with no shell quoting, so scion.Client runs unchanged inside the
// jail. The host runner it wraps is the real one (the prefix binary runs on
// the host).
package jail

import (
	"context"
	"os"
	"sort"
	"strconv"

	"github.com/stevegeek/lever/internal/exec"
)

// compile-time assertion: *Runner satisfies exec.Runner
var _ exec.Runner = (*Runner)(nil)

type Runner struct {
	host   exec.Runner
	prefix []string
	uid    string // run-user uid, for XDG_RUNTIME_DIR
}

func New(host exec.Runner, prefix []string, uid string) *Runner {
	return &Runner{host: host, prefix: prefix, uid: uid}
}

// OrbPrefix is the argv prefix that executes inside an OrbStack machine.
func OrbPrefix(machine, user string) []string {
	return []string{"orb", "-m", machine, "-u", user}
}

// jailEnvFor is the fixed environment every in-jail command needs, for a given
// run-user uid. Shared by Runner.jailEnv and AttachArgv (attach.go) so the env
// list lives in exactly one place.
func jailEnvFor(uid string) []string {
	env := []string{
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
	}
	// Agents run in their OWN per-agent network namespace (rootless podman's
	// default pasta networking), so each container's 127.0.0.1 is private. That
	// private loopback is what isolates one agent's in-container gateway proxy
	// (127.0.0.1:8462) from co-resident agents — under a shared --network=host
	// netns any agent could reach another's gateway and act as it (no creds).
	// Hub reachability across the netns boundary is restored host-side, not by
	// host networking: the guest containers.conf sets pasta
	// --map-host-loopback 169.254.1.2 (guest.EnsureRuntimes), and scion's
	// auto-computed container hub endpoint (host.containers.internal → 169.254.1.2)
	// then resolves to the VM-loopback hub. Egress containment is unaffected:
	// pasta's egress re-emerges on the VM OUTPUT chain (LEVER_EGRESS), verified
	// live. Escape hatch: set LEVER_FORCE_HOST_NETWORK to a truthy value (1/true)
	// on the host to fall back to scion's --network=host (shared netns) for
	// debugging — NOT isolation-safe. Parsed as a bool so =0/=false correctly
	// mean OFF and any unparseable/empty value stays OFF (own netns): a surprising
	// value on this security knob never silently re-opens the shared-loopback gap.
	if force, _ := strconv.ParseBool(os.Getenv("LEVER_FORCE_HOST_NETWORK")); force {
		env = append(env, "SCION_FORCE_HOST_NETWORK=1")
	}
	return env
}

// jailEnv is the fixed environment every in-jail command needs.
func (r *Runner) jailEnv() []string {
	return jailEnvFor(r.uid)
}

func envKVs(env map[string]string) []string {
	kvs := make([]string, 0, len(env))
	for k, v := range env {
		kvs = append(kvs, k+"="+v)
	}
	sort.Strings(kvs) // deterministic argv (testability)
	return kvs
}

func (r *Runner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *Runner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	argv := append([]string{}, r.prefix[1:]...)
	argv = append(argv, "env")
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	argv = append(argv, r.jailEnv()...)
	argv = append(argv, envKVs(env)...)
	argv = append(argv, name)
	argv = append(argv, args...)
	return r.host.Run(ctx, nil, r.prefix[0], argv...)
}
