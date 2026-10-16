package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Lifecycle is the instance's one record of what lever last observed and
// converged: the guest boot the manager last ran live in, and the
// fingerprints of the bring-up steps a converge may skip.
//
// Only a holder of the lifecycle lock writes it, and only while the state
// directory is outside the tree: inside it an agent could forge it, so
// callers neither read nor write it there. Nothing in it stands in for a
// security fence. A fingerprint lets a step skip work only when the step
// still checks its own postcondition; a lost or corrupt record only costs
// work done again.
type Lifecycle struct {
	Version   int              `json:"version"`
	GuestBoot *GuestBootRecord `json:"guest_boot,omitempty"`
	// Steps maps a step key to the fingerprint of the inputs it last
	// converged on.
	Steps map[string]Fingerprint `json:"steps,omitempty"`
}

// GuestBootRecord is the jail guest boot id in which lever last saw the
// manager live.
type GuestBootRecord struct {
	BootID     string    `json:"boot_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Fingerprint is a digest of a step's inputs (HashJSON) and when the step
// last converged on them.
type Fingerprint struct {
	Inputs string    `json:"inputs"`
	At     time.Time `json:"at"`
}

// LifecycleVersion is the record's format.
const LifecycleVersion = 1

// errLifecycleNewer: a record a newer lever wrote is left as it is.
var errLifecycleNewer = errors.New("lifecycle.json was written by a newer lever")

func (s State) LifecycleFile() string { return filepath.Join(s.Dir, "lifecycle.json") }

// legacyGuestBoot is the guest boot record before lifecycle.json held it.
func (s State) legacyGuestBoot() string { return filepath.Join(s.Dir, "guest-boot.json") }

// lifecycleMu serialises the read-modify-write of the record within one
// process: the bring-up runs steps on several lanes at once.
var lifecycleMu sync.Mutex

// LoadLifecycle reads the record. A record the file cannot give (absent,
// corrupt) is the zero record with the error, so a caller that only reads
// treats it as nothing recorded. The guest boot of a record that has none
// comes from the legacy guest-boot.json while that file remains.
func (s State) LoadLifecycle() (Lifecycle, error) {
	var l Lifecycle
	b, err := os.ReadFile(s.LifecycleFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return Lifecycle{}, fmt.Errorf("state: read lifecycle.json: %w", err)
	default:
		if err := json.Unmarshal(b, &l); err != nil {
			return Lifecycle{}, fmt.Errorf("state: parse lifecycle.json: %w", err)
		}
		if l.Version > LifecycleVersion {
			return Lifecycle{}, errLifecycleNewer
		}
	}
	if l.GuestBoot == nil {
		if old, err := LoadJSON[GuestBootRecord](s.legacyGuestBoot(), "guest boot record"); err == nil && old.BootID != "" {
			l.GuestBoot = &old
		}
	}
	return l, nil
}

// UpdateLifecycle applies fn to the record and writes it atomically (0600).
// A corrupt record is replaced; one a newer lever wrote is not. Once written,
// the legacy guest-boot.json it took over is removed.
func (s State) UpdateLifecycle(fn func(*Lifecycle)) error {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	l, err := s.LoadLifecycle()
	if errors.Is(err, errLifecycleNewer) {
		return err
	}
	if err != nil {
		l = Lifecycle{}
	}
	fn(&l)
	l.Version = LifecycleVersion
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("state: write lifecycle.json: %w", err)
	}
	if err := SaveJSON(s.LifecycleFile(), "lifecycle.json", l); err != nil {
		return err
	}
	if err := os.Remove(s.legacyGuestBoot()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: remove the migrated guest-boot.json: %w", err)
	}
	return nil
}

// StepMatches reports whether step last converged on inputs. Any doubt (no
// record, an unreadable one, an empty digest) is false.
func (s State) StepMatches(step, inputs string) bool {
	if inputs == "" {
		return false
	}
	l, err := s.LoadLifecycle()
	if err != nil {
		return false
	}
	fp, ok := l.Steps[step]
	return ok && fp.Inputs == inputs
}

// RecordStep notes that step converged on inputs at now.
func (s State) RecordStep(step, inputs string, now time.Time) error {
	return s.UpdateLifecycle(func(l *Lifecycle) {
		if l.Steps == nil {
			l.Steps = map[string]Fingerprint{}
		}
		l.Steps[step] = Fingerprint{Inputs: inputs, At: now.UTC()}
	})
}

// ForgetStep drops step's fingerprint, before the work it vouched for is
// undone or redone.
func (s State) ForgetStep(step string) error {
	l, err := s.LoadLifecycle()
	if err == nil {
		if _, ok := l.Steps[step]; !ok {
			return nil
		}
	}
	return s.UpdateLifecycle(func(l *Lifecycle) { delete(l.Steps, step) })
}
