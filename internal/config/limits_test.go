package config

import "testing"

const limitsBase = `
name: demo
backend: orbstack
tree: work
`

// Ports outside 1-65535, the broker's listeners on one port, and durations
// that cannot work are refused at load, not when a listener or iptables
// fails after the VM is up.
func TestValidateBrokerLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"allow_ports over range", "manager:\n  allow_ports: [70000]\n", []string{"manager.allow_ports", "70000"}},
		{"allow_ports zero", "manager:\n  allow_ports: [0]\n", []string{"manager.allow_ports", "0"}},
		{"jail_port over range", "broker:\n  jail_port: 70000\n", []string{"broker.jail_port", "70000"}},
		{"admin_port negative", "broker:\n  admin_port: -1\n", []string{"broker.admin_port", "-1"}},
		{"equal ports", "broker:\n  jail_port: 9500\n  admin_port: 9500\n", []string{"jail_port", "admin_port", "9500"}},
		{"negative grant_ttl", "broker:\n  grant_ttl: -1h\n", []string{"broker.grant_ttl", "negative"}},
		{"negative ticket_ttl", "broker:\n  ticket_ttl: -1m\n", []string{"broker.ticket_ttl", "negative"}},
		{"short grant_ttl with api-key", "broker:\n  llm_auth: api-key\n  api_key_file: /k\n  grant_ttl: 1h\n", []string{"broker.grant_ttl", "renewal"}},
		{"remote port negative", "remote:\n  enabled: true\n  port: -5\n", []string{"remote.port", "-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) { rejectNoHost(t, limitsBase+tc.body, tc.want...) })
	}
}

func TestValidateBrokerLimitsAccepts(t *testing.T) {
	for _, body := range []string{
		"manager:\n  allow_ports: [1, 65535]\n",
		"broker:\n  llm_auth: subscription\n  grant_ttl: 1h\n", // no api-key agent
		"broker:\n  llm_auth: api-key\n  api_key_file: /k\n  grant_ttl: 24h\n",
	} {
		if _, err := LoadNoHostChecks(writeConfig(t, limitsBase+body)); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
	}
}
