package voice

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveExisting resolves symlinks of the nearest existing ancestor of p and
// re-appends the part that does not exist yet.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// RefuseInTree refuses p, the value of a tool's flag, when it lies in the
// tree the agents can read and write, or is the tree itself (links
// followed, for the part of p that exists): the voice tools' models, program
// and socket must not. why says what an agent could do there.
func RefuseInTree(tree, flag, p, why string) error {
	t, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return fmt.Errorf("-tree: %w", err)
	}
	s, err := resolveExisting(p)
	if err != nil {
		return fmt.Errorf("%s: %w", flag, err)
	}
	rel, err := filepath.Rel(t, s)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s %s is inside -tree %s: %s", flag, p, tree, why)
	}
	return nil
}
