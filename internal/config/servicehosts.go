package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// serviceHostNameRE: the name is a directory under the state directory.
var serviceHostNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var serviceHostFingerprintRE = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

// markServiceHostTools makes every host: tool external, which host implies.
func (a *App) markServiceHostTools() {
	for i := range a.Broker.Tools {
		if a.Broker.Tools[i].Host != "" {
			a.Broker.Tools[i].External = true
		}
	}
}

// ServiceHost returns the declared service host called name.
func (a *App) ServiceHost(name string) (ServiceHost, bool) {
	for _, h := range a.Broker.ServiceHosts {
		if h.Name == name {
			return h, true
		}
	}
	return ServiceHost{}, false
}

// ServiceHostTools lists the broker tools that run on service host name.
func (a *App) ServiceHostTools(name string) []Tool {
	var out []Tool
	for _, t := range a.Broker.Tools {
		if t.Host == name {
			out = append(out, t)
		}
	}
	return out
}

func (a *App) validateServiceHosts() error {
	names, addrs := map[string]bool{}, map[string]string{}
	for _, h := range a.Broker.ServiceHosts {
		if !serviceHostNameRE.MatchString(h.Name) {
			return fmt.Errorf("config: broker.service_hosts name %q must match %s", h.Name, serviceHostNameRE)
		}
		if names[h.Name] {
			return fmt.Errorf("config: broker.service_hosts declares %q twice", h.Name)
		}
		names[h.Name] = true
		if err := validateServiceHostAddress(h.Address); err != nil {
			return fmt.Errorf("config: broker.service_hosts %q: %w", h.Name, err)
		}
		key := strings.ToLower(h.Address)
		if other, dup := addrs[key]; dup {
			return fmt.Errorf("config: broker.service_hosts %q and %q have the same address %s", other, h.Name, h.Address)
		}
		addrs[key] = h.Name
		if !serviceHostFingerprintRE.MatchString(h.Fingerprint) {
			return fmt.Errorf("config: broker.service_hosts %q fingerprint %q: want sha256: and 64 hex digits (lever service-host init prints it on the service host)", h.Name, h.Fingerprint)
		}
	}
	for _, t := range a.Broker.Tools {
		if t.Host != "" && !names[t.Host] {
			return fmt.Errorf("config: broker tool %q has host %q, which is not declared in broker.service_hosts", t.Name, t.Host)
		}
	}
	return nil
}

// validateServiceHostAddress wants host:port with a fixed port and nothing
// else: the broker builds the URL itself.
func validateServiceHostAddress(addr string) error {
	if strings.ContainsAny(addr, "/?#@ ") || strings.Contains(addr, "://") {
		return fmt.Errorf("address %q: want host:port (no scheme, path or user)", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: %v (want host:port)", addr, err)
	}
	if host == "" {
		return fmt.Errorf("address %q has no host", addr)
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return fmt.Errorf("address %q is not a single machine", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("address %q: port must be 1-65535", addr)
	}
	return nil
}
