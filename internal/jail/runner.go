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
	"sort"

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
	return []string{
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
		// Force the agent containers onto --network=host so they reach the
		// jail-local hub on loopback (the broker/agent-launch reads this).
		// Without it, rootless podman uses pasta networking and the agent
		// cannot reach the hub → heartbeats fail. scion's official escape
		// hatch (>= upstream da49e14); applies to any runtime, not just docker.
		"SCION_FORCE_HOST_NETWORK=1",
	}
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
