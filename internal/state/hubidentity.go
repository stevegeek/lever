package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// HubAuthKey is the private key lever signs hub assertions with (package
// hubauth). It is admin-equivalent for the jail's hub; delete it to rotate.
func (s State) HubAuthKey() string { return filepath.Join(s.Dir, "hub-auth.key") }

// HubIdentityFile records which hub users and project are lever's own, so a
// later apply can tell its own rows from ones an agent made.
func (s State) HubIdentityFile() string { return filepath.Join(s.Dir, "hub-identity.json") }

const (
	HubModeFresh    = "fresh"
	HubModeMigrated = "migrated"
)

// HubIdentityVersion is the HubIdentity.Version this lever writes.
const HubIdentityVersion = 1

// MaxHubIdentityRepairs bounds HubIdentity.Repairs; SaveHubIdentity keeps the
// newest.
const MaxHubIdentityRepairs = 20

type HubIdentity struct {
	Version        int    `json:"v"`
	Mode           string `json:"mode"`
	AdminID        string `json:"admin_id"`
	ControllerID   string `json:"controller_id"`
	ProjectID      string `json:"project_id"`
	KeyFingerprint string `json:"key_fingerprint"`
	// HubMarker is the id of lever's lever-controller role definition: a
	// hub that lacks it is not the hub this record was written for.
	HubMarker string `json:"hub_marker"`
	// Users maps each lowercased remote login email to its hub user id.
	Users   map[string]string `json:"users"`
	Repairs []string          `json:"repairs,omitempty"`
	// ProbePending is set when a migration demoted or renamed the controller
	// and cleared once the controller PAT's authority was probed after it, so
	// an apply that failed in between probes on the next run.
	ProbePending bool `json:"probe_pending,omitempty"`
}

// SaveHubIdentity refuses a record LoadHubIdentity would refuse.
func (s State) SaveHubIdentity(h HubIdentity) error {
	if err := h.check(); err != nil {
		return err
	}
	if n := len(h.Repairs); n > MaxHubIdentityRepairs {
		h.Repairs = h.Repairs[n-MaxHubIdentityRepairs:]
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("state: state dir: %w", err)
	}
	return SaveJSON(s.HubIdentityFile(), "hub-identity.json", h)
}

// LoadHubIdentity reads the record with one read, as loadPATRecord. found
// is false only when the file is absent; an unreadable or malformed record
// is an error, never a fresh start.
func (s State) LoadHubIdentity() (HubIdentity, bool, error) {
	b, err := os.ReadFile(s.HubIdentityFile())
	if errors.Is(err, fs.ErrNotExist) {
		return HubIdentity{}, false, nil
	}
	if err != nil {
		return HubIdentity{}, false, fmt.Errorf("state: read hub-identity.json: %w", err)
	}
	var h HubIdentity
	if err := json.Unmarshal(b, &h); err != nil {
		return HubIdentity{}, false, fmt.Errorf("state: parse hub-identity.json: %w", err)
	}
	if err := h.check(); err != nil {
		return HubIdentity{}, false, err
	}
	return h, true, nil
}

func (h HubIdentity) check() error {
	if h.Version != HubIdentityVersion {
		return fmt.Errorf("state: hub-identity.json has version %d, want %d", h.Version, HubIdentityVersion)
	}
	if h.Mode != HubModeFresh && h.Mode != HubModeMigrated {
		return fmt.Errorf("state: hub-identity.json has mode %q, want %q or %q", h.Mode, HubModeFresh, HubModeMigrated)
	}
	return nil
}

// RemoveHubIdentity drops the record; an absent one is not an error.
func (s State) RemoveHubIdentity() error {
	return removeIfPresent(s.HubIdentityFile(), "hub-identity.json")
}

// RemoveRemotePAT deletes the remote token and its record; absent files are
// not an error.
func (s State) RemoveRemotePAT() error {
	if err := removeIfPresent(s.RemotePAT(), "remote.pat"); err != nil {
		return err
	}
	return removeIfPresent(s.RemotePATRecord(), "remote.pat.json")
}

func removeIfPresent(path, what string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: remove %s: %w", what, err)
	}
	return nil
}
