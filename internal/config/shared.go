package config

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/state"
)

// SharedFolder is one shared_folders entry: a tree directory that the
// agents it names mount at SharedMountRoot/<name>, read-write for each
// writer and read-only for each reader. Off unless listed.
//
// The manager mounts the whole tree, so it always sees the folder: at its
// tree path read-only (a manager.read_only entry in all but name, with the
// same pins), unless it is a writer. A worker sees nothing of it unless it
// is listed. Mounts are create-time material (scion keeps a record's
// volumes for life): a change reaches an agent created after it, and the
// broker refuses to resume a worker whose record holds a shared mount the
// config no longer grants (broker.refuseStaleShares).
type SharedFolder struct {
	// Path is the folder, relative to the tree, spelt like a
	// manager.read_only entry.
	Path string `yaml:"path"`
	// Name is the mount name under SharedMountRoot. Empty = the last
	// component of Path.
	Name string `yaml:"name"`
	// Writers mount the folder read-write: declared workers, or the app
	// name for the manager. May be empty (only the host writes it).
	Writers []string `yaml:"writers"`
	// Readers mount it read-only: declared workers, or SharedAllWorkers
	// for every worker that is not a writer. The manager is always a
	// reader at least, and may be named here too.
	Readers []string `yaml:"readers"`
}

const (
	// SharedMountRoot is where every agent with access mounts a shared
	// folder, so the path is the same in each of them.
	SharedMountRoot = "/shared"
	// SharedAllWorkers in readers names every declared worker.
	SharedAllWorkers = "*"
)

// sharedNameRE is a mount name: one path component, lowercase.
var sharedNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// EffectiveName is the folder's mount name.
func (s SharedFolder) EffectiveName() string {
	if s.Name != "" {
		return s.Name
	}
	return path.Base(cleanRel(s.Path))
}

// MountTarget is where an agent with access mounts the folder.
func (s SharedFolder) MountTarget() string { return path.Join(SharedMountRoot, s.EffectiveName()) }

// writes reports whether agent is a writer of s.
func (s SharedFolder) writes(agent string) bool { return slices.Contains(s.Writers, agent) }

// reads reports whether worker is a reader of s (not counting writers).
func (s SharedFolder) reads(worker string) bool {
	return slices.Contains(s.Readers, SharedAllWorkers) || slices.Contains(s.Readers, worker)
}

// SharedMount is one shared folder as one agent mounts it.
type SharedMount struct {
	Name     string // mount name (SharedFolder.EffectiveName)
	Rel      string // tree-relative source, forward slashes
	Target   string // in-container mount point (SharedFolder.MountTarget)
	ReadOnly bool
}

// SharedMountsFor is agent's shared_folders plan, in config order: agent is
// a declared worker's name or the app name (the manager). A writer mounts
// read-write; the manager and every listed reader read-only; any other
// worker mounts nothing.
func (a *App) SharedMountsFor(agent string) []SharedMount {
	var out []SharedMount
	for _, s := range a.SharedFolders {
		m := SharedMount{Name: s.EffectiveName(), Rel: cleanRel(s.Path), Target: s.MountTarget()}
		switch {
		case s.writes(agent):
		case agent == a.Name || (a.knownAgent(agent) && s.reads(agent)):
			m.ReadOnly = true
		default:
			continue
		}
		out = append(out, m)
	}
	return out
}

// managerReadOnlyDirs is every tree directory the manager mounts read-only
// over itself: manager.read_only, then each shared folder the manager does
// not write. ManagerTreeMounts plans the mounts and pins for all of them.
func (a *App) managerReadOnlyDirs() []string {
	out := slices.Clone(a.Manager.ReadOnly)
	for _, s := range a.SharedFolders {
		if !s.writes(a.Name) {
			out = append(out, cleanRel(s.Path))
		}
	}
	return out
}

// managerWrittenShared is every shared folder the manager writes: pinned
// in the manager's tree plan (ManagerTreeMounts).
func (a *App) managerWrittenShared() []string {
	var out []string
	for _, s := range a.SharedFolders {
		if s.writes(a.Name) {
			out = append(out, cleanRel(s.Path))
		}
	}
	return out
}

// ProtectedDirs is every tree directory a worker's workspace must stay
// clear of, as the broker checks it on each start and resume:
// manager.read_only and every shared folder. A worker whose workspace
// reached either would write it through its own read-write mount.
func (a *App) ProtectedDirs() []string {
	out := slices.Clone(a.Manager.ReadOnly)
	for _, s := range a.SharedFolders {
		out = append(out, cleanRel(s.Path))
	}
	return out
}

// SharedFolderPaths is every shared folder's tree-relative path.
func (a *App) SharedFolderPaths() []string {
	out := make([]string, 0, len(a.SharedFolders))
	for _, s := range a.SharedFolders {
		out = append(out, cleanRel(s.Path))
	}
	return out
}

// validateSharedFolders checks shared_folders' shape (no host reads;
// PrepareSharedFoldersHost does those). A path is spelt like a
// manager.read_only entry and is subject to the same character rules. The
// folders must be disjoint from each other, from every manager.read_only
// entry and from every worker dir: a worker dir over or under a folder
// mounts it read-write at /workspace, whatever the lists say. Names are
// unique, and every listed agent is declared; nobody is both writer and
// reader.
func (a *App) validateSharedFolders() error {
	names := map[string]string{}
	for i, s := range a.SharedFolders {
		p := s.Path
		if p == "" {
			return fmt.Errorf("config: shared_folders entry %d has no path", i)
		}
		if filepath.IsAbs(p) || filepath.Clean(p) != p || p == "." || slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..") {
			return fmt.Errorf("config: shared_folders path %q must be a clean relative path inside the tree (no leading \"/\", \"./\" or trailing slash, no \"..\", not \".\")", p)
		}
		if strings.ContainsAny(p, readOnlyForbiddenChars) {
			return fmt.Errorf("config: shared_folders path %q must not contain any of %q (scion expands $VAR and ~ in a mount path, and the runtime splits mount options on : and ,)", p, readOnlyForbiddenChars)
		}
		if !isASCII(p) {
			return fmt.Errorf("config: shared_folders path %q must be ASCII — APFS treats the composed and decomposed spellings of an accented name as one directory, so a non-ASCII path can have a second spelling that every comparison here misses", p)
		}
		name := s.EffectiveName()
		if !sharedNameRE.MatchString(name) {
			return fmt.Errorf("config: shared_folders %q: mount name %q must match %s (set name: to choose one)", p, name, sharedNameRE)
		}
		if prev, ok := names[name]; ok {
			return fmt.Errorf("config: shared_folders %q and %q both mount as %s; set name: on one of them", prev, p, path.Join(SharedMountRoot, name))
		}
		names[name] = p
		for _, prev := range a.SharedFolders[:i] {
			if fsutil.RelOverlapFold(prev.Path, p) {
				return fmt.Errorf("config: shared_folders %q and %q overlap — one is the other or contains it; a directory can have one set of writers only", prev.Path, p)
			}
		}
		for _, e := range a.Manager.ReadOnly {
			if fsutil.RelOverlapFold(e, p) {
				return fmt.Errorf("config: shared_folders %q overlaps manager.read_only %q; list the directory in one of them only", p, e)
			}
		}
		for _, g := range a.Workers {
			if fsutil.RelOverlapFold(g.Dir, p) {
				return fmt.Errorf("config: shared folder %q overlaps worker %q (dir %q) — the worker mounts its dir read-write, so it would write the folder whatever writers says; name it in writers instead and move the folder out of its dir", p, g.Name, g.Dir)
			}
		}
		for _, part := range strings.Split(filepath.ToSlash(p), "/") {
			// Host-written places, wherever they sit: .lever holds the
			// manager's enrolment ticket, the state directory the broker's
			// secrets, .lever-files the chat file exchange.
			for _, host := range []string{".lever", state.DirName, chatfiles.Dir} {
				if strings.EqualFold(part, host) {
					return fmt.Errorf("config: shared_folders %q is or lies inside %s, a directory lever writes for the host; keep shared folders out of it", p, part)
				}
			}
		}
		if lf := a.Remote.LabelsFile; lf != "" && len(s.Writers) > 0 && !(len(s.Writers) == 1 && s.Writers[0] == a.Name) && fsutil.RelOverlapFold(filepath.ToSlash(filepath.Clean(lf)), p) {
			return fmt.Errorf("config: remote: labels_file %q is inside shared folder %q, which a worker writes; that worker would write the labels contacts see", lf, p)
		}
		if len(s.Writers) == 0 && len(s.Readers) == 0 {
			return fmt.Errorf("config: shared_folders %q names no writers and no readers; list at least one agent", p)
		}
		for _, w := range s.Writers {
			if w == SharedAllWorkers {
				return fmt.Errorf("config: shared_folders %q: writers must name each agent; %q is for readers only", p, SharedAllWorkers)
			}
			if !a.knownAgent(w) {
				return fmt.Errorf("config: shared_folders %q lists writer %q, which is not a declared worker or the manager (%s)", p, w, a.Name)
			}
		}
		if slices.Contains(s.Readers, SharedAllWorkers) && len(s.Readers) > 1 {
			return fmt.Errorf("config: shared_folders %q: readers %q already means every worker; list it alone", p, SharedAllWorkers)
		}
		for _, r := range s.Readers {
			if r == SharedAllWorkers {
				continue
			}
			if !a.knownAgent(r) {
				return fmt.Errorf("config: shared_folders %q lists reader %q, which is not a declared worker or the manager (%s)", p, r, a.Name)
			}
			if s.writes(r) {
				return fmt.Errorf("config: shared_folders %q lists %q as both writer and reader", p, r)
			}
		}
		if dup := firstDuplicate(s.Writers); dup != "" {
			return fmt.Errorf("config: shared_folders %q lists writer %q twice", p, dup)
		}
		if dup := firstDuplicate(s.Readers); dup != "" {
			return fmt.Errorf("config: shared_folders %q lists reader %q twice", p, dup)
		}
	}
	return nil
}

// firstDuplicate is the first value listed twice in xs, or "".
func firstDuplicate(xs []string) string {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}

// PrepareSharedFoldersHost checks the host tree before an agent that
// mounts a shared folder is created: each folder must exist as a real
// directory reached through no symbolic link (the guest resolves a mount
// source through links, and the tree is agent-writable). It does not walk
// the folder's contents: a writer may change them at any time, and a link
// inside resolves in the reading container, where it reaches only what
// that container already sees. Host programs inside a folder are refused
// at config load (checkHostPathsOutsideTree): no shared folder excuses one.
//
// It does refuse a file with a name OUTSIDE the folder (a hard link whose
// other name lies elsewhere in the tree): an agent that made such a name
// before the folder was shared could edit the file through it, whatever
// the folder's writers list says. New names across the boundary cannot be
// made from a container (a hard link across two mounts fails with EXDEV),
// so the check at create closes the gap. Hard links with every name inside
// the folder are the writers' business, like symbolic links.
func (a *App) PrepareSharedFoldersHost() error {
	for _, s := range a.SharedFolders {
		if err := walkNoSymlink(a.Tree, cleanRel(s.Path), false); err != nil {
			return fmt.Errorf("config: shared_folders %q: %w (create it as a real directory before starting the agents that mount it)", s.Path, err)
		}
		if err := refuseOutsideHardLinks(filepath.Join(a.Tree, filepath.FromSlash(cleanRel(s.Path)))); err != nil {
			return fmt.Errorf("config: shared_folders %q: %w", s.Path, err)
		}
	}
	return nil
}

// refuseOutsideHardLinks walks dir (never following a link) and fails on a
// regular file whose link count exceeds the names it has inside dir.
func refuseOutsideHardLinks(dir string) error {
	type inode struct{ dev, ino uint64 }
	type seen struct {
		path  string
		names uint64
		nlink uint64
	}
	files := map[inode]*seen{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink <= 1 {
			return nil
		}
		k := inode{uint64(st.Dev), uint64(st.Ino)}
		if f := files[k]; f != nil {
			f.names++
		} else {
			files[k] = &seen{p, 1, uint64(st.Nlink)}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.names < f.nlink {
			return fmt.Errorf("%s has %d hard links but only %d inside the folder: another name for the same file lies outside it, where an agent that is not a writer may edit it; replace it with a copy (cp, then mv over it)", f.path, f.nlink, f.names)
		}
	}
	return nil
}
