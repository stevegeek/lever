// Package fsutil holds the stdlib-only file helpers the host packages share.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WriteFileAtomic writes data to a temp file in the same directory as path
// then renames it over path — atomic on POSIX, so a crash mid-write leaves
// either the old file or the new one, never a torn partial write. The rename
// replaces whatever path was (a symlink included) with a regular file.
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Tree-confined file access.
//
// Everything under an instance tree is agent-writable, and the host process
// that scaffolds files into it runs as the operator. A plain os.ReadFile or
// os.WriteFile there follows whatever the agent planted: a CLAUDE.md, a
// SKILL.md or the `.claude` directory replaced by a symlink to the instance's
// lever.yaml, the operator's shell rc, or the state dir turns the operator's
// next `lever init` into a write to that host file. ReadInTree and
// WriteInTree confine every path to the tree: a symlink may be followed only
// while it resolves INSIDE the tree (an operator's own `CLAUDE.md ->
// docs/CLAUDE.md` keeps working, written in place so the link and the
// target's mode survive), and a link that leaves the tree, dangles, or ends
// on anything but a regular file is refused with a named error. Reads are
// capped so a FIFO or a device node cannot hang the host.
//
// The name walk gives the named errors; the I/O itself goes through an
// os.Root opened at the tree, which re-checks every component at open time,
// so a swap between the walk and the open is refused rather than followed.

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

// MaxTreeFileSize caps ReadInTree: scaffold files are a few KiB, so anything
// near the cap is not one of ours.
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
	// O_NONBLOCK: should the leaf become a FIFO between the walk and here,
	// the open returns instead of waiting for a writer; the fstat below then
	// refuses it.
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

// afterNoLinkWalk, when set, runs between ReadInTreeNoLinks's walk and its
// open: a test seam for a leaf swapped in that window.
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

// WriteInTree writes data to rel under tree IN PLACE (truncate + write, never
// a rename), creating missing parent directories. An existing file keeps its
// inode and mode; perm applies only when the file is created. A symlink on
// the path is followed only while it stays inside the tree — anything else
// is refused with ErrEscapesTree or ErrNotRegularFile before any directory
// is created.
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

// confineInTree walks rel below tree one component at a time. It returns the
// (link-resolved) info of the leaf, nil when the path is absent from some
// component down, or a named error: ErrEscapesTree when rel is not local or
// a symlink component resolves outside the real tree (or dangles — a target
// that does not exist cannot be placed), ErrNotRegularFile when the leaf
// exists and is not a regular file, or a plain error when an intermediate
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
