package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PATRecord is what lever knows about a hub user access token it minted,
// kept beside the token itself. The token is opaque and the hub will not
// tell a UAT holder anything about it (only the dev identity in the mint
// window may list or revoke tokens), so this record is the only way a later
// `lever apply` or `lever doctor` can judge whether the token is still the one
// lever wants: the scopes lever ASKED for (compared against the current
// constant to detect drift after an upgrade), the scopes the hub GRANTED
// (informational — an alias is stored expanded), when it was minted, when it
// expires (zero when scion printed no expiry), and the hub's id for it so the
// next mint window can revoke it.
type PATRecord struct {
	ID        string    `json:"id,omitempty"`
	Requested []string  `json:"requested_scopes"`
	Granted   []string  `json:"granted_scopes,omitempty"`
	MintedAt  time.Time `json:"minted_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// ControllerPATRecord and RemotePATRecord sit beside controller.pat and
// remote.pat; `lever destroy` removes them with the tokens.
func (s State) ControllerPATRecord() string { return filepath.Join(s.Dir, "controller.pat.json") }
func (s State) RemotePATRecord() string     { return filepath.Join(s.Dir, "remote.pat.json") }

func (s State) SaveControllerPATRecord(r PATRecord) error {
	return savePATRecord(s.ControllerPATRecord(), "controller.pat.json", r)
}

func (s State) SaveRemotePATRecord(r PATRecord) error {
	return savePATRecord(s.RemotePATRecord(), "remote.pat.json", r)
}

// LoadControllerPATRecord reads the controller token's record. found is false
// when no record exists — a token minted by a lever that predates records, or
// one the operator placed by hand — which callers must treat as "cannot vouch
// for this token", never as a record that happens to be empty.
func (s State) LoadControllerPATRecord() (PATRecord, bool, error) {
	return loadPATRecord(s.ControllerPATRecord(), "controller.pat.json")
}

func (s State) LoadRemotePATRecord() (PATRecord, bool, error) {
	return loadPATRecord(s.RemotePATRecord(), "remote.pat.json")
}

func savePATRecord(path, what string, r PATRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return SaveJSON(path, what, r)
}

// loadPATRecord is one read, so "absent" and "present" are decided by the
// same syscall — a stat-then-read would let a record deleted in between come
// back as found and empty, which would vouch for nothing while claiming to.
func loadPATRecord(path, what string) (PATRecord, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PATRecord{}, false, nil
	}
	if err != nil {
		return PATRecord{}, false, fmt.Errorf("state: read %s: %w", what, err)
	}
	var r PATRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return PATRecord{}, false, fmt.Errorf("state: parse %s: %w", what, err)
	}
	return r, true, nil
}
