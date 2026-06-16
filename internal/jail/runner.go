// Package jail provides a JailRunner: an exec.Runner that executes commands
// INSIDE an OrbStack machine via `orb -m <machine> -u <user> env [-C dir] K=V… cmd args`.
// GNU `env` sets the jail environment (and cwd via -C) with no shell quoting, so
// scion.Client runs unchanged inside the jail. The host runner it wraps
// is the real one (orb runs on the host).
package jail

import (
	"context"
	"sort"

	"github.com/lever-to/lever/internal/exec"
)

// compile-time assertion: *Runner satisfies exec.Runner
var _ exec.Runner = (*Runner)(nil)

type Runner struct {
	host    exec.Runner
	machine string
	user    string
	uid     string // run-user uid, for XDG_RUNTIME_DIR
}

func New(host exec.Runner, machine, user, uid string) *Runner {
	return &Runner{host: host, machine: machine, user: user, uid: uid}
}

// jailEnvFor is the fixed environment every in-jail command needs, for a given
// run-user uid. Shared by Runner.jailEnv and AttachArgv (attach.go) so the env
// list lives in exactly one place.
func jailEnvFor(uid string) []string {
	return []string{
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
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
	orbArgs := []string{"-m", r.machine, "-u", r.user, "env"}
	if dir != "" {
		orbArgs = append(orbArgs, "-C", dir)
	}
	orbArgs = append(orbArgs, r.jailEnv()...)
	orbArgs = append(orbArgs, envKVs(env)...)
	orbArgs = append(orbArgs, name)
	orbArgs = append(orbArgs, args...)
	return r.host.Run(ctx, nil, "orb", orbArgs...)
}
