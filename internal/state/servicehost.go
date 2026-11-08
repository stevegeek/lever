package state

import "path/filepath"

// ServiceHostDir holds the broker's client key pair for one service host
// (0700): `lever service-host pair` writes it, the broker presents it.
func (s State) ServiceHostDir(name string) string {
	return filepath.Join(s.Dir, "service-hosts", name)
}

// ServiceHostClientCert is public; ServiceHostClientKey is 0600.
func (s State) ServiceHostClientCert(name string) string {
	return filepath.Join(s.ServiceHostDir(name), "client.crt")
}

func (s State) ServiceHostClientKey(name string) string {
	return filepath.Join(s.ServiceHostDir(name), "client.key")
}

// ServiceHostDiscovery keeps a service-host tool's last good initialize and
// tools/list answers (0600), which the broker serves while the host is away.
// tool is a config tool name ([a-z0-9_-]).
func (s State) ServiceHostDiscovery(host, tool string) string {
	return filepath.Join(s.ServiceHostDir(host), "discovery-"+tool+".json")
}
