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
// remote web UI's users: the custom project role (its hub id and the
// permission set lever wrote into it), the project it is bound on, and each
// allowed user it bound, by email, with the hub user id the binding names.
//
// It is the only way `lever apply` and `lever doctor` can tell the grant is
// done: the grant needs the throwaway dev-auth window (role admin is
// hub-admin only), so neither can ask the hub. Pending lists the allowed
// users that had no hub user yet at grant time (never signed in); a record
// with any is incomplete, and the next apply retries them.
//
// Ceilings and CeilingPermissions record the access constraint lever puts on
// each bound user, so the hub-member baseline every hub user holds cannot
// create a project (see ensureRemoteCeiling in internal/cli/host). A record
// written before the ceiling existed has neither field; it reads as
// incomplete, so the next apply opens the window and adds the ceiling.
type RemoteRoleRecord struct {
	RoleID      string            `json:"role_id"`
	Permissions []string          `json:"permissions"`
	ProjectID   string            `json:"project_id"`
	Bound       map[string]string `json:"bound"` // email -> hub user id
	Pending     []string          `json:"pending,omitempty"`
	// Ceilings maps each bound email to the id of its access constraint.
	Ceilings map[string]string `json:"ceilings,omitempty"`
	// CeilingPermissions is the maximum-permission set lever wrote into
	// every ceiling.
	CeilingPermissions []string  `json:"ceiling_permissions,omitempty"`
	GrantedAt          time.Time `json:"granted_at"`
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

// LoadRemoteRoleRecord reads the record. found is false when none exists: no
// grant has run yet. One read decides both, as for loadPATRecord.
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
