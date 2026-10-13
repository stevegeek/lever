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
// kept beside the token. The hub tells a token holder nothing about it (only
// its issuer's session may list or revoke it), so this record is
// the only way apply or doctor can judge the token: the scopes lever asked
// for (to detect drift after an upgrade), the scopes granted
// (informational), mint and expiry times (zero expiry when scion printed
// none), and the hub's id so the next mint can revoke it.
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
// when none exists (a token placed by hand, say), which callers must treat as
// "cannot vouch for this token", never as an empty record.
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

// loadPATRecord decides absent and present with one read: a stat-then-read
// could return a record deleted in between as found and empty.
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
