// Package jail provides a proc.Runner that executes commands inside a jail
// via a backend-supplied argv prefix (["orb","-m",m,"-u",u] for OrbStack,
// ["limactl","shell",vm] for Lima) followed by `env [-C dir] K=V… cmd args`.
// GNU env needs no shell quoting, so scion.Client runs unchanged inside the
// jail. The controller PAT (SCION_HUB_TOKEN) never goes on the host argv; see
// hubtoken.go.
package jail

import (
	"context"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

var _ proc.Runner = (*Runner)(nil)

// ForceHostNetworkEnv is a debugging escape hatch that makes in-jail scion
// commands run agents under --network=host. It is not isolation-safe; see
// Config.ForceHostNetwork.
const ForceHostNetworkEnv = "LEVER_FORCE_HOST_NETWORK"

// ForceHostNetworkFromEnv is the only place ForceHostNetworkEnv is read. Any
// value that does not parse as true stays off, so a surprising value on this
// security knob never re-opens the shared loopback.
func ForceHostNetworkFromEnv() bool {
	force, _ := strconv.ParseBool(os.Getenv(ForceHostNetworkEnv))
	return force
}

// Config is everything a Runner needs to reach one jail.
type Config struct {
	Host   proc.Runner // the prefix binary runs on the host
	Prefix []string
	UID    string // run-user uid, for XDG_RUNTIME_DIR
	// ForceHostNetwork re-emits scion's SCION_FORCE_HOST_NETWORK so agents run
	// under --network=host instead of their own pasta netns. The per-agent
	// netns keeps each container's 127.0.0.1 private, which is what isolates
	// one agent's in-container gateway proxy (127.0.0.1:8462) from the others:
	// under a shared netns any agent could reach another's gateway and act as
	// it. The hub stays reachable through the guest's pasta mapping of
	// 169.254.1.2 to the VM loopback (guest.PastaHostAddr).
	ForceHostNetwork bool
	// OutputLimit caps each of stdout and stderr of a command whose context
	// carries no cap of its own (proc.WithOutputLimit); 0 leaves it uncapped.
	OutputLimit int
}

type Runner struct {
	cfg Config
}

func New(cfg Config) *Runner {
	return &Runner{cfg: cfg}
}

func (r *Runner) jailEnv() []string {
	env := []string{
		"XDG_RUNTIME_DIR=/run/user/" + r.cfg.UID,
		"PATH=/usr/local/bin:/usr/bin:/bin",
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
	sort.Strings(kvs)
	return kvs
}

// argv returns the hub token it withheld from the argv. With a token the
// shape is
//
//	<prefix> env <jailEnv> sh -c <hubTokenStdinScript> _ env [-C dir] K=V… cmd args
func (r *Runner) argv(dir string, env map[string]string, name string, args []string) ([]string, string, error) {
	tok := env[hubTokenEnv]
	if err := checkHubToken(tok); err != nil {
		return nil, "", err
	}
	argv := append([]string{}, r.cfg.Prefix...)
	if tok != "" {
		env = maps.Clone(env)
		delete(env, hubTokenEnv)
		argv = append(argv, "env")
		argv = append(argv, r.jailEnv()...)
		argv = append(argv, "sh", "-c", hubTokenStdinScript, "_")
	}
	argv = append(argv, "env")
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	if tok == "" {
		argv = append(argv, r.jailEnv()...)
	}
	argv = append(argv, envKVs(env)...)
	argv = append(argv, name)
	argv = append(argv, args...)
	return argv, tok, nil
}

// limitOutput applies cfg.OutputLimit to a ctx that carries no cap of its
// own: a caller that legitimately reads more, such as a conversation
// capture, sets its own cap, which wins.
func (r *Runner) limitOutput(ctx context.Context) context.Context {
	if _, ok := proc.OutputLimit(ctx); ok || r.cfg.OutputLimit <= 0 {
		return ctx
	}
	return proc.WithOutputLimit(ctx, r.cfg.OutputLimit)
}

func (r *Runner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *Runner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv, tok, err := r.argv(dir, env, name, args)
	if err != nil {
		return proc.Result{}, err
	}
	ctx = r.limitOutput(ctx)
	if tok == "" {
		return r.cfg.Host.Run(ctx, nil, argv[0], argv[1:]...)
	}
	return r.cfg.Host.RunStdin(ctx, strings.NewReader(tok+"\n"), nil, argv[0], argv[1:]...)
}

func (r *Runner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv, tok, err := r.argv("", env, name, args)
	if err != nil {
		return proc.Result{}, err
	}
	if tok != "" {
		// The token line leads; the payload follows on the same stream.
		line := strings.NewReader(tok + "\n")
		if stdin == nil {
			stdin = line
		} else {
			stdin = io.MultiReader(line, stdin)
		}
	}
	return r.cfg.Host.RunStdin(r.limitOutput(ctx), stdin, nil, argv[0], argv[1:]...)
}

// AttachArgv builds the host argv to exec an interactive command inside the
// jail; a TTY handover cannot go through the Runner. inner must carry no hub
// token: a caller that needs one stages it with StageHubToken and wraps inner
// in WithHubTokenFromFile, so the host argv stays secret-free.
func (r *Runner) AttachArgv(inner []string) []string {
	return slices.Concat(r.cfg.Prefix, []string{"env"}, r.jailEnv(), inner)
}
