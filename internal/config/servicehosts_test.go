package config

import (
	"strings"
	"testing"
)

const shFP = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// shCfg is baseCfg plus a service-host tool and its host.
var shCfg = baseCfg + `    - name: mail
      host: laptop
      operations:
        - {name: search}
  service_hosts:
    - name: laptop
      address: laptop.example.ts.net:7443
      fingerprint: ` + shFP + "\n"

func TestLoadServiceHostTool(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, shCfg))
	if err != nil {
		t.Fatal(err)
	}
	var mail Tool
	for _, tl := range app.Broker.Tools {
		if tl.Name == "mail" {
			mail = tl
		}
	}
	if !mail.External || mail.Host != "laptop" {
		t.Fatalf("mail = %+v, want an external tool on laptop", mail)
	}
	if h, ok := app.ServiceHost("laptop"); !ok || h.Address != "laptop.example.ts.net:7443" {
		t.Fatalf("service host = %+v %v", h, ok)
	}
	if got := app.ServiceHostTools("laptop"); len(got) != 1 || got[0].Name != "mail" {
		t.Fatalf("tools on laptop = %+v", got)
	}
	coarse := strings.Replace(shCfg, "      operations:\n        - {name: search}\n  service_hosts:", "      gate: coarse\n  service_hosts:", 1)
	if _, err := LoadNoHostChecks(writeConfig(t, coarse)); err != nil {
		t.Fatalf("a coarse service-host tool must load: %v", err)
	}
}

func TestLoadRejectsServiceHostShapeErrors(t *testing.T) {
	cases := []struct{ name, find, repl, want string }{
		{"host and backend", "host: laptop", "host: laptop\n      backend: 127.0.0.1:1", "both host and backend"},
		{"host and command", "host: laptop", "host: laptop\n      command: [x]", "both host and command"},
		{"host and allow_non_loopback", "host: laptop", "host: laptop\n      allow_non_loopback: true", "host implies"},
		{"undeclared host", "host: laptop", "host: desktop", "not declared in broker.service_hosts"},
		{"fine without operations", "      operations:\n        - {name: search}\n  service_hosts:", "  service_hosts:", "needs operations"},
		{"bad fingerprint", shFP, "sha256:abc", "fingerprint"},
		{"no fingerprint", "      fingerprint: " + shFP + "\n", "", "fingerprint"},
		{"scheme in address", "address: laptop", "address: https://laptop", "host:port"},
		{"path in address", ":7443", ":7443/mcp", "host:port"},
		{"no port", ":7443", "", "host:port"},
		{"port zero", ":7443", ":0", "1-65535"},
		{"wildcard address", "laptop.example.ts.net:7443", "0.0.0.0:7443", "single machine"},
		{"bad name", "    - name: laptop\n", "    - name: Lap/top\n", "must match"},
		{"duplicate name", "  service_hosts:\n", "  service_hosts:\n    - {name: laptop, address: \"other:1\", fingerprint: " + shFP + "}\n", "twice"},
		{"duplicate address", "  service_hosts:\n", "  service_hosts:\n    - {name: other, address: \"LAPTOP.example.ts.net:7443\", fingerprint: " + shFP + "}\n", "same address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := strings.Replace(shCfg, c.find, c.repl, 1)
			if cfg == shCfg {
				t.Fatalf("case %q changed nothing", c.name)
			}
			rejectNoHost(t, cfg, c.want)
		})
	}
}

func TestValidateRefusesHostWithoutExternal(t *testing.T) {
	err := Tool{Name: "x", Host: "laptop", Operations: []Op{{Name: "a"}}}.validate()
	if err == nil || !strings.Contains(err.Error(), "not external") {
		t.Fatalf("a host: tool built without Load must not pass as supervised: %v", err)
	}
}
