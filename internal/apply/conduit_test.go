package apply

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
)

func TestAgentGatewayPortIsTheAgentsGateway(t *testing.T) {
	_, port, err := net.SplitHostPort(agent.LocalGatewayAddr)
	if err != nil {
		t.Fatal(err)
	}
	if port != strconv.Itoa(AgentGatewayPort) {
		t.Fatalf("AgentGatewayPort = %d, the agent's gateway listens on %s", AgentGatewayPort, agent.LocalGatewayAddr)
	}
}

func TestConduitPolicy(t *testing.T) {
	off := &config.App{}
	p := ConduitPolicy(off)
	if !p.DisableSSH || !p.DisableSSHSocketForwarding || !slices.Equal(p.DenyPorts, []int{AgentGatewayPort}) {
		t.Fatalf("ssh off: %+v; want SSH off, sockets off, the gateway denied", p)
	}
	on := &config.App{SSH: config.SSH{Enabled: true, DenyPorts: []int{5432, AgentGatewayPort, 22}}}
	p = ConduitPolicy(on)
	if p.DisableSSH || !p.DisableSSHSocketForwarding || !slices.Equal(p.DenyPorts, []int{22, 5432, AgentGatewayPort}) {
		t.Fatalf("ssh on: %+v; want SSH on, sockets off, the gateway and ssh.deny_ports denied", p)
	}
}

func TestHubEnvCarriesThePolicyOnlyWithControls(t *testing.T) {
	app := &config.App{SSH: config.SSH{Enabled: true}}
	without := HubEnv("i", "fp", app, false)
	for k := range without {
		if strings.HasPrefix(k, conduit.EnvPrefix) {
			t.Fatalf("a scion without the controls gets %s", k)
		}
	}
	if len(without) != len(HubAuthEnv("i", "fp")) {
		t.Fatalf("without controls the env is not HubAuthEnv: %v", without)
	}
	with := HubEnv("i", "fp", app, true)
	if !ConduitPolicy(app).Covers(with) {
		t.Fatalf("with controls the env does not carry the policy: %v", with)
	}
	for k, v := range HubAuthEnv("i", "fp") {
		if with[k] != v {
			t.Fatalf("%s lost from the auth env", k)
		}
	}
}

// The running hub's conduit env is read through HubConduitEnv, since
// scion's RunningHub does not report it: a hub that carries lever's policy is
// kept, one that lacks it (or carries another) is restarted.
func TestScionServerComparesTheConduitPolicy(t *testing.T) {
	app := helloApp("")
	app.SSH.Enabled = true
	want := HubEnv("inst1", "sha256:fp1", app, true)
	for name, tc := range map[string]struct {
		live        map[string]string
		readErr     error
		wantRestart bool
		wantErr     bool
	}{
		"policy live":  {live: ConduitPolicy(app).HubEnv()},
		"no policy":    {live: map[string]string{}, wantRestart: true},
		"other policy": {live: ConduitPolicy(&config.App{}).HubEnv(), wantRestart: true},
		"unreadable":   {readErr: errors.New("boom"), wantErr: true},
		"stale extra key": {live: func() map[string]string {
			m := ConduitPolicy(app).HubEnv()
			m[conduit.EnvPrefix+"INTERNALLISTEN"] = ":1"
			return m
		}(), wantRestart: true},
	} {
		t.Run(name, func(t *testing.T) {
			app := helloApp(t.TempDir())
			app.SSH.Enabled = true
			f := scionOKRunner()
			scriptRunningHub(f, runningHubOut(42, matchingHubArgv(app), testHubAuthEnv()))
			sink := &logSink{}
			err := runApply(app, Deps{
				Scion:         hubScion(f, app),
				HubAuthEnv:    want,
				HubConduitEnv: func(context.Context) (map[string]string, error) { return tc.live, tc.readErr },
				EnsureHubAuth: func(context.Context) (bool, error) { return false, nil },
				Log:           sink.logf,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantErr {
				return
			}
			r := scionServerRun{f: f, logs: sink}
			wantCalls := []string{"start"}
			if tc.wantRestart {
				wantCalls = []string{"stop", "start"}
			}
			if got := r.serverCalls(); !slices.Equal(got, wantCalls) {
				t.Fatalf("server calls = %q, want %q", got, wantCalls)
			}
			got := r.startEnv(t)
			for k, v := range want {
				if got[k] != v {
					t.Errorf("start env %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
