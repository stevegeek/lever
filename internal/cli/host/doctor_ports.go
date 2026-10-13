package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

type instancePortsProbes struct {
	// epoch reads /epoch on the admin port; an error when no lever broker answers.
	epoch                    func(adminPort int) (broker.EpochAnswer, error)
	dial                     dialFunc
	brokerAlive, remoteAlive bool
	// holder names the process on a loopback port ("pid 123, python3"), or ""
	// when it cannot tell.
	holder func(port int) string
}

func instancePortsProbesFor(ctx context.Context, st state.State, dp doctorProbes) instancePortsProbes {
	_, _, brokerAlive := state.PIDStatus(st.PID())
	_, _, remoteAlive := state.PIDStatus(st.RemotePID())
	client := &http.Client{Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return instancePortsProbes{
		epoch: func(port int) (broker.EpochAnswer, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return readBrokerIdentity(ctx, client, fmt.Sprintf("http://127.0.0.1:%d", port))
		},
		dial:        dp.dial,
		brokerAlive: brokerAlive,
		remoteAlive: remoteAlive,
		holder: func(port int) string {
			if dp.portHolder == nil {
				return ""
			}
			return dp.portHolder(ctx, port)
		},
	}
}

// lsofHolder is best effort: lsof may be missing or unable to see another
// user's process. The command name is the process's own and is sanitized
// where the report is printed.
func lsofHolder(ctx context.Context, r proc.Runner, port int) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nil, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc")
	if err != nil {
		return ""
	}
	var pid, cmd string
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == "":
			pid = line[1:]
		case strings.HasPrefix(line, "c") && cmd == "":
			cmd = line[1:]
		}
	}
	if pid == "" {
		return ""
	}
	if cmd == "" {
		return "pid " + pid
	}
	return "pid " + pid + ", " + cmd
}

type instancePort struct {
	port int
	what string
	// ownerAlive: this instance's process for the port runs, so a listener there
	// is very likely its own.
	ownerAlive bool
}

// instancePorts lists the host ports this instance's own processes bind: the
// broker's two, its supervised tools' backends, and the remote proxy's two
// when enabled. An external tool's port is the operator's server.
func instancePorts(app *config.App, brokerAlive, remoteAlive bool) []instancePort {
	ports := []instancePort{
		{app.EffectiveJailPort(), "broker.jail_port", brokerAlive},
		{app.EffectiveAdminPort(), "broker.admin_port", brokerAlive},
	}
	for _, t := range app.Broker.Tools {
		if t.External {
			continue
		}
		_, p, err := net.SplitHostPort(backendHostPort(t.Backend))
		if n, perr := strconv.Atoi(p); err == nil && perr == nil {
			ports = append(ports, instancePort{n, "tool " + t.Name + " backend", brokerAlive})
		}
	}
	if app.RemoteEnabled() {
		ports = append(ports,
			instancePort{app.EffectiveRemotePort(), "remote.port", remoteAlive},
			instancePort{app.EffectiveRemoteLoginPort(), "remote.login_port", remoteAlive})
	}
	return ports
}

// checkInstancePorts warns when a port this instance binds is held by another
// process; config validation sees one lever.yaml and cannot catch a second
// instance. Two signals:
//
//   - The broker on the admin port reports another instance id (or none,
//     while this instance's broker is down).
//   - Something listens on a port whose owning process of this instance is
//     not running, so the next bring-up clashes. On macOS a 127.0.0.1 bind can
//     succeed beside a wildcard listener, and the two then split the clients.
//
// A warning: the holder may be exiting, and nothing is broken yet.
func checkInstancePorts(app *config.App, st state.State, p instancePortsProbes) checkResult {
	const name = "instance ports"
	ours, err := st.ReadInstanceID()
	if err != nil && !errors.Is(err, state.ErrNoInstanceID) {
		return checkResult{name, false, err.Error(), "see the instance id file " + stateRel(st, st.InstanceIDFile())}
	}
	var held []string
	foreignBroker := false
	if got, err := p.epoch(app.EffectiveAdminPort()); err == nil {
		switch {
		case got.InstanceID != "" && got.InstanceID == ours:
			p.brokerAlive = true
		case got.InstanceID == "" && p.brokerAlive:
			return warnResult(name, fmt.Sprintf("the broker on 127.0.0.1:%d reports no instance id: it predates this lever version", app.EffectiveAdminPort()),
				"run `lever apply` to restart it with this binary")
		default:
			foreignBroker = true
			held = append(held, fmt.Sprintf("broker ports %d/%d by another lever instance's broker (instance id %s, this instance's %s)",
				app.EffectiveJailPort(), app.EffectiveAdminPort(), shortID(got.InstanceID), shortID(ours)))
		}
	}
	for _, ip := range instancePorts(app, p.brokerAlive, p.remoteAlive) {
		if ip.ownerAlive || foreignBroker && (ip.what == "broker.jail_port" || ip.what == "broker.admin_port") {
			continue
		}
		if p.dial(fmt.Sprintf("127.0.0.1:%d", ip.port)) != nil {
			continue
		}
		who := "another process"
		if h := p.holder(ip.port); h != "" {
			who = h
		}
		held = append(held, fmt.Sprintf("%s %d by %s", ip.what, ip.port, who))
	}
	if len(held) > 0 {
		return warnResult(name, "held by a process that is not this instance's: "+strings.Join(held, "; "),
			"stop that process, or give this instance its own ports (broker.jail_port, broker.admin_port, remote.port, remote.login_port, tool backends); see the operations guide on running two instances on one host")
	}
	return checkResult{name, true, "no configured port is held by another process", ""}
}
