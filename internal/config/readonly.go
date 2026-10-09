package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/stevegeek/lever/internal/fsutil"
)

// TreeMount is one bind mount of a tree directory over itself in the
// manager container, as manager.read_only asks for it. Rel is relative to
// the tree with forward slashes; the caller joins it onto its own root (the
// in-jail tree for the mount source, the container workspace for the
// target). ReadOnly marks a read_only entry; a false one is a PIN — a
// directory mounted read-write over itself so the agent cannot rename or
// remove it (a mount point answers EBUSY). Pins are every strict ancestor of
// an entry (so the protected directory cannot be moved away from its host
// path) and, while read_only or shared_folders is set, every worker dir and its ancestors (so
// the manager cannot swap a worker dir for a link to a protected one before
// dispatching that worker, which mounts its dir read-write).
type TreeMount struct {
	Rel      string
	ReadOnly bool
	// WorkerPin marks a pin that exists only for a worker dir (the dir or
	// one of its ancestors), not as an entry's ancestor: its absence lets
	// the manager redirect a worker, not move an entry. For wording.
	WorkerPin bool
}

// readOnlyForbiddenChars are characters a read_only entry (and, while
// read_only is set, a worker dir, which becomes a pin) must not contain.
// scion expands `$VAR` and `~` in a volume's source and target, so either
// would mount a path other than the one the config names; `:` and `,`
// separate fields in the runtime's own mount syntax.
const readOnlyForbiddenChars = "$~:,"

// ManagerTreeMounts returns the manager's read_only plan: every entry
// read-only; every strict ancestor of an entry, every worker dir and every
// strict ancestor of a worker dir (all below the tree root) as a read-write
// pin; each spelling once (entries first, so an entry is never demoted to
// a pin). The order is deterministic — by depth, then by path — so a parent
// is listed before its children. podman sorts mounts by destination depth
// itself; the order here is for a stable inline config and stable tests.
// Validate has already rejected nested entries and worker dirs that overlap
// an entry, so no directory is both an entry and a pin. Empty when nothing
// is configured: worker dirs are pinned only to protect read_only entries.
func (a *App) ManagerTreeMounts() []TreeMount {
	dirs := a.managerReadOnlyDirs()
	if len(dirs) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []TreeMount
	add := func(rel string, readOnly, workerPin bool) {
		// Keyed on the exact spelling, NOT case-folded: on a case-sensitive
		// guest filesystem (Lima on a Linux host) `KB` and `kb` are two
		// directories and each needs its own pin. On a case-insensitive host
		// the second spelling is a harmless extra mount of the same one.
		if seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, TreeMount{Rel: rel, ReadOnly: readOnly, WorkerPin: workerPin})
	}
	for _, e := range dirs {
		add(cleanRel(e), true, false)
	}
	pinWithAncestors := func(rel string, self, workerPin bool) {
		if self {
			add(rel, false, workerPin)
		}
		for dir := parentRel(rel); dir != ""; dir = parentRel(dir) {
			add(dir, false, workerPin)
		}
	}
	for _, e := range dirs {
		pinWithAncestors(cleanRel(e), false, false)
	}
	for _, g := range a.Workers {
		pinWithAncestors(cleanRel(g.Dir), true, true)
	}
	slices.SortFunc(out, func(x, y TreeMount) int {
		return cmp.Or(cmp.Compare(strings.Count(x.Rel, "/"), strings.Count(y.Rel, "/")), cmp.Compare(x.Rel, y.Rel))
	})
	return out
}

// isASCII reports whether s is plain ASCII (no Unicode normalisation
// aliases are possible).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// cleanRel is a tree-relative path in the one spelling a mount uses:
// cleaned, forward slashes.
func cleanRel(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

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
// PrepareManagerReadOnlyHost does those). Each entry must be a clean
// relative path strictly inside the tree, spelt the one way a mount target
// will be (no "./", no trailing slash, no ".." anywhere), and free of the
// characters scion or the runtime would interpret (readOnlyForbiddenChars).
// Entries must be disjoint: a nested pair would make one directory both a
// read-only entry and the other entry's read-write pin. And no worker dir
// may overlap an entry — a worker mounts its own subtree read-write at
// /workspace and never sees the manager's volumes, so a worker over (or
// under) a read_only path writes it freely, and the manager can ask a
// worker to. Every comparison folds case: on a case-insensitive host
// `Assistant/tools` and `assistant/tools` are one directory.
func (a *App) validateManagerReadOnly() error {
	if len(a.managerReadOnlyDirs()) == 0 {
		return nil
	}
	for i, e := range a.Manager.ReadOnly {
		if e == "" {
			return fmt.Errorf("config: manager.read_only entry %d is empty", i)
		}
		if filepath.IsAbs(e) || filepath.Clean(e) != e || e == "." || slices.Contains(strings.Split(filepath.ToSlash(e), "/"), "..") {
			return fmt.Errorf("config: manager.read_only entry %q must be a clean relative path inside the tree (no leading \"/\", \"./\" or trailing slash, no \"..\", not \".\")", e)
		}
		if strings.ContainsAny(e, readOnlyForbiddenChars) {
			return fmt.Errorf("config: manager.read_only entry %q must not contain any of %q (scion expands $VAR and ~ in a mount path, and the runtime splits mount options on : and ,)", e, readOnlyForbiddenChars)
		}
		if !isASCII(e) {
			return fmt.Errorf("config: manager.read_only entry %q must be ASCII — APFS treats the composed and decomposed spellings of an accented name as one directory, so a non-ASCII entry can have a second spelling that every comparison here misses", e)
		}
		for _, prev := range a.Manager.ReadOnly[:i] {
			if strings.EqualFold(prev, e) {
				return fmt.Errorf("config: manager.read_only lists %q twice (as %q and %q; the host filesystem may not tell case apart)", e, prev, e)
			}
			if fsutil.RelOverlapFold(prev, e) {
				return fmt.Errorf("config: manager.read_only entries %q and %q are nested — one contains the other, which would make a read-only directory the other's read-write pin; list only the outer one", prev, e)
			}
		}
		for _, g := range a.Workers {
			if fsutil.RelOverlapFold(g.Dir, e) {
				return fmt.Errorf("config: worker %q (dir %q) overlaps manager.read_only entry %q — the worker mounts its dir read-write and never sees the manager's read-only mounts, so it (or the manager, by dispatching it) could write the protected path; move the worker or the read_only entry", g.Name, g.Dir, e)
			}
		}
	}
	// With read_only set, every worker dir becomes a pin in the manager, so
	// it is subject to the same character rule as an entry.
	for _, g := range a.Workers {
		if strings.ContainsAny(g.Dir, readOnlyForbiddenChars) {
			return fmt.Errorf("config: worker %q dir %q must not contain any of %q while manager.read_only or shared_folders is set (the manager pins each worker dir with a mount, and scion expands $VAR and ~ in a mount path)", g.Name, g.Dir, readOnlyForbiddenChars)
		}
		if !isASCII(g.Dir) {
			return fmt.Errorf("config: worker %q dir %q must be ASCII while manager.read_only or shared_folders is set — APFS treats the composed and decomposed spellings of an accented name as one directory, so the overlap check with the read_only entries could miss an alias", g.Name, g.Dir)
		}
	}
	return nil
}

// PrepareManagerReadOnlyHost readies the host tree for the manager's
// read_only mounts and refuses a tree that would defeat them. Nothing
// happens when read_only is unset. Otherwise, as the operator:
//
//   - every entry must be a real directory reached through no symlink: each
//     component from the tree root down is Lstat'ed. The guest resolves a
//     mount source through symlinks and the tree is agent-writable, so a
//     link planted at an entry or an ancestor would mount something else
//     while the config still reads as protected;
//   - no symlink INSIDE an entry may resolve outside it: host code loading
//     through such a link would read content the agent can write;
//   - every worker dir must be reached through no symlink (a worker dir
//     swapped for a link to an entry would mount the entry read-write in
//     that worker), and a missing one is created — through an os.Root at
//     the tree, which refuses to follow a link out — so the manager's pin
//     for it has a target. The walk runs again after the mkdir, so a swap
//     in between is caught.
//
// Not part of CheckHost on purpose: that runs at every config load (doctor,
// the broker), and only a manager CREATE needs it — apply calls it before
// it acts on the manager record and again right before the create. The
// check and the create are not atomic; the window is the moments before one
// create, against an agent that can only act in it if it already holds the
// paths writable (a manager from before read_only was set — and the second
// call runs after a --fresh delete has removed that one).
func (a *App) PrepareManagerReadOnlyHost() error {
	// Shared folders the manager mounts: no link on the path, but their
	// contents are the writers' (PrepareSharedFoldersHost).
	if err := a.PrepareSharedFoldersHost(); err != nil {
		return err
	}
	if len(a.managerReadOnlyDirs()) == 0 {
		return nil
	}
	for _, e := range a.Manager.ReadOnly {
		if err := walkNoSymlink(a.Tree, e, false); err != nil {
			return fmt.Errorf("config: manager.read_only entry %q: %w", e, err)
		}
		if err := refuseEscapingLinks(filepath.Join(a.Tree, e)); err != nil {
			return fmt.Errorf("config: manager.read_only entry %q: %w", e, err)
		}
	}
	if len(a.Workers) == 0 {
		return nil
	}
	root, err := os.OpenRoot(a.Tree)
	if err != nil {
		return fmt.Errorf("config: manager.read_only: opening the tree: %w", err)
	}
	defer root.Close()
	for _, g := range a.Workers {
		rel := filepath.Clean(g.Dir)
		if err := walkNoSymlink(a.Tree, rel, true); err != nil {
			return fmt.Errorf("config: worker %q dir %q (pinned while manager.read_only or shared_folders is set): %w", g.Name, g.Dir, err)
		}
		if err := root.MkdirAll(rel, 0o755); err != nil {
			return fmt.Errorf("config: worker %q dir %q: creating it: %w", g.Name, g.Dir, err)
		}
		if afterWorkerDirMkdir != nil {
			afterWorkerDirMkdir(rel)
		}
		if err := walkNoSymlink(a.Tree, rel, false); err != nil {
			return fmt.Errorf("config: worker %q dir %q (pinned while manager.read_only or shared_folders is set): %w", g.Name, g.Dir, err)
		}
	}
	return nil
}

// afterWorkerDirMkdir is a test seam: called between a worker dir's mkdir
// and the walk that follows it, so a test can make the swap that walk
// exists to catch. nil outside tests.
var afterWorkerDirMkdir func(rel string)

// walkNoSymlink Lstats each component of rel below tree and fails on a
// symbolic link or a non-directory. A missing component is an error unless
// allowMissing, which ends the walk there (the caller creates the rest).
func walkNoSymlink(tree, rel string, allowMissing bool) error {
	p := tree
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if allowMissing && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("goes through a symbolic link at %s — the tree is agent-writable, and a link would make the jail mount something other than the directory the config names; replace it with a real directory", p)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", p)
		}
	}
	return nil
}

// refuseEscapingLinks walks dir (never following a link) and fails closed
// on what would let code loaded from dir read agent-writable content:
//
//   - a symbolic link whose target lies outside dir, lexically (the target
//     read relative to the link's own directory) or by its real path; a
//     link whose real path cannot be resolved (dangling, a loop) is refused
//     too — the lexical reading of `s/../x` is not the kernel's when `s` is
//     itself a link, and a dangling target is one the agent may create
//     later outside the entry;
//   - a file with more than one hard link: a name for it made before the
//     protection (anywhere in the tree) lets the manager edit the
//     protected file through a writable name.
//
// Read-only mounting protects the names under dir, not what a link there
// points at nor another name of the same inode.
func refuseEscapingLinks(dir string) error {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	inside := func(base, p string) bool {
		rel, err := filepath.Rel(base, p)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return fmt.Errorf("%s has %d hard links: another name for the same file may sit where the agent can write, and editing it there edits this one; replace it with a copy (cp, then mv over it)", p, st.Nlink)
			}
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		target = filepath.Clean(target)
		if !inside(dir, target) {
			return fmt.Errorf("symbolic link %s points outside the entry (to %s): code loaded through it would read agent-writable content; make it a real file or point it inside the entry", p, target)
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("symbolic link %s cannot be resolved (%v): a dangling or looping link may resolve outside the entry once the agent creates its target; remove it or make it resolve inside the entry", p, err)
		}
		if !inside(realDir, real) {
			return fmt.Errorf("symbolic link %s resolves outside the entry (to %s): code loaded through it would read agent-writable content; make it a real file or point it inside the entry", p, real)
		}
		return nil
	})
}
