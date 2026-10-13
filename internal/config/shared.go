package config

import (
	"errors"
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

// SharedFolder is a tree directory that the agents it names mount at
// SharedMountRoot/<name>, read-write for writers and read-only for readers.
//
// The manager mounts the whole tree, so it always sees the folder, read-only
// (like a manager.read_only entry) unless it is a writer. Mounts are
// create-time only, and the broker refuses to resume a worker whose record
// holds a shared mount the config does not grant (broker.refuseStaleShares).
type SharedFolder struct {
	// Path is spelt like a manager.read_only entry.
	Path string `yaml:"path"`
	// Name defaults to the last component of Path.
	Name string `yaml:"name"`
	// Writers are declared workers, or the app name for the manager.
	Writers []string `yaml:"writers"`
	// Readers are declared workers, or SharedAllWorkers.
	Readers []string `yaml:"readers"`
}

const (
	SharedMountRoot  = "/shared"
	SharedAllWorkers = "*" // in readers: every worker that is not a writer
)

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

func (s SharedFolder) writes(agent string) bool { return slices.Contains(s.Writers, agent) }

func (s SharedFolder) reads(worker string) bool {
	return slices.Contains(s.Readers, SharedAllWorkers) || slices.Contains(s.Readers, worker)
}

// SharedMount is one shared folder as one agent mounts it.
type SharedMount struct {
	Name     string
	Rel      string // tree-relative source, forward slashes
	Target   string
	ReadOnly bool
}

// SharedMountsFor is agent's shared_folders plan; agent is a worker name or
// the app name (the manager).
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

func (a *App) managerReadOnlyDirs() []string {
	out := slices.Clone(a.Manager.ReadOnly)
	for _, s := range a.SharedFolders {
		if !s.writes(a.Name) {
			out = append(out, cleanRel(s.Path))
		}
	}
	return out
}

func (a *App) managerWrittenShared() []string {
	var out []string
	for _, s := range a.SharedFolders {
		if s.writes(a.Name) {
			out = append(out, cleanRel(s.Path))
		}
	}
	return out
}

// ProtectedDirs is every tree directory a worker's workspace must stay clear
// of, or it would write it through its own read-write mount.
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

// validateSharedFolders: the folders must be disjoint from each other, from
// manager.read_only and from every worker dir, since a worker dir over or
// under a folder mounts it read-write whatever the lists say.
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
			// Host-written places: .lever holds the manager's enrolment
			// ticket, the state directory the broker's secrets.
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

// PrepareSharedFoldersHost requires each folder to be a real directory
// reached through no symbolic link: the guest resolves a mount source
// through links, and the tree is agent-writable. The contents are not
// walked: a link inside resolves in the reading container, where it reaches
// only what that container already sees.
func (a *App) PrepareSharedFoldersHost() error {
	for _, s := range a.SharedFolders {
		if err := walkNoSymlink(a.Tree, cleanRel(s.Path), false); err != nil {
			return fmt.Errorf("config: shared_folders %q: %w (create it as a real directory before starting the agents that mount it)", s.Path, err)
		}
	}
	return nil
}

// CheckSharedFolderLinks refuses, right before the manager is created, a
// file in a shared folder with a hard link outside the folder: an agent
// that made it earlier could edit the file through it. No new name across
// the boundary can be made from a container afterwards (EXDEV between
// mounts), so the check at create closes the gap.
func (a *App) CheckSharedFolderLinks() error {
	for _, s := range a.SharedFolders {
		if err := refuseOutsideHardLinks(filepath.Join(a.Tree, filepath.FromSlash(cleanRel(s.Path)))); err != nil {
			return fmt.Errorf("config: shared_folders %q: %w", s.Path, err)
		}
	}
	return nil
}

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
			// An unreadable directory is refused, not skipped: it may hold
			// an earlier name that a reader can still open (container root
			// reads past the mode).
			if p != dir && errors.Is(err, fs.ErrNotExist) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if errors.Is(err, fs.ErrPermission) {
				return fmt.Errorf("%s cannot be read, so lever cannot check it for hard links that leave the folder; make it readable (chmod -R u+rX on the folder) and try again", p)
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
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
			return fmt.Errorf("%s has %d hard links but only %d inside the folder: another name for the same file lies outside the folder (elsewhere in the tree, where an agent that is not a writer may edit it, or outside the tree), and lever cannot tell which; replace it with a copy (cp, then mv over it)", f.path, f.nlink, f.names)
		}
	}
	return nil
}
