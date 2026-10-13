package config

import (
	"strings"
	"testing"
)

// A first-party tool trusts the X-Lever-Caller header the broker sets, so
// opening its backend port to the jail would let an agent dial it directly
// and name any caller.
const firstPartyPortCfg = `
name: demo
backend: orbstack
tree: work
manager:
  allow_ports: [3201]
broker:
  llm_auth: subscription
  tools:
    - name: db
      command: [true]
      backend: 127.0.0.1:3201
      operations: [{name: read}]
`

func TestManagerAllowPortsRejectsFirstPartyToolPort(t *testing.T) {
	rejectNoHost(t, firstPartyPortCfg, "allow_ports", "3201", "db", "bypass")
}

// An external tool is the operator's own server, so its port stays allowed.
func TestManagerAllowPortsAcceptsExternalToolPort(t *testing.T) {
	cfg := `
name: demo
backend: orbstack
tree: work
manager:
  allow_ports: [3901]
broker:
  llm_auth: subscription
  tools:
    - name: qmd
      external: true
      backend: "[::1]:3901/mcp"
      operations: [{name: search}]
`
	if _, err := LoadNoHostChecks(writeConfig(t, cfg)); err != nil {
		t.Fatalf("an external tool's port in allow_ports must load: %v", err)
	}
}

func TestManagerAllowPortsAcceptsUnrelatedPortBesideFirstPartyTool(t *testing.T) {
	cfg := strings.Replace(firstPartyPortCfg, "allow_ports: [3201]", "allow_ports: [3305]", 1)
	if _, err := LoadNoHostChecks(writeConfig(t, cfg)); err != nil {
		t.Fatalf("an unrelated allow_ports entry must load: %v", err)
	}
}

func TestBackendPort(t *testing.T) {
	cases := []struct {
		backend string
		port    int
		ok      bool
	}{
		{"127.0.0.1:3201", 3201, true},
		{"[::1]:3901/mcp", 3901, true},
		{"http://127.0.0.1:3201/x", 3201, true},
		{"127.0.0.1", 0, false},
		{"", 0, false},
		{"127.0.0.1:notaport", 0, false},
	}
	for _, c := range cases {
		port, ok := backendPort(c.backend)
		if port != c.port || ok != c.ok {
			t.Errorf("backendPort(%q) = (%d, %v), want (%d, %v)", c.backend, port, ok, c.port, c.ok)
		}
	}
}
