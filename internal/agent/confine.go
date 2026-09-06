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
// `lever-agent boot` runs as ROOT in scion's pre-start hook, and every file it
// touches (~/.lever-id/*, ~/.claude/settings.json, ~/.scion/scion-services.yaml,
// /workspace/.lever/bootstrap.json) sits in a tree the agent (uid 1000) owns.
// Following a link there would hand the agent a root write (chmod 0700 the
// target, write attacker-chosen bytes through it) or a root read (a FIFO also
// hangs the read). So, mirroring wire.Stage on the host side, all work goes
// through an os.Root anchored at the one component the agent cannot replace
// (the mount point: $HOME or /workspace), and every agent-controlled component
// is Lstat-checked and refused if it is a link — even one that stays inside
// the root, because a link there is never legitimate.
var errRefusedPath = errors.New("agent: refusing to follow a symbolic link under the agent-writable tree")

// splitAbove returns the directory depth levels above path as an os.Root
// anchor, with path made relative to it. depth counts the agent-controlled
// components in the production layout: 1 for ~/.lever-id (root = $HOME),
// 2 for ~/.claude/settings.json and <ws>/.lever/bootstrap.json. The anchor
// must already exist; a relative path anchors at ".".
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
// An absent name is fine (the caller creates it). The check is by Lstat, so it
// never follows the link it is checking; r's own confinement then bounds any
// TOCTOU swap to the root, where the agent could already write anyway.
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

// checkedFile refuses a link at rel's directory (when it is not the root
// itself) and at rel, in that order, so a linked directory is reported before
// anything is created under it.
func checkedFile(r *os.Root, rel string) error {
	if d := filepath.Dir(rel); d != "." {
		if err := refuseLink(r, d, true); err != nil {
			return err
		}
	}
	return refuseLink(r, rel, false)
}
