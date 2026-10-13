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

// TreeMount is one bind mount of a tree directory over itself in the manager
// container. Rel is tree-relative with forward slashes. A mount that is not
// ReadOnly is a pin: mounted read-write over itself so the agent cannot
// rename or remove it (a mount point answers EBUSY). Every strict ancestor of
// an entry is pinned, so the protected directory cannot be moved away, and
// so is every worker dir with its ancestors, so the manager cannot swap a
// worker dir for a link to a protected one before dispatching that worker.
type TreeMount struct {
	Rel      string
	ReadOnly bool
	// WorkerPin marks a pin that exists only for a worker dir, for wording.
	WorkerPin bool
}

// readOnlyForbiddenChars may not appear in a mounted path: scion expands
// `$VAR` and `~` in a volume's source and target, and `:` and `,` separate
// fields in the runtime's mount syntax.
const readOnlyForbiddenChars = "$~:,"

// ManagerTreeMounts returns the manager's mount plan (see TreeMount), parents
// first. Entries are added before pins so an entry is never demoted to a pin.
// Empty when nothing is configured: worker dirs are pinned only to protect
// something.
//
// A shared folder the manager does not write is an entry; one it writes is
// pinned, so the manager cannot swap it for a link that a worker would then
// mount in its place.
func (a *App) ManagerTreeMounts() []TreeMount {
	return a.treeMountPlan(a.managerReadOnlyDirs(), a.managerWrittenShared())
}

// ReadOnlyEntryMounts is the manager.read_only part of the plan alone, which
// the broker's tool guard requires before it starts a tool whose program
// lies under a read_only entry.
func (a *App) ReadOnlyEntryMounts() []TreeMount {
	return a.treeMountPlan(a.Manager.ReadOnly, nil)
}

func (a *App) treeMountPlan(dirs, pinned []string) []TreeMount {
	if len(dirs) == 0 && len(pinned) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []TreeMount
	add := func(rel string, readOnly, workerPin bool) {
		// Not case-folded: on a case-sensitive guest filesystem `KB` and `kb`
		// are two directories and each needs its own pin.
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
	for _, p := range pinned {
		pinWithAncestors(cleanRel(p), true, false)
	}
	for _, g := range a.Workers {
		pinWithAncestors(cleanRel(g.Dir), true, true)
	}
	slices.SortFunc(out, func(x, y TreeMount) int {
		return cmp.Or(cmp.Compare(strings.Count(x.Rel, "/"), strings.Count(y.Rel, "/")), cmp.Compare(x.Rel, y.Rel))
	})
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func cleanRel(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

// parentRel is "" for a top-level path: the tree root is the workspace mount
// itself and needs no pin.
func parentRel(rel string) string {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return ""
	}
	return rel[:i]
}

// validateManagerReadOnly checks manager.read_only's shape; host reads are
// PrepareManagerReadOnlyHost's. Entries must be disjoint, since a nested pair
// would make one directory the other's read-write pin. No worker dir may
// overlap an entry: a worker mounts its subtree read-write and never sees the
// manager's volumes, and the manager can ask a worker to write. Every
// comparison folds case for case-insensitive hosts.
func (a *App) validateManagerReadOnly() error {
	if len(a.ManagerTreeMounts()) == 0 {
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
	// Every worker dir becomes a pin, so the same character rules apply.
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
// read_only mounts and refuses a tree that would defeat them:
//
//   - every entry must be a real directory reached through no symlink: the
//     guest resolves a mount source through symlinks and the tree is
//     agent-writable, so a planted link would mount something else while
//     the config still reads as protected;
//   - no symlink inside an entry may resolve outside it: host code loading
//     through it would read content the agent can write;
//   - every worker dir must be reached through no symlink, or a worker dir
//     swapped for a link to an entry would mount the entry read-write in that
//     worker. A missing one is created through an os.Root, which refuses to
//     follow a link out, and walked again after the mkdir to catch a swap.
//
// Not part of CheckHost: only a manager create needs it, and apply calls it
// before acting on the manager record and again right before the create.
// The check and the create are not atomic; only an agent that already holds
// the paths writable could act in that window.
func (a *App) PrepareManagerReadOnlyHost() error {
	if err := a.PrepareSharedFoldersHost(); err != nil {
		return err
	}
	if len(a.ManagerTreeMounts()) == 0 {
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

// afterWorkerDirMkdir is a test seam between a worker dir's mkdir and the
// walk that follows it.
var afterWorkerDirMkdir func(rel string)

// walkNoSymlink fails on a symbolic link or a non-directory at any
// component of rel. allowMissing ends the walk at a missing component.
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

// refuseEscapingLinks fails closed on what would let code loaded from dir
// read agent-writable content: read-only mounting protects the names under
// dir, not what a link there points at nor another name of the same inode.
//
//   - a symbolic link whose target lies outside dir, lexically or by its
//     real path; an unresolvable link is refused too, since the lexical
//     reading of `s/../x` is not the kernel's when `s` is a link, and the
//     agent may create a dangling target later;
//   - a file with more than one hard link: another name for it in the tree
//     lets the manager edit the protected file.
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
