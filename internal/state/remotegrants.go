package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RemoteGrants is what every remote login may do under the config the last
// `lever apply` started from: one digest per login of its tier and per-login
// settings. A running proxy admits a login only while the digest it started
// with is still here, so removing or narrowing a login takes effect as soon
// as apply writes this file, before any hub call and before the proxy
// restarts.
type RemoteGrants struct {
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`
	// Logins maps each allowed login ("" for the unnamed operator when
	// allowed_users is empty) to its grant digest.
	Logins map[string]string `json:"logins"`
}

const RemoteGrantsVersion = 1

func (s State) RemoteGrantsFile() string { return filepath.Join(s.Dir, "remote-grants.json") }

func (s State) SaveRemoteGrants(g RemoteGrants) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	g.Version = RemoteGrantsVersion
	return SaveJSON(s.RemoteGrantsFile(), "remote-grants.json", g)
}

// LoadRemoteGrants fails on a missing, unreadable or unknown-version file:
// its reader fails closed.
func (s State) LoadRemoteGrants() (RemoteGrants, error) {
	b, err := os.ReadFile(s.RemoteGrantsFile())
	if err != nil {
		return RemoteGrants{}, fmt.Errorf("state: read remote-grants.json: %w", err)
	}
	var g RemoteGrants
	if err := json.Unmarshal(b, &g); err != nil {
		return RemoteGrants{}, fmt.Errorf("state: parse remote-grants.json: %w", err)
	}
	if g.Version != RemoteGrantsVersion {
		return RemoteGrants{}, fmt.Errorf("state: remote-grants.json version %d, want %d", g.Version, RemoteGrantsVersion)
	}
	return g, nil
}
