// Package conduit is lever's side of scion's hub relay (conduit) for `lever
// ssh`: the hub's conduit policy as lever starts the hub with it, the probe
// for a jail scion that has the policy settings, the read-back of the policy
// from the running hub, and the in-jail tunnel argv. It runs only on the
// host side.
package conduit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// Policy is the hub's restriction on its conduit relay
// (server.hub.conduit.disable_ssh, deny_ports and
// disable_ssh_socket_forwarding). The hub enforces it on tunnels and the port
// proxy, and hands it to every agent it starts as SCION_CONDUIT_POLICY, where
// sciontool enforces it on SSH forwarding, which the hub cannot see.
type Policy struct {
	DisableSSH                 bool
	DenyPorts                  []int
	DisableSSHSocketForwarding bool
}

// The hub's environment names for the policy. scion maps a compound setting
// only when written without inner underscores; an underscored spelling is
// silently ignored.
const (
	EnvPrefix                = "SCION_SERVER_HUB_CONDUIT_"
	envConduitDisableSSH     = EnvPrefix + "DISABLESSH"
	envConduitDenyPorts      = EnvPrefix + "DENYPORTS"
	envConduitDisableSockets = EnvPrefix + "DISABLESSHSOCKETFORWARDING"
)

// AgentPolicyEnv is the variable the hub sets on every agent it starts.
const AgentPolicyEnv = "SCION_CONDUIT_POLICY"

// Normalized returns p with its ports sorted and unique.
func (p Policy) Normalized() Policy {
	ports := slices.Clone(p.DenyPorts)
	slices.Sort(ports)
	p.DenyPorts = slices.Compact(ports)
	return p
}

// HubEnv is the policy as the environment of a hub start. Env, not
// settings.yaml: scion lets SCION_SERVER_* override the file, which agents can
// rewrite through the workstation routes.
func (p Policy) HubEnv() map[string]string {
	p = p.Normalized()
	ports := make([]string, len(p.DenyPorts))
	for i, n := range p.DenyPorts {
		ports[i] = strconv.Itoa(n)
	}
	return map[string]string{
		envConduitDisableSSH:     strconv.FormatBool(p.DisableSSH),
		envConduitDenyPorts:      strings.Join(ports, ","),
		envConduitDisableSockets: strconv.FormatBool(p.DisableSSHSocketForwarding),
	}
}

// Covers reports whether a hub started with env enforces at least want: SSH
// off if want has it off, socket forwarding off if want has it off, and every
// port want denies. A value lever cannot read counts as not covering.
func (want Policy) Covers(env map[string]string) bool {
	got, ok := conduitPolicyFromEnv(env)
	if !ok {
		return false
	}
	if want.DisableSSH && !got.DisableSSH {
		return false
	}
	if want.DisableSSHSocketForwarding && !got.DisableSSHSocketForwarding {
		return false
	}
	for _, p := range want.DenyPorts {
		if !slices.Contains(got.DenyPorts, p) {
			return false
		}
	}
	return true
}

func conduitPolicyFromEnv(env map[string]string) (Policy, bool) {
	var p Policy
	for _, k := range []string{envConduitDisableSSH, envConduitDenyPorts, envConduitDisableSockets} {
		if _, ok := env[k]; !ok {
			return Policy{}, false
		}
	}
	var err error
	if p.DisableSSH, err = strconv.ParseBool(env[envConduitDisableSSH]); err != nil {
		return Policy{}, false
	}
	if p.DisableSSHSocketForwarding, err = strconv.ParseBool(env[envConduitDisableSockets]); err != nil {
		return Policy{}, false
	}
	if s := strings.TrimSpace(env[envConduitDenyPorts]); s != "" {
		for _, f := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 || n > 65535 {
				return Policy{}, false
			}
			p.DenyPorts = append(p.DenyPorts, n)
		}
	}
	return p, true
}

// AgentValue is the SCION_CONDUIT_POLICY value a hub started with HubEnv
// hands its agents (pkg/conduit Policy's JSON).
func (p Policy) AgentValue() string {
	p = p.Normalized()
	b, _ := json.Marshal(struct {
		DisableSSH                 bool  `json:"disable_ssh,omitempty"`
		DenyPorts                  []int `json:"deny_ports,omitempty"`
		DisableSSHSocketForwarding bool  `json:"disable_ssh_socket_forwarding,omitempty"`
	}{p.DisableSSH, p.DenyPorts, p.DisableSSHSocketForwarding})
	if string(b) == "{}" {
		return ""
	}
	return string(b)
}

// conduitProbeSettings exercises every policy key. The schema of a scion
// without the controls refuses each of them (the conduit block allows no
// unknown keys).
const conduitProbeSettings = `schema_version: "1"
server:
  hub:
    conduit:
      disable_ssh: true
      deny_ports: [8462]
      disable_ssh_socket_forwarding: true
`

// conduitProbeScript validates conduitProbeSettings with the jail's scion in
// a fresh private home, so neither the run user's settings nor a project
// takes part, and removes it after.
const conduitProbeScript = `set -u
d=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/lever-conduit-probe.XXXXXX") || exit 70
trap 'rm -rf -- "$d"' EXIT
mkdir -m 700 "$d/.scion" && cat > "$d/.scion/settings.yaml" || exit 70
cd "$d" && HOME="$d" "$1" --format json config validate
`

// ErrProbe is a probe whose answer lever could not read; callers treat it as
// no support.
var ErrProbe = errors.New("could not tell whether the jail's scion has the hub's conduit controls")

// Controls reports whether the jail's scion (bin, run through the jail runner
// r) knows the hub's conduit policy settings, by having it validate settings
// that use them. It runs the CLI only, never the hub.
func Controls(ctx context.Context, r proc.Runner, bin string) (bool, error) {
	res, err := r.RunStdin(ctx, strings.NewReader(conduitProbeSettings), nil, "sh", "-c", conduitProbeScript, "_", bin)
	if res.Code == 70 {
		return false, fmt.Errorf("%w: the probe could not write its scratch settings", ErrProbe)
	}
	supported, perr := parseConduitProbe(res.Stdout)
	if perr != nil {
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrProbe, err)
		}
		return false, perr
	}
	return supported, nil
}

// conduitProbeKeys are the settings whose refusal means "no controls".
var conduitProbeKeys = []string{"disable_ssh", "deny_ports", "disable_ssh_socket_forwarding"}

func parseConduitProbe(out string) (bool, error) {
	i := strings.IndexByte(out, '{')
	if i < 0 {
		return false, fmt.Errorf("%w: scion config validate printed no JSON", ErrProbe)
	}
	var v struct {
		Files []struct {
			Format string   `json:"format"`
			Valid  bool     `json:"valid"`
			Errors []string `json:"errors"`
		} `json:"files"`
		Valid *bool `json:"valid"`
	}
	if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&v); err != nil || v.Valid == nil {
		return false, fmt.Errorf("%w: unreadable scion config validate output", ErrProbe)
	}
	if len(v.Files) != 1 || v.Files[0].Format != "versioned" {
		return false, fmt.Errorf("%w: scion config validate did not validate the probe's settings", ErrProbe)
	}
	f := v.Files[0]
	if *v.Valid && f.Valid && len(f.Errors) == 0 {
		return true, nil
	}
	for _, e := range f.Errors {
		for _, k := range conduitProbeKeys {
			if strings.Contains(e, k) {
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("%w: scion refused the probe's settings for another reason", ErrProbe)
}

// LiveHub is the running hub's conduit environment. Running is false when no
// scion server runs.
type LiveHub struct {
	Running bool
	Env     map[string]string
}

// liveHubScript prints the server's pid, then only its EnvPrefix entries,
// filtered in the guest: the daemon's environment can hold the session secret
// and a hub token. Agents can write the pid file (scion's workstation routes
// write under $HOME) and run processes the guest sees as the jail user, so
// the process must also be the root-installed scion ($2) in the reader's own
// pid and user namespaces, not a decoy in a container; anything else reads as
// no hub.
const liveHubScript = `f="$HOME/$1"
[ -f "$f" ] && [ ! -L "$f" ] || exit 0
pid=$(tr -cd '0-9' < "$f")
[ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ] || exit 0
[ "$(readlink "/proc/$pid/exe")" = "$2" ] || exit 0
[ "$(readlink "/proc/$pid/ns/pid")" = "$(readlink /proc/self/ns/pid)" ] || exit 0
[ "$(readlink "/proc/$pid/ns/user")" = "$(readlink /proc/self/ns/user)" ] || exit 0
case "$(tr '\000' ' ' < "/proc/$pid/cmdline")" in
*scion*" server "*) ;;
*) exit 0 ;;
esac
printf '%s\n' "$pid"
tr '\000' '\n' < "/proc/$pid/environ" | grep '^` + EnvPrefix + `' || true
`

// ScionPath is where lever installs the jail's scion, root-owned.
const ScionPath = "/usr/local/bin/scion"

// ReadLiveHub reads the running hub's conduit environment from /proc in the
// jail.
func ReadLiveHub(ctx context.Context, r proc.Runner) (LiveHub, error) {
	res, err := r.Run(ctx, nil, "sh", "-c", liveHubScript, "_", layout.ServerPIDRel, ScionPath)
	if err != nil {
		return LiveHub{}, fmt.Errorf("reading the running hub's conduit settings: %w", err)
	}
	return parseLiveHub(res.Stdout)
}

func parseLiveHub(out string) (LiveHub, error) {
	first, rest, _ := strings.Cut(out, "\n")
	first = strings.TrimSpace(first)
	if first == "" {
		return LiveHub{}, nil
	}
	if _, err := strconv.Atoi(first); err != nil {
		return LiveHub{}, fmt.Errorf("running hub probe printed %q", first)
	}
	h := LiveHub{Running: true, Env: map[string]string{}}
	for _, kv := range strings.Split(rest, "\n") {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, EnvPrefix) {
			h.Env[k] = v
		}
	}
	return h, nil
}

// TunnelSSHArgv is the in-jail argv of an SSH tunnel to agent on stdin and
// stdout (scion tunnel --stdio --ssh), for an ssh ProxyCommand. It pins the
// hub endpoint and names the project by its hub id, so neither the working
// directory nor the jail's project config picks them, and carries no token.
func TunnelSSHArgv(bin, endpoint, projectID, agent string) []string {
	return []string{"env", "SCION_HUB_ENDPOINT=" + endpoint, bin, "tunnel", "--stdio", "--ssh", "--project", projectID, agent}
}
