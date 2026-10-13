package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// errRefusedPath is returned when a path lever-agent is about to read or
// write under the agent-writable home is a symbolic link, or exists as
// something other than the kind of file expected there.
//
// lever-agent boot runs in scion's pre-start hook, and every file it touches
// sits in a tree the agent owns (a worker's read-only bootstrap mount takes
// the same path). Under rootless podman sciontool drops to the agent user
// before pre-start hooks, so a planted link gains nothing. On a rootful
// runtime, or when that drop fails (sciontool logs it and carries on as
// container root), boot runs as root, and following a link would give the
// agent a root write or read; a FIFO hangs the read at any uid. Nothing here
// may rely on boot being root or not. So all work goes through an os.Root
// anchored at the mount point the agent cannot replace, and every
// agent-controlled component is Lstat-checked and refused if it is a link,
// even one that stays inside the root.
var errRefusedPath = errors.New("agent: refusing to follow a symbolic link under the agent-writable tree")

// splitAbove returns the directory depth levels above path as an os.Root
// anchor, with path made relative to it. depth counts the agent-controlled
// components, e.g. 1 for ~/.lever-id, 2 for ~/.claude/settings.json. The
// anchor must already exist; a relative path anchors at ".".
func splitAbove(path string, depth int) (root, rel string) {
	path = filepath.Clean(path)
	root = path
	for range depth {
		root = filepath.Dir(root)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil { // cannot happen: root is a prefix of path by construction
		rel = filepath.Base(path)
	}
	return root, rel
}

// refuseLink fails with errRefusedPath when name exists inside r and is a
// symbolic link, or is not a directory (wantDir) / a regular file (!wantDir).
// An absent name is fine. r's confinement bounds any TOCTOU swap after the
// Lstat to the root, where the agent could already write anyway.
func refuseLink(r *os.Root, name string, wantDir bool) error {
	fi, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent: lstat %s in %s: %w", name, r.Name(), err)
	}
	switch mode := fi.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s in %s is a symbolic link", errRefusedPath, name, r.Name())
	case wantDir && !mode.IsDir():
		return fmt.Errorf("%w: %s in %s is not a directory", errRefusedPath, name, r.Name())
	case !wantDir && !mode.IsRegular():
		return fmt.Errorf("%w: %s in %s is not a regular file", errRefusedPath, name, r.Name())
	}
	return nil
}

// checkedFile checks rel's directory before rel, so a linked directory is
// reported before anything is created under it.
func checkedFile(r *os.Root, rel string) error {
	if d := filepath.Dir(rel); d != "." {
		if err := refuseLink(r, d, true); err != nil {
			return err
		}
	}
	return refuseLink(r, rel, false)
}
