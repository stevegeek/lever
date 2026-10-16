package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HubExperimentsRecord is what lever last pinned in the jail hub's experiment
// overrides. Doctor reads it to tell whether the pin was ever made without
// an admin call; whether a feature is live it asks the hub.
//
// NotApplicable means the hub had no experiments admin route (an older scion).
// Off lists the names lever holds at an explicit false; a name this hub does
// not register is off by construction and not listed. Asked lists every name
// the pin looked for; a pinned name missing from it was never checked on this
// hub.
type HubExperimentsRecord struct {
	NotApplicable bool     `json:"not_applicable,omitempty"`
	Off           []string `json:"off,omitempty"`
	// On lists the pinned experiments lever turned on (only hub.conduit, for
	// `lever ssh`).
	On       []string  `json:"on,omitempty"`
	Asked    []string  `json:"asked,omitempty"`
	Revision int64     `json:"revision"`
	PinnedAt time.Time `json:"pinned_at"`
}

// HubExperiments sits beside remote-role.json; like it, it describes the jail
// hub's database and dies with it.
func (s State) HubExperiments() string { return filepath.Join(s.Dir, "hub-experiments.json") }

func (s State) SaveHubExperimentsRecord(r HubExperimentsRecord) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return SaveJSON(s.HubExperiments(), "hub-experiments.json", r)
}

// LoadHubExperimentsRecord reads the record; found is false when lever never
// pinned the experiments on this instance.
func (s State) LoadHubExperimentsRecord() (HubExperimentsRecord, bool, error) {
	b, err := os.ReadFile(s.HubExperiments())
	if errors.Is(err, os.ErrNotExist) {
		return HubExperimentsRecord{}, false, nil
	}
	if err != nil {
		return HubExperimentsRecord{}, false, fmt.Errorf("state: read hub-experiments.json: %w", err)
	}
	var r HubExperimentsRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return HubExperimentsRecord{}, false, fmt.Errorf("state: parse hub-experiments.json: %w", err)
	}
	return r, true, nil
}
