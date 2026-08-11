// Package wire holds the on-the-wire/on-disk envelope types shared across the
// host, the broker and the in-container agent — the ONE place their JSON
// contract is declared, so a tag can never drift between a producer and a
// consumer that live in different packages. It is a leaf: it imports nothing
// internal, so any package (including internal/agent, whose tests import
// internal/broker) can depend on it without an import cycle.
package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Bootstrap is the enrolment envelope the host/manager (for the manager) or the
// broker (for a worker, and on auto-re-enrol) deposits as bootstrap.json for an
// agent's lever-agent boot to consume. Previously this 4-field struct was
// declared three times (agent, apply, broker) with hand-maintained,
// "must-stay-identical" json tags; this is now its single declaration.
type Bootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

// Stage writes b as <root>/<rel>/bootstrap.json — the deposit LoadBootstrap
// reads by convention — creating the directory 0700 and the file 0600. This is
// the single staging path for the envelope, shared by the host (manager
// enrolment) and the broker (worker dispatch + auto-re-enrol, which re-stages a
// fresh ticket over the existing file on every resume).
//
// EVERYTHING under root is agent-writable, and the process calling this is the
// HOST, running as the operator. So rel names a path an adversary controls: an
// agent that replaces its `.lever` with a symlink is asking the host to write,
// and chmod 0600, wherever the link points — the instance root's lever.yaml,
// the broker's revocation state, the controller PAT. root is the one component
// the agent cannot replace (it is the mount point), so all work happens through
// an os.Root confined to it, which refuses to traverse out by symlink or by
// `..`. A symlink at the staging dir or at bootstrap.json is refused outright,
// even one that stays inside the tree, because staging one agent's ticket into
// another agent's directory is its own defect.
//
// The explicit Chmod after WriteFile is load-bearing on a re-stage: WriteFile
// applies its perm only when it CREATES the file, so overwriting an existing
// bootstrap.json would otherwise keep whatever mode it already had.
func Stage(root, rel string, b Bootstrap) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("wire: stage bootstrap: open tree %q: %w", root, err)
	}
	defer r.Close()

	// Lstat BEFORE MkdirAll: an existing in-tree symlink would otherwise be
	// followed by the create.
	if err := refuseSymlink(r, rel); err != nil {
		return err
	}
	if err := r.MkdirAll(rel, 0o700); err != nil {
		return fmt.Errorf("wire: stage bootstrap: mkdir: %w", err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("wire: stage bootstrap: marshal: %w", err)
	}
	p := filepath.Join(rel, "bootstrap.json")
	if err := refuseSymlink(r, p); err != nil {
		return err
	}
	if err := r.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("wire: stage bootstrap: write: %w", err)
	}
	if err := r.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("wire: stage bootstrap: chmod: %w", err)
	}
	return nil
}

// refuseSymlink fails when name exists inside r and is a symbolic link. An
// absent name is fine (Stage creates it); any other stat error is left to the
// operation that follows, which reports it with better context.
func refuseSymlink(r *os.Root, name string) error {
	fi, err := r.Lstat(name)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wire: stage bootstrap: %q in %s is a symbolic link; refusing to write enrolment material through it "+
			"(an agent can write here, so following the link would let it choose the host's target)", name, r.Name())
	}
	return nil
}
