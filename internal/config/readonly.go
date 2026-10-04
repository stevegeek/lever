package config

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// TreeMount is one bind mount of a tree directory over itself in the
// manager container, as manager.read_only asks for it. Rel is relative to
// the tree with forward slashes; the caller joins it onto its own root (the
// in-jail tree for the mount source, the container workspace for the
// target). ReadOnly marks a read_only entry; a false one is a PIN — an
// ancestor of an entry mounted read-write over itself so the agent cannot
// rename or remove it (a mount point answers EBUSY) and so cannot move the
// protected directory away from its host path.
type TreeMount struct {
	Rel      string
	ReadOnly bool
}

// ManagerTreeMounts returns the manager's read_only plan: every entry
// read-only, every strict ancestor of an entry (below the tree root) as a
// read-write pin, each directory once. The order is deterministic — by
// depth, then by path — so a parent is listed before its children. podman
// sorts mounts by destination depth itself; the order here is for a stable
// inline config and stable tests. Validate has already rejected nested
// entries, so no directory is both an entry and a pin. Empty when nothing
// is configured.
func (a *App) ManagerTreeMounts() []TreeMount {
	if len(a.Manager.ReadOnly) == 0 {
		return nil
	}
	ro := make(map[string]bool)
	for _, e := range a.Manager.ReadOnly {
		rel := filepath.ToSlash(e)
		ro[rel] = true
		for dir := parentRel(rel); dir != ""; dir = parentRel(dir) {
			if _, seen := ro[dir]; !seen {
				ro[dir] = false
			}
		}
	}
	out := make([]TreeMount, 0, len(ro))
	for rel, readOnly := range ro {
		out = append(out, TreeMount{Rel: rel, ReadOnly: readOnly})
	}
	slices.SortFunc(out, func(x, y TreeMount) int {
		return cmp.Or(cmp.Compare(strings.Count(x.Rel, "/"), strings.Count(y.Rel, "/")), cmp.Compare(x.Rel, y.Rel))
	})
	return out
}

// parentRel is the parent of a forward-slash relative path, or "" for a
// top-level one (whose parent is the tree root, which is the workspace
// mount itself and needs no pin).
func parentRel(rel string) string {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return ""
	}
	return rel[:i]
}

// validateManagerReadOnly checks manager.read_only's shape (no host reads;
// CheckManagerReadOnlyHost does those). Each entry must be a clean relative
// path strictly inside the tree, spelt the one way a mount target will be
// (no "./", no trailing slash, no ".." anywhere), so the doctor row and the
// worker overlap test compare like with like. Entries must be disjoint: a
// nested pair would make one directory both a read-only entry and the other
// entry's read-write pin. And no worker dir may overlap an entry — a worker
// mounts its own subtree read-write at /workspace and never sees the
// manager's volumes, so a worker over (or under) a read_only path writes it
// freely, and the manager can ask a worker to.
func (a *App) validateManagerReadOnly() error {
	for i, e := range a.Manager.ReadOnly {
		if e == "" {
			return fmt.Errorf("config: manager.read_only entry %d is empty", i)
		}
		if filepath.IsAbs(e) || filepath.Clean(e) != e || e == "." || slices.Contains(strings.Split(filepath.ToSlash(e), "/"), "..") {
			return fmt.Errorf("config: manager.read_only entry %q must be a clean relative path inside the tree (no leading \"/\", \"./\" or trailing slash, no \"..\", not \".\")", e)
		}
		for _, prev := range a.Manager.ReadOnly[:i] {
			if prev == e {
				return fmt.Errorf("config: manager.read_only lists %q twice", e)
			}
			if pathOverlaps(prev, e) {
				return fmt.Errorf("config: manager.read_only entries %q and %q are nested — one contains the other, which would make a read-only directory the other's read-write pin; list only the outer one", prev, e)
			}
		}
		for _, g := range a.Workers {
			if pathOverlaps(g.Dir, e) {
				return fmt.Errorf("config: worker %q (dir %q) overlaps manager.read_only entry %q — the worker mounts its dir read-write and never sees the manager's read-only mounts, so it (or the manager, by dispatching it) could write the protected path; move the worker or the read_only entry", g.Name, g.Dir, e)
			}
		}
	}
	return nil
}

// CheckManagerReadOnlyHost verifies on the host that every manager.read_only
// entry is a real directory under the tree, reached through no symlink: each
// component from the tree root down is Lstat'ed. The guest resolves a mount
// source through symlinks, and the tree is agent-writable, so a link planted
// at an entry or at any of its ancestors would make the guest mount whatever
// the link names (and pin the wrong directory) while the config still reads
// as protected. A missing entry is an error too: the mount would fail, or
// worse be created empty by the runtime. Not part of CheckHost on purpose:
// that runs at every config load (doctor, the broker), and only the manager
// CREATE needs it — apply calls it before it starts the manager. The check
// and the create are not atomic; the window is the few seconds of one apply,
// against an agent that can only act in it if it already holds a writable
// path (a manager from before read_only was set).
func (a *App) CheckManagerReadOnlyHost() error {
	for _, e := range a.Manager.ReadOnly {
		p := a.Tree
		for _, c := range strings.Split(filepath.ToSlash(e), "/") {
			p = filepath.Join(p, c)
			fi, err := os.Lstat(p)
			if err != nil {
				return fmt.Errorf("config: manager.read_only entry %q: %w", e, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("config: manager.read_only entry %q goes through a symbolic link at %s — the tree is agent-writable, and a link would make the manager mount something other than the directory the config names; replace it with a real directory", e, p)
			}
			if !fi.IsDir() {
				return fmt.Errorf("config: manager.read_only entry %q: %s is not a directory", e, p)
			}
		}
	}
	return nil
}
