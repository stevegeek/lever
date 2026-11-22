package broker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

// A spawned agent's workspace is <tree>/<dir_root>/<name>. The tree is
// agent-writable (the manager mounts all of it) and the broker acts there
// as the operator, so every step below reaches the directory with no
// symbolic link on its path, through an os.Root held open, and checks it is
// the directory spawn created (its device and inode, kept in the record).

// errDirSwapped: the workspace is not the directory spawn created.
var errDirSwapped = errors.New("the workspace is not the directory spawn created (replaced, moved or a link)")

// dirIdentity is a directory's "dev:ino".
func dirIdentity(fi fs.FileInfo) (string, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("no inode information")
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

func splitAgentDir(rel string) (root, name string) {
	root, name = path.Split(path.Clean(rel))
	return strings.TrimSuffix(root, "/"), name
}

// createAgentDir creates the new agent's workspace, exclusively: the name is
// fresh, so an entry already there was planted and refuses the spawn. The
// files are written through a handle on the directory just made, each
// created exclusively, so nothing outside it is reached whatever is
// swapped meanwhile. It returns the directory's identity.
func (b *Broker) createAgentDir(rel string, files map[string][]byte) (string, error) {
	if b.tree == "" {
		return "", errors.New("no instance tree is wired")
	}
	root, name := splitAgentDir(rel)
	parent, err := fsutil.MkdirInTreeNoLinks(b.tree, root, 0o755)
	if err != nil {
		return "", fmt.Errorf("dir_root %s: %w", root, err)
	}
	defer parent.Close()
	if err := parent.Mkdir(name, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", rel, err)
	}
	fi, err := parent.Lstat(name)
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s: %w", rel, errDirSwapped)
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if st, err := dir.Stat("."); err != nil || !os.SameFile(st, fi) {
		return "", fmt.Errorf("%s: %w", rel, errDirSwapped)
	}
	for p, data := range files {
		if err := writeNewFile(dir, p, data); err != nil {
			// The directory is the one just made: remove it through its
			// own handle, as destroy would.
			if rerr := removeAgentDir(parent, dir, name); rerr != nil {
				err = fmt.Errorf("%w (and removing it: %v)", err, rerr)
			}
			return "", fmt.Errorf("%s/%s: %w", rel, p, err)
		}
	}
	return dirIdentity(fi)
}

// writeNewFile creates p (and its parents) inside dir, exclusively and
// with no link at its leaf.
func writeNewFile(dir *os.Root, p string, data []byte) error {
	if d := path.Dir(p); d != "." {
		if err := dir.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	f, err := dir.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// openAgentDir opens the workspace of rec as an os.Root, refusing anything
// but the directory spawn created. The caller closes both.
func (b *Broker) openAgentDir(rec AgentRecord) (parent, dir *os.Root, err error) {
	if b.tree == "" {
		return nil, nil, errors.New("no instance tree is wired")
	}
	root, name := splitAgentDir(rec.Dir)
	if name == "" || rec.DirID == "" {
		return nil, nil, fmt.Errorf("%s: %w", rec.Dir, errDirSwapped)
	}
	parent, err = fsutil.OpenDirInTreeNoLinks(b.tree, root)
	if err != nil {
		return nil, nil, err
	}
	fi, err := parent.Lstat(name)
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	if id, ierr := dirIdentity(fi); fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || ierr != nil || id != rec.DirID {
		parent.Close()
		return nil, nil, fmt.Errorf("%s: %w", rec.Dir, errDirSwapped)
	}
	dir, err = parent.OpenRoot(name)
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	if st, err := dir.Stat("."); err != nil || !os.SameFile(st, fi) {
		dir.Close()
		parent.Close()
		return nil, nil, fmt.Errorf("%s: %w", rec.Dir, errDirSwapped)
	}
	return parent, dir, nil
}

// Archive limits: past any, the archive is refused and the workspace kept.
const (
	archiveMaxBytes = 8 << 30
	archiveMaxFiles = 200_000
	archiveTimeout  = 15 * time.Minute
)

var errArchiveLimit = errors.New("the workspace is past the archive limits")

// archiveDir writes dir as a gzip tar to out. Links are stored as links,
// never followed; only regular files are read, each opened with no link at
// its leaf and checked against the walk's Lstat; FIFOs, devices and sockets
// are skipped.
func archiveDir(ctx context.Context, dir *os.Root, out io.Writer) (files int, skipped int, err error) {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	var total int64
	walkErr := fs.WalkDir(dir.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if p == "." {
			return nil
		}
		fi, err := dir.Lstat(p)
		if err != nil {
			return err
		}
		if files++; files > archiveMaxFiles {
			return fmt.Errorf("%w (more than %d entries)", errArchiveLimit, archiveMaxFiles)
		}
		switch {
		case fi.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()})
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := dir.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: p, Linkname: target, Mode: 0o777, ModTime: fi.ModTime()})
		case fi.Mode().IsRegular():
		default:
			skipped++
			return nil
		}
		if total += fi.Size(); total > archiveMaxBytes {
			return fmt.Errorf("%w (more than %d GiB)", errArchiveLimit, archiveMaxBytes>>30)
		}
		f, err := dir.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || !os.SameFile(st, fi) {
			return fmt.Errorf("%s changed while it was archived", p)
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Size: st.Size(), Mode: int64(st.Mode().Perm()), ModTime: st.ModTime()}); err != nil {
			return err
		}
		// A file that grows meanwhile is cut at its size in the header.
		n, err := io.Copy(tw, io.LimitReader(f, st.Size()))
		if err != nil {
			return err
		}
		if n < st.Size() {
			return fmt.Errorf("%s shrank while it was archived", p)
		}
		return nil
	})
	if walkErr != nil {
		return files, skipped, walkErr
	}
	if err := tw.Close(); err != nil {
		return files, skipped, err
	}
	return files, skipped, gz.Close()
}

// archiveAgentDir packs rec's workspace into the archive directory as
// <name>.tar.gz (0600, created exclusively, synced) and returns its path.
func (b *Broker) archiveAgentDir(ctx context.Context, dir *os.Root, rec AgentRecord) (string, error) {
	if b.archiveDir == "" {
		return "", errors.New("no archive directory is wired")
	}
	if err := os.MkdirAll(b.archiveDir, 0o700); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(b.archiveDir); err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the archive directory %s is not a private directory", b.archiveDir)
	}
	dst := path.Join(b.archiveDir, rec.Name+".tar.gz")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	_, _, err = archiveDir(ctx, dir, f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// removeAgentDir empties the opened workspace through its own handle, so
// nothing outside it is reached whatever is renamed meanwhile, then removes
// the empty directory from its parent.
func removeAgentDir(parent, dir *os.Root, name string) error {
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := dir.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	return parent.Remove(name)
}
