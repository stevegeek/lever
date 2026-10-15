package conduit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestPolicyHubEnv(t *testing.T) {
	p := Policy{DenyPorts: []int{9000, 8462, 9000}, DisableSSHSocketForwarding: true}
	env := p.HubEnv()
	want := map[string]string{
		"SCION_SERVER_HUB_CONDUIT_DISABLESSH":                 "false",
		"SCION_SERVER_HUB_CONDUIT_DENYPORTS":                  "8462,9000",
		"SCION_SERVER_HUB_CONDUIT_DISABLESSHSOCKETFORWARDING": "true",
	}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("%s = %q, want %q", k, env[k], v)
		}
		if !strings.HasPrefix(k, EnvPrefix) {
			t.Fatalf("%s is not read back by ReadLiveHub", k)
		}
	}
	if got := p.AgentValue(); got != `{"deny_ports":[8462,9000],"disable_ssh_socket_forwarding":true}` {
		t.Fatalf("agent value = %s", got)
	}
	if got := (Policy{}).AgentValue(); got != "" {
		t.Fatalf("empty policy agent value = %q, want none", got)
	}
}

func TestPolicyCovers(t *testing.T) {
	want := Policy{DenyPorts: []int{8462}, DisableSSHSocketForwarding: true}
	for _, tc := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"exact", want.HubEnv(), true},
		{"stricter", Policy{DisableSSH: true, DenyPorts: []int{22, 8462}, DisableSSHSocketForwarding: true}.HubEnv(), true},
		{"port missing", Policy{DenyPorts: []int{22}, DisableSSHSocketForwarding: true}.HubEnv(), false},
		{"sockets allowed", Policy{DenyPorts: []int{8462}}.HubEnv(), false},
		{"no env", map[string]string{}, false},
		{"one key missing", map[string]string{envConduitDenyPorts: "8462", envConduitDisableSockets: "true"}, false},
		{"unreadable bool", map[string]string{envConduitDisableSSH: "maybe", envConduitDenyPorts: "8462", envConduitDisableSockets: "true"}, false},
		{"unreadable port", map[string]string{envConduitDisableSSH: "false", envConduitDenyPorts: "8462,x", envConduitDisableSockets: "true"}, false},
		{"port out of range", map[string]string{envConduitDisableSSH: "false", envConduitDenyPorts: "8462,70000", envConduitDisableSockets: "true"}, false},
	} {
		if got := want.Covers(tc.env); got != tc.ok {
			t.Errorf("%s: Covers = %v, want %v", tc.name, got, tc.ok)
		}
	}
	if !(Policy{DisableSSH: true}).Covers(Policy{DisableSSH: true}.HubEnv()) ||
		(Policy{DisableSSH: true}).Covers(Policy{}.HubEnv()) {
		t.Fatal("a policy with SSH off is covered only by a hub with SSH off")
	}
}

func TestParseConduitProbe(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      bool
		err       bool
	}{
		{"controls", `{"files":[{"path":"/x/.scion/settings.yaml","format":"versioned","version":"1","valid":true}],"valid":true}`, true, false},
		{"noise before the JSON", "Note: something\n" + `{"files":[{"format":"versioned","valid":true}],"valid":true}`, true, false},
		{"warnings only", `{"files":[{"format":"versioned","valid":true,"warnings":["w"]}],"valid":true}`, true, false},
		{"no controls", `{"files":[{"format":"versioned","valid":false,"errors":["/server/hub/conduit: additional properties 'disable_ssh', 'deny_ports', 'disable_ssh_socket_forwarding' not allowed"]}],"valid":false}`, false, false},
		{"refused for another reason", `{"files":[{"format":"versioned","valid":false,"errors":["schema_version: bad"]}],"valid":false}`, false, true},
		{"a second file", `{"files":[{"format":"versioned","valid":true},{"format":"versioned","valid":true}],"valid":true}`, false, true},
		{"not versioned", `{"files":[{"format":"minimal","valid":true}],"valid":true}`, false, true},
		{"file valid, run not", `{"files":[{"format":"versioned","valid":true}],"valid":false}`, false, true},
		{"no valid field", `{"files":[{"format":"versioned","valid":true}]}`, false, true},
		{"no JSON", "Error: unknown command", false, true},
		{"broken JSON", `{"files":[`, false, true},
	} {
		got, err := parseConduitProbe(tc.out)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: = %v, %v; want %v, err=%v", tc.name, got, err, tc.want, tc.err)
		}
		if err != nil && !errors.Is(err, ErrProbe) {
			t.Errorf("%s: err %v is not ErrProbe", tc.name, err)
		}
	}
}

// The probe script runs scion on the probe settings in a fresh home it then
// removes, with the settings on scion's stdin path and nothing of the real
// home in reach.
func TestConduitProbeScriptRunsScionInAScratchHome(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "run")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "scion")
	script := `#!/bin/sh
[ "$(pwd -P)" = "$(cd "$HOME" && pwd -P)" ] || { echo "cwd is not HOME" >&2; exit 9; }
[ "$1 $2 $3 $4" = "--format json config validate" ] || { echo "argv $*" >&2; exit 9; }
grep -q 'deny_ports: \[8462\]' "$HOME/.scion/settings.yaml" || exit 9
echo "$HOME" > "` + filepath.Join(dir, "home") + `"
printf '{"files":[{"format":"versioned","valid":true}],"valid":true}\n'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, err := Controls(context.Background(), envRunner{proc.RealRunner{}, map[string]string{"XDG_RUNTIME_DIR": runtime}}, fake)
	if err != nil || !ok {
		t.Fatalf("ConduitControls = %v, %v", ok, err)
	}
	home, err := os.ReadFile(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSpace(string(home))
	if !strings.HasPrefix(h, runtime+"/lever-conduit-probe.") {
		t.Fatalf("scion ran with HOME %s, want a scratch dir in the runtime dir", h)
	}
	if _, err := os.Stat(h); !os.IsNotExist(err) {
		t.Fatalf("scratch home %s left behind: %v", h, err)
	}
}

type envRunner struct {
	proc.Runner
	env map[string]string
}

func (r envRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	m := map[string]string{}
	for k, v := range r.env {
		m[k] = v
	}
	for k, v := range env {
		m[k] = v
	}
	return r.Runner.RunStdin(ctx, stdin, m, name, args...)
}

func TestParseLiveHub(t *testing.T) {
	h, err := parseLiveHub("")
	if err != nil || h.Running {
		t.Fatalf("no hub: %+v %v", h, err)
	}
	h, err = parseLiveHub("123\n" + EnvPrefix + "DENYPORTS=8462\nSCION_SERVER_SESSION_SECRET=x\n")
	if err != nil || !h.Running || h.Env[EnvPrefix+"DENYPORTS"] != "8462" || len(h.Env) != 1 {
		t.Fatalf("hub: %+v %v", h, err)
	}
	if _, err := parseLiveHub("stale\n"); err == nil {
		t.Fatal("a bad pid line parsed")
	}
	if !strings.Contains(liveHubScript, "grep '^"+EnvPrefix+"'") {
		t.Fatal("the env is not filtered by an anchored prefix in the guest")
	}
}

func TestTunnelSSHArgv(t *testing.T) {
	got := TunnelSSHArgv("scion", "http://127.0.0.1:8080", "p-1", "w1")
	want := []string{"env", "SCION_HUB_ENDPOINT=http://127.0.0.1:8080", "scion", "tunnel", "--stdio", "--ssh", "--project", "p-1", "w1"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q", got)
	}
}
