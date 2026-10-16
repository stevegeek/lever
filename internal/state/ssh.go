package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// SSHDir holds what `lever ssh` keeps on the host (0700): the agents' pinned
// host keys and the ids of the tunnel tokens it minted and has not revoked.
// Never a token itself.
func (s State) SSHDir() string { return filepath.Join(s.Dir, "ssh") }

// SSHKnownHosts is the known-hosts file `lever ssh` pins each agent's host
// key in, under its host alias.
func (s State) SSHKnownHosts() string { return filepath.Join(s.SSHDir(), "known_hosts") }

// sshTokensDir has one empty file per minted token id, so concurrent `lever
// ssh` processes never rewrite a shared file.
func (s State) sshTokensDir() string { return filepath.Join(s.SSHDir(), "tokens") }

// tokenIDRE is what a hub token id may look like as a file name.
var tokenIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// EnsureSSHDir makes SSHDir and its token directory, 0700.
func (s State) EnsureSSHDir() error {
	for _, d := range []string{s.Dir, s.SSHDir(), s.sshTokensDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

// RecordSSHToken notes a minted tunnel token's id until ForgetSSHToken.
func (s State) RecordSSHToken(id string) error {
	if !tokenIDRE.MatchString(id) {
		return fmt.Errorf("state: token id %q is not a plain id", id)
	}
	if err := s.EnsureSSHDir(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.sshTokensDir(), id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("state: record ssh token: %w", err)
	}
	return f.Close()
}

// ForgetSSHToken drops a token id; an absent one is not an error.
func (s State) ForgetSSHToken(id string) error {
	if !tokenIDRE.MatchString(id) {
		return fmt.Errorf("state: token id %q is not a plain id", id)
	}
	return removeIfPresent(filepath.Join(s.sshTokensDir(), id), "ssh token record")
}

// SSHTokens lists the recorded token ids, sorted.
func (s State) SSHTokens() ([]string, error) {
	ents, err := os.ReadDir(s.sshTokensDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: list ssh tokens: %w", err)
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && tokenIDRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
