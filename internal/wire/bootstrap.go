// Package wire holds the on-the-wire and on-disk envelope types shared by the
// host, the broker and the in-container agent, so a JSON tag cannot drift
// between producer and consumer. It is a leaf: it must import nothing
// internal, so any package can depend on it without an import cycle.
package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Bootstrap is the enrolment envelope the host (for the manager) or the broker
// (for a worker, and on auto-re-enrol) deposits as bootstrap.json for
// lever-agent boot to consume.
type Bootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
	// Claude is the agent's Claude Code configuration from the host config,
	// applied by lever-agent boot at every start. nil when none is
	// configured.
	Claude *Claude `json:"claude,omitempty"`
}

// Stage writes b as <root>/<rel>/bootstrap.json (directory 0700, file 0600).
// It may overwrite an existing file on re-stage.
//
// Everything under root is agent-writable, and the caller is the host running
// as the operator, so rel is adversary-controlled: a symlinked .lever would
// make the host write and chmod wherever the link points. root is the mount
// point the agent cannot replace, so all work goes through an os.Root confined
// to it. A symlink at the staging dir or at bootstrap.json is refused even when
// it stays inside the tree, since it could stage one agent's ticket into
// another agent's directory.
//
// The Chmod after WriteFile is needed on re-stage: WriteFile applies perm only
// when it creates the file.
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

// refuseSymlink fails when name is a symbolic link. Stat errors are ignored:
// the operation that follows reports them with better context.
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
