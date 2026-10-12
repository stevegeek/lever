package voice

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// SpeechDir is where m's files live in the models directory dir: one
// private directory per model, laid out as the repository.
func SpeechDir(dir string, m SpeechModel) string { return filepath.Join(dir, m.Name) }

// SpeechPath is where file rel (a path in m's Files) lives in dir.
func SpeechPath(dir string, m SpeechModel, rel string) string {
	return filepath.Join(SpeechDir(dir, m), filepath.FromSlash(rel))
}

// fileModel is one file of m as the single-file fetch and check see it.
func (m SpeechModel) fileModel(f SpeechFile) Model {
	return Model{Name: m.Name + " " + f.Path, File: path.Base(f.Path), Revision: m.Revision, Size: f.Size, SHA256: f.SHA256}
}

// FetchSpeech downloads every file of m that is not there verified into
// SpeechDir(dir, m), from Hugging Face at the pinned commit, and returns
// that directory. Each file is kept only at its pinned size and sha256
// (Fetcher.FetchFrom), mode 0600, in directories made 0700.
func (f Fetcher) FetchSpeech(ctx context.Context, m SpeechModel, dir string) (string, error) {
	return f.FetchSpeechFrom(ctx, m, dir, m.FileURL)
}

// FetchSpeechFrom is FetchSpeech from src(file) instead of the table's URLs
// (tests).
func (f Fetcher) FetchSpeechFrom(ctx context.Context, m SpeechModel, dir string, src func(SpeechFile) string) (string, error) {
	if why := m.Pinned(); why != "" {
		return "", fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	if err := PrivateDir(dir); err != nil {
		return "", err
	}
	root := SpeechDir(dir, m)
	if err := PrivateDir(root); err != nil {
		return "", err
	}
	for _, file := range m.Files {
		sub, err := privateDirs(root, path.Dir(file.Path), true)
		if err != nil {
			return "", err
		}
		if _, err := f.FetchFrom(ctx, m.fileModel(file), src(file), sub); err != nil {
			return "", err
		}
	}
	return root, nil
}

// privateDirs checks (and with create, makes) each directory of rel below
// root, every one a private directory of the caller's, and returns the
// last. rel "." is root itself.
func privateDirs(root, rel string, create bool) (string, error) {
	p := root
	if err := checkPrivateDir(p, create); err != nil {
		return "", err
	}
	if rel == "." {
		return p, nil
	}
	for _, c := range strings.Split(rel, "/") {
		p = filepath.Join(p, c)
		if err := checkPrivateDir(p, create); err != nil {
			return "", err
		}
	}
	return p, nil
}

func checkPrivateDir(p string, create bool) error {
	if create {
		return PrivateDir(p)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory (a symbolic link?)", ErrModel, p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is %v: other users can reach it; chmod 700 it", ErrModel, p, fi.Mode().Perm())
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d, not to you", ErrModel, p, owner)
	}
	return nil
}

// VerifySpeech checks every file of m in dir: each directory on the way a
// private directory of the caller's (no symbolic link), each file a
// regular file of the caller's of the pinned size and sha256 (Verify). A
// missing file or directory is fs.ErrNotExist. The model's files are
// hashed whole, which takes a moment for a large model.
func VerifySpeech(dir string, m SpeechModel) error {
	if why := m.Pinned(); why != "" {
		return fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	root := SpeechDir(dir, m)
	for _, file := range m.Files {
		sub, err := privateDirs(root, path.Dir(file.Path), false)
		if err != nil {
			return err
		}
		if err := Verify(filepath.Join(sub, path.Base(file.Path)), m.fileModel(file)); err != nil {
			return err
		}
	}
	return nil
}

// speechStamp is a fingerprint of the files and directories of m in dir as
// the file system describes them (device, inode, size, times, owner, mode,
// links), without reading them: a file replaced or written changes it (its
// ctime at least). "" when the platform has no such description, or a
// file is missing: then only a full VerifySpeech will do.
func speechStamp(dir string, m SpeechModel) string {
	root := SpeechDir(dir, m)
	var b strings.Builder
	seen := map[string]bool{}
	add := func(p string) bool {
		if seen[p] {
			return true
		}
		seen[p] = true
		fi, err := os.Lstat(p)
		if err != nil {
			return false
		}
		s := fileStamp(fi)
		if s == "" {
			return false
		}
		b.WriteString(p)
		b.WriteByte('\x00')
		b.WriteString(s)
		b.WriteByte('\n')
		return true
	}
	if !add(root) {
		return ""
	}
	for _, f := range m.Files {
		d := root
		if rel := path.Dir(f.Path); rel != "." {
			for _, c := range strings.Split(rel, "/") {
				d = filepath.Join(d, c)
				if !add(d) {
					return ""
				}
			}
		}
		if !add(filepath.Join(d, path.Base(f.Path))) {
			return ""
		}
	}
	return b.String()
}

// speechMissing reports whether err is a missing model file or directory.
func speechMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
