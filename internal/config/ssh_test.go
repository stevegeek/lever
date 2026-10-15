package config

import (
	"strings"
	"testing"
)

func TestSSHDenyPortsValidated(t *testing.T) {
	for _, tc := range []struct {
		ports []int
		err   string
	}{
		{nil, ""},
		{[]int{22, 5432}, ""},
		{[]int{0}, "port 0"},
		{[]int{65536}, "port 65536"},
	} {
		a := &App{SSH: SSH{Enabled: true, DenyPorts: tc.ports}}
		err := a.validateSSH()
		if (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%v: err = %v, want %q", tc.ports, err, tc.err)
		}
	}
}

func TestSSHOffByDefault(t *testing.T) {
	if (&App{}).SSHEnabled() {
		t.Fatal("ssh is on in an empty config")
	}
}

func TestSSHBlockLoads(t *testing.T) {
	p := writeConfig(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nssh:\n  enabled: true\n  deny_ports: [5432]\n")
	app, err := LoadNoHostChecks(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !app.SSHEnabled() || len(app.SSH.DenyPorts) != 1 || app.SSH.DenyPorts[0] != 5432 {
		t.Fatalf("ssh = %+v", app.SSH)
	}
	p = writeConfig(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nssh:\n  enabled: true\n  deny_ports: [0]\n")
	if _, err := LoadNoHostChecks(p); err == nil || !strings.Contains(err.Error(), "ssh.deny_ports") {
		t.Fatalf("a bad deny port loads: %v", err)
	}
}
