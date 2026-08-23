// Package jail provides a JailRunner: a proc.Runner that executes commands
// INSIDE a jail via a backend-supplied argv prefix — e.g.
// ["orb","-m",m,"-u",u] (OrbStack) or ["limactl","shell",vm] (Lima) — followed
// by `env [-C dir] K=V… cmd args`. GNU `env` sets the jail environment (and
// cwd via -C) with no shell quoting, so scion.Client runs unchanged inside the
// jail. The host runner it wraps is the real one (the prefix binary runs on
// the host).
package jail

import (
	"context"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"

	"github.com/stevegeek/lever/internal/proc"
)

// compile-time assertion: *Runner satisfies proc.Runner
var _ proc.Runner = (*Runner)(nil)

// ForceHostNetworkEnv is the host-side escape hatch that makes every in-jail
// scion command fall back to `--network=host` (a shared netns) for debugging —
// NOT isolation-safe. See Config.ForceHostNetwork for what it changes.
const ForceHostNetworkEnv = "LEVER_FORCE_HOST_NETWORK"

// ForceHostNetworkFromEnv reads ForceHostNetworkEnv from the process
// environment. Parsed as a bool so =0/=false correctly mean OFF and any
// unparseable/empty value stays OFF (own netns): a surprising value on this
// security knob never silently re-opens the shared-loopback gap. It is the
// ONLY place the variable is read; the transport itself (Runner) is pure and
// takes the answer through Config.
func ForceHostNetworkFromEnv() bool {
	force, _ := strconv.ParseBool(os.Getenv(ForceHostNetworkEnv))
	return force
}

// Config is everything a Runner needs to reach one jail.
type Config struct {
	Host   proc.Runner // host runner (the prefix binary runs on the host)
	Prefix []string    // backend argv prefix, e.g. ["orb","-m",m,"-u",u]
	UID    string      // run-user uid, for XDG_RUNTIME_DIR
	// ForceHostNetwork re-emits scion's SCION_FORCE_HOST_NETWORK so agents run
	// under --network=host (shared netns) instead of their own pasta netns.
	// Agents run in their OWN per-agent network namespace by default (rootless
	// podman's pasta networking), so each container's 127.0.0.1 is private.
	// That private loopback is what isolates one agent's in-container gateway
	// proxy (127.0.0.1:8462) from co-resident agents — under a shared
	// --network=host netns any agent could reach another's gateway and act as
	// it (no creds). Hub reachability across the netns boundary is restored
	// host-side, not by host networking: the guest containers.conf sets pasta
	// --map-host-loopback 169.254.1.2 (guest.EnsureRuntimes), and scion's
	// auto-computed container hub endpoint (host.containers.internal →
	// 169.254.1.2) then resolves to the VM-loopback hub. Egress containment is
	// unaffected: pasta's egress re-emerges on the VM OUTPUT chain
	// (LEVER_EGRESS), verified live. Constructors take the value from
	// ForceHostNetworkFromEnv.
	ForceHostNetwork bool
}

type Runner struct {
	cfg Config
}

func New(cfg Config) *Runner {
	return &Runner{cfg: cfg}
}

// jailEnv is the fixed environment every in-jail command needs. Shared by
// RunIn and AttachArgv so the env list lives in exactly one place.
func (r *Runner) jailEnv() []string {
	env := []string{
		"XDG_RUNTIME_DIR=/run/user/" + r.cfg.UID,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
	}
	if r.cfg.ForceHostNetwork {
		env = append(env, "SCION_FORCE_HOST_NETWORK=1")
	}
	return env
}

func envKVs(env map[string]string) []string {
	kvs := make([]string, 0, len(env))
	for k, v := range env {
		kvs = append(kvs, k+"="+v)
	}
	sort.Strings(kvs) // deterministic argv (testability)
	return kvs
}

// argv builds the full host argv (prefix included) for an in-jail command.
func (r *Runner) argv(dir string, env map[string]string, name string, args []string) []string {
	argv := append([]string{}, r.cfg.Prefix...)
	argv = append(argv, "env")
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	argv = append(argv, r.jailEnv()...)
	argv = append(argv, envKVs(env)...)
	argv = append(argv, name)
	argv = append(argv, args...)
	return argv
}

func (r *Runner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *Runner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := r.argv(dir, env, name, args)
	return r.cfg.Host.Run(ctx, nil, argv[0], argv[1:]...)
}

func (r *Runner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := r.argv("", env, name, args)
	return r.cfg.Host.RunStdin(ctx, stdin, nil, argv[0], argv[1:]...)
}

// AttachArgv builds the host argv to attach an interactive scion command INSIDE
// the jail. It mirrors RunIn's prefix+env shape but returns the argv for the
// caller to exec() directly — interactive TTY handover can't go through the
// Runner. inner is the in-jail command (e.g. the argv from
// scion.Client.AttachArgv).
func (r *Runner) AttachArgv(inner []string) []string {
	return slices.Concat(r.cfg.Prefix, []string{"env"}, r.jailEnv(), inner)
}
