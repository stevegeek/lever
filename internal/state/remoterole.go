package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RemoteRoleRecord is what lever knows about the hub role it granted the
// remote web UI's users: the custom project role (hub id and permissions),
// the project it is bound on, and each bound user by email with its hub user
// id.
//
// Doctor reads the hub itself; the record says what the last apply bound.
// Pending is left from older levers, which could not bind a user before its
// first sign-in.
//
// Ceilings and CeilingPermissions record the access constraint on each bound
// user, so the hub-member baseline cannot create a project (see
// ensureRemoteCeiling in internal/cli/host). A record without them reads as
// incomplete, so the next apply adds the ceiling.
type RemoteRoleRecord struct {
	RoleID      string            `json:"role_id"`
	Permissions []string          `json:"permissions"`
	ProjectID   string            `json:"project_id"`
	Bound       map[string]string `json:"bound"` // email -> hub user id
	Pending     []string          `json:"pending,omitempty"`
	Ceilings    map[string]string `json:"ceilings,omitempty"`
	// CeilingPermissions is the maximum-permission set lever wrote into
	// every ceiling.
	CeilingPermissions []string  `json:"ceiling_permissions,omitempty"`
	GrantedAt          time.Time `json:"granted_at"`
	// Contacts lists the bound emails granted as contacts: the contact role
	// (ContactRoleID, ContactPermissions) instead of the web role, and a
	// ceiling written with ContactCeilingPermissions. Every other bound
	// email is an operator.
	Contacts                  []string `json:"contacts,omitempty"`
	ContactRoleID             string   `json:"contact_role_id,omitempty"`
	ContactPermissions        []string `json:"contact_permissions,omitempty"`
	ContactCeilingPermissions []string `json:"contact_ceiling_permissions,omitempty"`
}

// RemoteRole sits beside remote.pat.json; `lever destroy` removes it with the
// tokens, since the role and bindings die with the jail hub's DB.
func (s State) RemoteRole() string { return filepath.Join(s.Dir, "remote-role.json") }

func (s State) SaveRemoteRoleRecord(r RemoteRoleRecord) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return SaveJSON(s.RemoteRole(), "remote-role.json", r)
}

// LoadRemoteRoleRecord reads the record with one read, as loadPATRecord.
// found is false when no grant has run yet.
func (s State) LoadRemoteRoleRecord() (RemoteRoleRecord, bool, error) {
	b, err := os.ReadFile(s.RemoteRole())
	if errors.Is(err, os.ErrNotExist) {
		return RemoteRoleRecord{}, false, nil
	}
	if err != nil {
		return RemoteRoleRecord{}, false, fmt.Errorf("state: read remote-role.json: %w", err)
	}
	var r RemoteRoleRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return RemoteRoleRecord{}, false, fmt.Errorf("state: parse remote-role.json: %w", err)
	}
	return r, true, nil
}
