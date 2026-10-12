package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// portsApp: broker 9443/9444, one supervised tool on 9201, one external on
// 9301, remote 9445/9447.
func portsApp(remote bool) *config.App {
	app := &config.App{Name: "a"}
	app.Broker.JailPort, app.Broker.AdminPort = 9443, 9444
	app.Broker.Tools = []config.Tool{
		{Name: "db", Command: []string{"/bin/db"}, Backend: "127.0.0.1:9201"},
		{Name: "ext", External: true, Backend: "127.0.0.1:9301/mcp"},
	}
	if remote {
		app.Remote.Enabled = true
		app.Remote.Port, app.Remote.LoginPort = 9445, 9447
	}
	return app
}

// portsProbes: listening is the set of ports something listens on; epoch
// answers with a when a is non-nil.
func portsProbes(a *broker.EpochAnswer, brokerAlive, remoteAlive bool, listening ...int) instancePortsProbes {
	on := map[string]bool{}
	for _, p := range listening {
		on[fmt.Sprintf("127.0.0.1:%d", p)] = true
	}
	return instancePortsProbes{
		epoch: func(int) (broker.EpochAnswer, error) {
			if a == nil {
				return broker.EpochAnswer{}, errNoBroker
			}
			return *a, nil
		},
		dial: func(addr string) error {
			if on[addr] {
				return nil
			}
			return errors.New("refused")
		},
		brokerAlive: brokerAlive, remoteAlive: remoteAlive,
		holder: func(port int) string {
			if port == 9201 {
				return "pid 42, python3"
			}
			return ""
		},
	}
}

func TestCheckInstancePorts(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	ours, err := st.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	own := answer(ours, "v", "h")
	other := answer(idOther, "v", "h")
	old := answer("", "v0", "")
	cases := []struct {
		name   string
		app    *config.App
		probes instancePortsProbes
		warn   bool
		want   []string
		absent []string
	}{
		{"all down, nothing listens", portsApp(true), portsProbes(nil, false, false), false, nil, nil},
		{"own broker up", portsApp(false), portsProbes(&own, true, false, 9443, 9444, 9201), false, nil, nil},
		{"own broker up, pid file gone", portsApp(false), portsProbes(&own, false, false, 9443, 9444, 9201), false, nil, nil},
		{"another instance's broker", portsApp(false), portsProbes(&other, false, false, 9443, 9444), true,
			[]string{"another lever instance's broker", shortID(idOther)}, []string{"broker.jail_port 9443 by"}},
		{"another instance's broker while ours claims to run", portsApp(false), portsProbes(&other, true, false, 9443, 9444), true,
			[]string{"another lever instance's broker"}, nil},
		{"old broker of ours", portsApp(false), portsProbes(&old, true, false, 9443, 9444), true, []string{"no instance id"}, nil},
		{"old broker, not ours", portsApp(false), portsProbes(&old, false, false, 9443, 9444), true, []string{"another lever instance's broker"}, nil},
		{"tool port held while the broker is down", portsApp(false), portsProbes(nil, false, false, 9201), true,
			[]string{"tool db backend 9201 by pid 42, python3"}, []string{"9301"}},
		{"external tool's port is its own", portsApp(false), portsProbes(nil, false, false, 9301), false, nil, nil},
		{"remote port held while the proxy is down", portsApp(true), portsProbes(nil, false, false, 9447), true,
			[]string{"remote.login_port 9447 by another process"}, nil},
		{"remote ports are the proxy's own", portsApp(true), portsProbes(nil, false, true, 9445, 9447), false, nil, nil},
		{"remote off: its ports are not this instance's", portsApp(false), portsProbes(nil, false, false, 8445), false, nil, nil},
		{"a non-lever process on the jail port", portsApp(false), portsProbes(nil, false, false, 9443), true,
			[]string{"broker.jail_port 9443 by another process"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkInstancePorts(c.app, st, c.probes)
			if !r.ok {
				t.Fatalf("a failure row: %+v", r)
			}
			if gotWarn := r.fix != ""; gotWarn != c.warn {
				t.Fatalf("warn = %v, want %v: %+v", gotWarn, c.warn, r)
			}
			for _, w := range c.want {
				if !strings.Contains(r.detail, w) {
					t.Errorf("detail %q lacks %q", r.detail, w)
				}
			}
			for _, w := range c.absent {
				if strings.Contains(r.detail, w) {
					t.Errorf("detail %q has %q", r.detail, w)
				}
			}
		})
	}
}

// A malformed instance id file fails the row: nothing can be compared.
func TestCheckInstancePortsMalformedID(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	if err := os.WriteFile(st.InstanceIDFile(), []byte("zzz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkInstancePorts(portsApp(false), st, portsProbes(nil, false, false)); r.ok {
		t.Fatalf("passed: %+v", r)
	}
}

func TestLsofHolder(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("lsof -nP -iTCP:9201", proc.Result{Stdout: "p4242\ncpython3\nf5\n"})
	if got := lsofHolder(context.Background(), r, 9201); got != "pid 4242, python3" {
		t.Fatalf("holder = %q", got)
	}
}
