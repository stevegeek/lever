// Package fsutil holds the file helpers the host packages share. It uses the
// standard library only, plus golang.org/x/sys/unix for InsideByIdentity,
// whose fstatat and openat the syscall package does not export on every
// platform.
package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// WriteFileAtomic writes data to a synced temp file in path's directory and
// renames it over path, so a crash or power loss leaves the old file or the
// new one, never a torn or empty one. The rename replaces whatever path was
// (a symlink included) with a regular file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir is best effort: some file systems refuse to sync a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Tree-confined file access.
//
// Everything under an instance tree is agent-writable, and the host process
// that scaffolds into it runs as the operator. A plain os.ReadFile or
// os.WriteFile there follows whatever symlink the agent planted, turning the
// operator's next `lever init` into a write to an arbitrary host file.
// ReadInTree and WriteInTree follow a symlink only while it resolves inside
// the tree (an operator's own `CLAUDE.md -> docs/CLAUDE.md` keeps working,
// written in place); a link that leaves the tree, dangles, or ends on
// anything but a regular file is refused. Reads are capped so a FIFO or
// device node cannot hang the host.
//
// The name walk gives the named errors; the I/O goes through an os.Root
// opened at the tree, so a swap between the walk and the open is refused
// rather than followed.

var (
	// ErrEscapesTree reports a path (or a symlink on it) that resolves
	// outside the tree, dangles, or is not local to the tree.
	ErrEscapesTree = errors.New("path resolves outside the tree")
	// ErrNotRegularFile reports a leaf that is not a regular file (a
	// directory, FIFO, socket, or device node).
	ErrNotRegularFile = errors.New("not a regular file")
	// ErrFileTooLarge reports a file above MaxTreeFileSize.
	ErrFileTooLarge = errors.New("file exceeds the read cap")
)

// MaxTreeFileSize caps ReadInTree: scaffold files are a few KiB.
const MaxTreeFileSize = 1 << 20

// ReadInTree reads the regular file at rel (slash-separated, relative) under
// tree. An absent file reports fs.ErrNotExist; a path that leaves the tree
// reports ErrEscapesTree; a non-file leaf ErrNotRegularFile; an oversize
// file ErrFileTooLarge.
func ReadInTree(tree, rel string) ([]byte, error) {
	fi, err := confineInTree(tree, rel)
	if err != nil {
		return nil, err
	}
	if fi == nil {
		return nil, &fs.PathError{Op: "read", Path: filepath.Join(tree, rel), Err: fs.ErrNotExist}
	}
	if fi.Size() > MaxTreeFileSize {
		return nil, fmt.Errorf("%s: %d bytes: %w", filepath.Join(tree, rel), fi.Size(), ErrFileTooLarge)
	}
	r, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// O_NONBLOCK: a leaf swapped for a FIFO after the walk does not hang the
	// open; the fstat below refuses it.
	f, err := r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %s: %w", filepath.Join(tree, rel), st.Mode().Type(), ErrNotRegularFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxTreeFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxTreeFileSize {
		return nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrFileTooLarge)
	}
	return b, nil
}

// ErrSymlink reports a symbolic link on a path read with ReadInTreeNoLinks.
var ErrSymlink = errors.New("symbolic link refused")

// StatInTreeNoLinks walks rel below tree with Lstat and returns the leaf's
// info. Any symbolic link on the way (in-tree or not) is ErrSymlink; a path
// that is not local is ErrEscapesTree; an absent component passes
// fs.ErrNotExist through. The tree path itself may be a link (the
// operator's own layout); nothing below it may.
func StatInTreeNoLinks(tree, rel string) (fs.FileInfo, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, fmt.Errorf("%q: %w", rel, ErrEscapesTree)
	}
	cur := tree
	var fi fs.FileInfo
	for _, p := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, p)
		var err error
		if fi, err = os.Lstat(cur); err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", cur, ErrSymlink)
		}
	}
	return fi, nil
}

// afterNoLinkWalk is a test seam run between ReadInTreeNoLinks's walk and
// its open.
var afterNoLinkWalk func()

// ReadInTreeNoLinks reads a jail-written file host-side: a regular file at
// most max bytes, with no symbolic link on any component of rel. The open
// goes through an os.Root at tree and must land on the file the walk saw
// (os.SameFile), so a swap between the walk and the open is refused.
func ReadInTreeNoLinks(tree, rel string, max int64) ([]byte, error) {
	leaf, err := StatInTreeNoLinks(tree, rel)
	if err != nil {
		return nil, err
	}
	if !leaf.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %s: %w", filepath.Join(tree, rel), leaf.Mode().Type(), ErrNotRegularFile)
	}
	if leaf.Size() > max {
		return nil, fmt.Errorf("%s: %d bytes: %w", filepath.Join(tree, rel), leaf.Size(), ErrFileTooLarge)
	}
	if afterNoLinkWalk != nil {
		afterNoLinkWalk()
	}
	r, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// O_NONBLOCK: a leaf swapped for a FIFO after the walk does not hang
	// the open; SameFile below then refuses it.
	f, err := r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(st, leaf) || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s changed while it was read: %w", filepath.Join(tree, rel), ErrSymlink)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrFileTooLarge)
	}
	return b, nil
}

// afterNoLinkStep is a test seam run between a directory's Lstat and its
// open in rootNoLinks.
var afterNoLinkStep func(name string)

// rootNoLinks opens dir below tree as an os.Root one component at a time.
// Each component is Lstat-ed through its parent's Root (a link is
// ErrSymlink), opened, and must be the directory the Lstat saw. os.Root
// alone follows a link that stays inside it; the SameFile check is what
// refuses a component swapped for a link between check and open. With
// mkdir, a missing component is created (Mkdir never follows a link). ""
// and "." are the tree itself, which may be a link (the operator's own
// layout). The caller closes the Root.
func rootNoLinks(tree, dir string, mkdir bool, perm os.FileMode) (*os.Root, error) {
	cur, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	if dir == "" || dir == "." {
		return cur, nil
	}
	for _, p := range strings.Split(dir, "/") {
		fi, err := cur.Lstat(p)
		if mkdir && errors.Is(err, fs.ErrNotExist) {
			if err = cur.Mkdir(p, perm); err == nil || errors.Is(err, fs.ErrExist) {
				fi, err = cur.Lstat(p)
			}
		}
		if err != nil {
			cur.Close()
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			cur.Close()
			return nil, fmt.Errorf("%s: %w", p, ErrSymlink)
		}
		if !fi.IsDir() {
			cur.Close()
			return nil, fmt.Errorf("%s: not a directory", p)
		}
		if afterNoLinkStep != nil {
			afterNoLinkStep(p)
		}
		next, err := cur.OpenRoot(p)
		if err != nil {
			// A link swapped in after the Lstat is ErrSymlink; anything else
			// (EACCES, a removed directory) is the real error.
			now, lerr := cur.Lstat(p)
			cur.Close()
			if lerr == nil && (now.Mode()&fs.ModeSymlink != 0 || !os.SameFile(now, fi)) {
				return nil, fmt.Errorf("%s: %w", p, ErrSymlink)
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		cur.Close()
		if st, err := next.Stat("."); err != nil || !os.SameFile(st, fi) {
			next.Close()
			return nil, fmt.Errorf("%s changed while it was opened: %w", p, ErrSymlink)
		}
		cur = next
	}
	return cur, nil
}

// OpenDirInTreeNoLinks opens the directory rel below tree as an os.Root
// with no symbolic link on any component. The caller closes it.
func OpenDirInTreeNoLinks(tree, rel string) (*os.Root, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, fmt.Errorf("%q: %w", rel, ErrEscapesTree)
	}
	return rootNoLinks(tree, rel, false, 0)
}

func splitNoLinks(rel string) (dir, leaf string, err error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", "", fmt.Errorf("%q: %w", rel, ErrEscapesTree)
	}
	dir, leaf = path.Split(rel)
	return strings.TrimSuffix(dir, "/"), leaf, nil
}

// CreateInTreeNoLinks creates a new regular file at rel below tree, making
// missing parents with dirPerm. No component below tree may be a symbolic
// link at check or at open, and the leaf is created O_EXCL, which never
// follows a link: an existing entry of any kind is fs.ErrExist. The file is
// chmod-ed to perm (umask does not apply).
func CreateInTreeNoLinks(tree, rel string, dirPerm, perm os.FileMode) (*os.File, error) {
	dir, leaf, err := splitNoLinks(rel)
	if err != nil {
		return nil, err
	}
	r, err := rootNoLinks(tree, dir, true, dirPerm)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.OpenFile(leaf, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrNotRegularFile)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// afterReplaceWrite is a test seam run between ReplaceInTreeNoLinks's temp
// file write and its rename.
var afterReplaceWrite func(dir *os.Root, tmp string)

// ReplaceInTreeNoLinks writes data as a new regular file at rel below tree
// (mode perm, umask aside) and renames it over whatever entry rel names,
// making missing parents with dirPerm. No component below tree may be a
// symbolic link.
//
// The existing entry is never opened: the agent can make it a hard link to
// another file (an in-place write would land in that file), a FIFO (an open
// would block for ever), or a link swapped in after any check. The rename
// replaces the entry itself; a directory there makes it fail. The temp file
// is created O_EXCL under a random name in the same directory, so the agent
// cannot pre-plant it, and is removed on any failure.
//
// The directory is held open from the walk on, so the file lands in the
// directory the walk checked even if the agent renames it away meanwhile;
// moving its own directory gains the agent nothing it could not do with a
// copy.
func ReplaceInTreeNoLinks(tree, rel string, data []byte, dirPerm, perm os.FileMode) error {
	dir, leaf, err := splitNoLinks(rel)
	if err != nil {
		return err
	}
	r, err := rootNoLinks(tree, dir, true, dirPerm)
	if err != nil {
		return err
	}
	defer r.Close()
	f, tmp, err := createTempIn(r, leaf, perm)
	if err != nil {
		return err
	}
	// f stays open until the check after the rename: a closed and unlinked
	// inode's number can be reused at once, which would defeat SameFile.
	defer f.Close()
	renamed := false
	defer func() {
		if !renamed {
			_ = r.Remove(tmp)
		}
	}()
	st, err := writeSynced(f, data, perm)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(tree, dir, tmp), err)
	}
	if afterReplaceWrite != nil {
		afterReplaceWrite(r, tmp)
	}
	if err := r.Rename(tmp, leaf); err != nil {
		return err
	}
	renamed = true
	// The temp name sits in an agent-writable directory: an entry swapped in
	// for it before the rename is now at leaf instead of the data written.
	now, err := r.Lstat(leaf)
	if err != nil {
		return fmt.Errorf("%s after the replace: %w", filepath.Join(tree, rel), err)
	}
	if !now.Mode().IsRegular() || !os.SameFile(now, st) {
		return fmt.Errorf("%s changed while it was replaced: %w", filepath.Join(tree, rel), ErrSymlink)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Best effort, as in syncDir: some file systems refuse a directory sync.
	if d, err := r.Open("."); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// createTempIn creates a new regular file in r named after leaf with a
// random suffix. O_EXCL never follows a link and fails on any existing
// entry, so a name the agent planted is skipped, never reused.
func createTempIn(r *os.Root, leaf string, perm os.FileMode) (*os.File, string, error) {
	var err error
	for range 8 {
		var b [8]byte
		if _, err = rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := "." + leaf + ".tmp-" + hex.EncodeToString(b[:])
		var f *os.File
		f, err = r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, perm)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", err
}

// writeSynced writes data to f, sets perm and syncs it, and returns the
// file's info as written. The caller closes f.
func writeSynced(f *os.File, data []byte, perm os.FileMode) (fs.FileInfo, error) {
	if _, err := f.Write(data); err != nil {
		return nil, err
	}
	if err := f.Chmod(perm); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	return st, nil
}

// OpenInTreeNoLinks opens the regular file at rel below tree for reading,
// with no symbolic link on any component and one name only (ErrHardLink).
// The file may still grow past max: the caller reads through
// io.LimitReader(f, max+1).
func OpenInTreeNoLinks(tree, rel string, max int64) (*os.File, fs.FileInfo, error) {
	dir, leaf, err := splitNoLinks(rel)
	if err != nil {
		return nil, nil, err
	}
	r, err := rootNoLinks(tree, dir, false, 0)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	fi, err := r.Lstat(leaf)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrSymlink)
	case !fi.Mode().IsRegular():
		return nil, nil, fmt.Errorf("%s: %s: %w", filepath.Join(tree, rel), fi.Mode().Type(), ErrNotRegularFile)
	case fi.Size() > max:
		return nil, nil, fmt.Errorf("%s: %d bytes: %w", filepath.Join(tree, rel), fi.Size(), ErrFileTooLarge)
	}
	if afterNoLinkWalk != nil {
		afterNoLinkWalk()
	}
	f, err := r.OpenFile(leaf, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrSymlink)
	}
	st, err := f.Stat()
	if err != nil || !os.SameFile(st, fi) || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while it was opened: %w", filepath.Join(tree, rel), ErrSymlink)
	}
	if hardLinked(st) {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", filepath.Join(tree, rel), ErrHardLink)
	}
	return f, st, nil
}

// ErrHardLink reports a regular file with more than one name: a second name
// in the agent's directory says nothing about where the inode came from.
var ErrHardLink = errors.New("hard link refused")

func hardLinked(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}

// RemoveInTreeNoLinks removes the entry at rel (a file, or a link itself,
// never its target), reached with no symbolic link on its directories.
func RemoveInTreeNoLinks(tree, rel string) error {
	dir, leaf, err := splitNoLinks(rel)
	if err != nil {
		return err
	}
	r, err := rootNoLinks(tree, dir, false, 0)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Remove(leaf)
}

// WriteInTree writes data to rel under tree in place (truncate and write,
// never a rename), creating missing parents. An existing file keeps its
// inode and mode. A symlink on the path is followed only while it stays
// inside the tree; anything else is refused before any directory is
// created.
func WriteInTree(tree, rel string, data []byte, perm os.FileMode) error {
	if _, err := confineInTree(tree, rel); err != nil {
		return err
	}
	r, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer r.Close()
	name := filepath.FromSlash(rel)
	if dir := filepath.Dir(name); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NONBLOCK, perm)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err != nil {
		f.Close()
		return err
	} else if !st.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("%s: %s: %w", filepath.Join(tree, rel), st.Mode().Type(), ErrNotRegularFile)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// confineInTree walks rel below tree one component at a time and returns
// the link-resolved info of the leaf, nil when the path is absent, or
// ErrEscapesTree (not local, or a link resolving outside the real tree or
// dangling), ErrNotRegularFile, or a plain error when an intermediate
// component is not a directory.
func confineInTree(tree, rel string) (fs.FileInfo, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, fmt.Errorf("%q: %w", rel, ErrEscapesTree)
	}
	realTree, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, "/")
	cur := tree
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: dangling symbolic link: %w", cur, ErrEscapesTree)
			}
			if err != nil {
				return nil, err
			}
			if resolved != realTree && !strings.HasPrefix(resolved, realTree+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: symbolic link to %s: %w", cur, resolved, ErrEscapesTree)
			}
			if fi, err = os.Stat(cur); err != nil {
				return nil, err
			}
		}
		if i == len(parts)-1 {
			if !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: %s: %w", cur, fi.Mode().Type(), ErrNotRegularFile)
			}
			return fi, nil
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("%s: not a directory", cur)
		}
	}
	return nil, nil // unreachable: the loop returns on the leaf
}

// RelOverlapFold reports whether two tree-relative paths name the same
// directory or one contains the other, with Unicode case folding: on a
// case-insensitive host filesystem (APFS by default) a byte compare would
// let a config or an agent-chosen path name a protected directory under
// another spelling. "." overlaps everything.
func RelOverlapFold(a, b string) bool {
	ca, cb := relComponents(a), relComponents(b)
	n := min(len(ca), len(cb))
	for i := range n {
		if !strings.EqualFold(ca[i], cb[i]) {
			return false
		}
	}
	return true
}

func relComponents(p string) []string {
	c := filepath.ToSlash(filepath.Clean(p))
	if c == "." || c == "" {
		return nil
	}
	return strings.Split(c, "/")
}
