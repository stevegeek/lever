package treebackup

import (
	"io/fs"
	"os"
	"syscall"
	"unicode/utf8"
)

// restoreFSWrap lets tests interpose on the restore's system calls. It is
// nil outside tests.
var restoreFSWrap func(restoreFS) restoreFS

// foldingFS makes the target behave like a case-insensitive filesystem:
// creating a name that folds to an existing one fails with EEXIST.
type foldingFS struct{ restoreFS }

func (f foldingFS) taken(dirfd int, name string) (bool, error) {
	fd, err := f.restoreFS.openDir(dirfd, ".")
	if err != nil {
		return false, err
	}
	d := os.NewFile(uintptr(fd), ".")
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return false, err
	}
	k := foldKey(name)
	for _, n := range names {
		if foldKey(n) == k {
			return true, nil
		}
	}
	return false, nil
}

func (f foldingFS) mkdirat(dirfd int, name string) error {
	if t, err := f.taken(dirfd, name); err != nil || t {
		return firstErr(err, fs.ErrExist)
	}
	return f.restoreFS.mkdirat(dirfd, name)
}

func (f foldingFS) createFile(dirfd int, name string) (int, error) {
	if t, err := f.taken(dirfd, name); err != nil || t {
		return -1, firstErr(err, fs.ErrExist)
	}
	return f.restoreFS.createFile(dirfd, name)
}

func (f foldingFS) symlinkat(target string, dirfd int, name string) error {
	if t, err := f.taken(dirfd, name); err != nil || t {
		return firstErr(err, fs.ErrExist)
	}
	return f.restoreFS.symlinkat(target, dirfd, name)
}

// utf8OnlyFS refuses names that are not valid UTF-8 with EILSEQ, as APFS
// does.
type utf8OnlyFS struct{ restoreFS }

func (f utf8OnlyFS) mkdirat(dirfd int, name string) error {
	if !utf8.ValidString(name) {
		return syscall.EILSEQ
	}
	return f.restoreFS.mkdirat(dirfd, name)
}

func (f utf8OnlyFS) createFile(dirfd int, name string) (int, error) {
	if !utf8.ValidString(name) {
		return -1, syscall.EILSEQ
	}
	return f.restoreFS.createFile(dirfd, name)
}

func (f utf8OnlyFS) symlinkat(target string, dirfd int, name string) error {
	if !utf8.ValidString(name) {
		return syscall.EILSEQ
	}
	return f.restoreFS.symlinkat(target, dirfd, name)
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
