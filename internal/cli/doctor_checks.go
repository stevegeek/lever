package cli

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
)

// checkResult is one diagnostic outcome. detail is shown in both the pass and
// fail lines; fix is a remediation hint shown only on failure.
type checkResult struct {
	name   string
	ok     bool
	detail string
	fix    string
}

// dialFunc probes a TCP address, returning nil if something is listening. It is
// injected so the checks are unit-testable without real listeners.
type dialFunc func(addr string) error

// tcpDial is the production dialFunc: a short-timeout TCP connect, closed at once.
func tcpDial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// checkBrokerAlive verifies the recorded broker process is alive AND actually
// listening on the jail port. It distinguishes three failure modes so the fix
// is unambiguous: never started, died (stale pid), and alive-but-not-serving.
func checkBrokerAlive(st brokerctl.State, jailPort int, dial dialFunc) checkResult {
	const name = "broker running"
	pid, found, alive := st.BrokerPIDStatus()
	switch {
	case !found:
		return checkResult{name, false, "no broker.pid — the broker was never started (or was cleanly stopped)", "run `lever apply` or `lever up`"}
	case !alive:
		return checkResult{name, false, fmt.Sprintf("broker.pid names pid %d, but that process is gone (stale pid file)", pid), "run `lever apply` or `lever up`"}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", jailPort)
	if err := dial(addr); err != nil {
		return checkResult{name, false, fmt.Sprintf("pid %d is alive but nothing is listening on %s", pid, addr), "inspect .lever-state/broker.log, then restart with `lever apply`"}
	}
	return checkResult{name, true, fmt.Sprintf("pid %d, serving on %s", pid, addr), ""}
}

// checkExternalBackends verifies every external tool's backend is listening.
// External servers are fronted, not spawned, so a down one surfaces only as a
// gateway 502 at call time — this turns that into an up-front diagnosis.
func checkExternalBackends(tools []config.Tool, dial dialFunc) checkResult {
	const name = "external tool backends"
	var down []string
	probed := 0
	for _, t := range tools {
		if !t.External {
			continue
		}
		probed++
		addr := backendHostPort(t.Backend)
		if err := dial(addr); err != nil {
			down = append(down, fmt.Sprintf("%s (%s)", t.Name, addr))
		}
	}
	switch {
	case probed == 0:
		return checkResult{name, true, "no external tools declared", ""}
	case len(down) > 0:
		return checkResult{name, false, "not listening: " + strings.Join(down, ", "), "start the server(s) (e.g. your MCP launcher); each must listen on its loopback backend"}
	default:
		return checkResult{name, true, fmt.Sprintf("%d reachable", probed), ""}
	}
}

// backendHostPort strips an optional path from a "host:port[/path]" backend,
// leaving the "host:port" a TCP dial needs. External backends are validated
// scheme-less, so no scheme handling is required.
func backendHostPort(backend string) string {
	if i := strings.IndexByte(backend, '/'); i >= 0 {
		return backend[:i]
	}
	return backend
}
