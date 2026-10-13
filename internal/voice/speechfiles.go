package voice

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SpeechDir is where m's files live in the models directory dir: one
// private directory per model, laid out as below the archive's top
// directory.
func SpeechDir(dir string, m SpeechModel) string { return filepath.Join(dir, m.Name) }

// SpeechPath is where file rel (a path below m's directory) lives in dir.
func SpeechPath(dir string, m SpeechModel, rel string) string {
	return filepath.Join(SpeechDir(dir, m), filepath.FromSlash(rel))
}

// The bounds of one model's archive, against a damaged one (its sha256 is
// pinned, so not against a hostile one): entries, and bytes unpacked.
const (
	maxSpeechEntries = 20000
	maxSpeechBytes   = 4 << 30
)

// speechManifest is the file lever writes into a model's directory once it
// has unpacked the model's verified archive there: the archive's sha256
// and the path, size and sha256 of every file unpacked. No archive entry
// can have its name (it starts with a dot).
const speechManifest = ".lever-manifest"

// maxSpeechManifest bounds the manifest lever reads.
const maxSpeechManifest = 4 << 20

type speechManifestJSON struct {
	ArchiveSHA256 string       `json:"archive_sha256"`
	Files         []SpeechFile `json:"files"`
}

// FetchSpeech makes SpeechDir(dir, m) hold m's files, unpacked from its
// pinned archive, unless they are there verified already (VerifySpeech),
// and returns that directory. The archive is downloaded from GitHub to a
// private temp file in dir and used only at its pinned size and sha256
// (Fetcher.download); it is unpacked into a private temp directory
// (unpackSpeech: regular files 0600 and directories 0700 only), its
// manifest written, and that directory renamed into place, replacing the
// old one. The archive is deleted.
func (f Fetcher) FetchSpeech(ctx context.Context, m SpeechModel, dir string) (string, error) {
	return f.FetchSpeechFrom(ctx, m, dir, m.ArchiveURL)
}

// FetchSpeechFrom is FetchSpeech from src instead of the table's URL
// (tests).
func (f Fetcher) FetchSpeechFrom(ctx context.Context, m SpeechModel, dir, src string) (string, error) {
	if why := m.Pinned(); why != "" {
		return "", fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	if err := PrivateDir(dir); err != nil {
		return "", err
	}
	root := SpeechDir(dir, m)
	if _, err := VerifySpeech(dir, m); err == nil {
		return root, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A model that is there but wrong is replaced below, never used.
		f.say("%s: the files there do not verify (%v); downloading again\n", m.Name, err)
	}
	archive, err := f.download(ctx, download{name: m.Name, src: src, size: m.ArchiveSize, sha256: m.ArchiveSHA256,
		allow: GitHubReleaseHost, hosts: "a GitHub release host"}, dir)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)
	tmp, err := os.MkdirTemp(dir, ".fetch-"+m.Name+"-")
	if err != nil {
		return "", err
	}
	placed := false
	defer func() {
		if !placed {
			os.RemoveAll(tmp)
		}
	}()
	f.say("%s: unpacking\n", m.Name)
	files, err := unpackSpeech(archive, tmp, m)
	if err != nil {
		return "", err
	}
	if err := writeSpeechManifest(tmp, m, files); err != nil {
		return "", err
	}
	// The old copy, if any, moves aside (a name removeStaleFetches knows)
	// and goes once the new one is in place.
	old := ""
	if _, err := os.Lstat(root); err == nil {
		old = filepath.Join(dir, ".fetch-old-"+m.Name+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Rename(root, old); err != nil {
			return "", err
		}
		// Fresh times, so another fetch does not take it for stale.
		now := time.Now()
		_ = os.Chtimes(old, now, now)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(tmp, root); err != nil {
		if old != "" {
			_ = os.Rename(old, root)
		}
		return "", err
	}
	placed = true
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return root, nil
}

// unpackSpeech unpacks m's archive (bzip2-compressed tar) at p into the
// private directory into (unpackSpeechTar).
func unpackSpeech(p, into string, m SpeechModel) ([]SpeechFile, error) {
	file, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return unpackSpeechTar(tar.NewReader(bzip2.NewReader(bufio.NewReader(file))), into, m)
}

// unpackSpeechTar unpacks the entries below the archive's top directory
// m.ArchiveRoot into the private directory into, and returns the files
// unpacked, in path order. Only directories (made 0700) and regular files
// (written 0600, each once) are unpacked; an entry with a name component
// that starts with a dot (such as macOS's ._ files) is skipped. Anything
// else refuses the archive: an entry outside the top directory, a name
// that is not a plain relative path, a link or device, too many entries
// or bytes, and an archive without one of m's RequiredFiles.
func unpackSpeechTar(tr *tar.Reader, into string, m SpeechModel) ([]SpeechFile, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s's archive: %s", ErrModel, m.Name, fmt.Sprintf(format, a...))
	}
	var files []SpeechFile
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad("%v", err)
		}
		if n >= maxSpeechEntries {
			return nil, bad("more than %d entries", maxSpeechEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		top, rel, _ := strings.Cut(name, "/")
		switch {
		case top != m.ArchiveRoot:
			return nil, bad("the entry %q is not below %s/", h.Name, m.ArchiveRoot)
		case rel == "" && h.Typeflag == tar.TypeDir:
			continue
		case rel != "" && hiddenSpeechPath(rel):
			continue
		case !archivePathOK(rel):
			return nil, bad("the entry %q is not a plain relative path", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if _, err := privateDirs(into, rel, true); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if h.Size < 0 || total+h.Size > maxSpeechBytes {
				return nil, bad("more than %d bytes", int64(maxSpeechBytes))
			}
			total += h.Size
			sum, err := unpackSpeechFile(tr, into, rel, h.Size)
			if err != nil {
				return nil, bad("%s: %v", h.Name, err)
			}
			files = append(files, SpeechFile{Path: rel, Size: h.Size, SHA256: sum})
		default:
			return nil, bad("the entry %q is not a regular file or a directory (type %q)", h.Name, h.Typeflag)
		}
	}
	slices.SortFunc(files, func(a, b SpeechFile) int { return strings.Compare(a.Path, b.Path) })
	if why := speechMissingRole(m, files); why != "" {
		return nil, bad("%s", why)
	}
	return files, nil
}

// unpackSpeechFile writes the next size bytes of tr to rel below into, a
// new file (an existing one, a second entry of the same name, is refused)
// of mode 0600, and returns their sha256.
func unpackSpeechFile(tr io.Reader, into, rel string, size int64) (string, error) {
	sub, err := privateDirs(into, path.Dir(rel), true)
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(filepath.Join(sub, path.Base(rel)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("%d of %d bytes", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// speechMissingRole names a file of m's RequiredFiles that files lacks
// ("" = none).
func speechMissingRole(m SpeechModel, files []SpeechFile) string {
	for _, p := range m.RequiredFiles() {
		if !slices.ContainsFunc(files, func(f SpeechFile) bool { return f.Path == p }) {
			return fmt.Sprintf("it has no %s, which sherpa-onnx needs", p)
		}
	}
	return ""
}

// hiddenSpeechPath reports whether a component of rel starts with a dot
// and none is "." or ".." (such a path is refused, not skipped).
func hiddenSpeechPath(rel string) bool {
	cs := strings.Split(rel, "/")
	return !slices.Contains(cs, ".") && !slices.Contains(cs, "..") &&
		slices.ContainsFunc(cs, func(c string) bool { return strings.HasPrefix(c, ".") })
}

// archivePathOK accepts a relative slash path of plain components, as
// cleanSpeechPath, except that a space may be inside a component
// (espeak-ng's data has a voice file named "Mr serious").
func archivePathOK(rel string) bool {
	if rel == "" || path.Clean(rel) != rel || path.IsAbs(rel) || strings.Contains(rel, `\`) {
		return false
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasPrefix(c, " ") || strings.HasSuffix(c, " ") {
			return false
		}
		for _, r := range c {
			if r < 0x20 || r > 0x7e {
				return false
			}
		}
	}
	return true
}

// writeSpeechManifest writes the manifest of files, unpacked from m's
// archive, into dir: a new file, mode 0600.
func writeSpeechManifest(dir string, m SpeechModel, files []SpeechFile) error {
	b, err := json.Marshal(speechManifestJSON{ArchiveSHA256: m.ArchiveSHA256, Files: files})
	if err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir, speechManifest), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	if cerr := out.Sync(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// readSpeechManifest reads the manifest in m's directory root: a regular
// file of the caller's that no one else can write, written for m's pinned
// archive, listing plain paths once each with a size and sha256, among
// them m's RequiredFiles.
func readSpeechManifest(root string, m SpeechModel) ([]SpeechFile, error) {
	p := filepath.Join(root, speechManifest)
	file, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s has no %s: lever voice fetch did not unpack it", ErrModel, root, speechManifest)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrModel, p, err)
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrModel, p)
	case fi.Mode().Perm()&0o022 != 0:
		return nil, fmt.Errorf("%w: %s is %v: another user can change it", ErrModel, p, fi.Mode().Perm())
	case fi.Size() > maxSpeechManifest:
		return nil, fmt.Errorf("%w: %s is too large", ErrModel, p)
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return nil, fmt.Errorf("%w: %s belongs to uid %d, not to you", ErrModel, p, owner)
	}
	b, err := io.ReadAll(io.LimitReader(file, maxSpeechManifest+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSpeechManifest {
		return nil, fmt.Errorf("%w: %s is too large", ErrModel, p)
	}
	var man speechManifestJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&man); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrModel, p, err)
	}
	if man.ArchiveSHA256 != m.ArchiveSHA256 {
		return nil, fmt.Errorf("%w: %s was unpacked from another archive than the one this lever pins (sha256 %s)", ErrModel, root, m.ArchiveSHA256)
	}
	seen := map[string]bool{}
	for _, f := range man.Files {
		switch {
		case !archivePathOK(f.Path) || seen[f.Path]:
			return nil, fmt.Errorf("%w: %s lists %q, not a plain path once", ErrModel, p, f.Path)
		case f.Size < 0 || !isHex(f.SHA256, 64):
			return nil, fmt.Errorf("%w: %s: %s has no size and sha256", ErrModel, p, f.Path)
		}
		seen[f.Path] = true
	}
	if why := speechMissingRole(m, man.Files); why != "" {
		return nil, fmt.Errorf("%w: %s: %s", ErrModel, p, why)
	}
	return man.Files, nil
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

// VerifySpeech checks m's directory in dir and returns its files: the
// directory and every one below it a private directory of the caller's (no
// symbolic link), its manifest one lever wrote for m's pinned archive
// (readSpeechManifest), nothing else in it but the files the manifest
// lists, and each of them a regular file of the caller's of the size and
// sha256 the manifest records (verifyFile). So every file the engine can
// load is as lever unpacked it from the verified archive. The manifest
// itself is trusted as lever wrote it, in a private directory: it is not
// pinned. A missing directory is fs.ErrNotExist; a listed file missing is
// ErrModel. The files are hashed whole, which takes a moment
// for a large model.
func VerifySpeech(dir string, m SpeechModel) ([]SpeechFile, error) {
	if why := m.Pinned(); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	root := SpeechDir(dir, m)
	if _, err := os.Lstat(root); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(root, false); err != nil {
		return nil, err
	}
	files, err := readSpeechManifest(root, m)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[f.Path] = true
	}
	// Every file is looked for first, so a missing one is found before any
	// file is hashed.
	for _, f := range files {
		if _, err := os.Lstat(SpeechPath(dir, m, f.Path)); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s is missing from %s", ErrModel, f.Path, root)
		} else if err != nil {
			return nil, err
		}
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return checkPrivateDir(p, false)
		case rel == speechManifest || listed[rel]:
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symbolic link, not among the files lever unpacked", ErrModel, p)
		}
		return fmt.Errorf("%w: %s is not among the files lever unpacked for %s", ErrModel, p, m.Name)
	})
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := verifyFile(SpeechPath(dir, m, f.Path), f.Size, f.SHA256); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// speechStamp is a fingerprint of m's directory in dir and everything in
// it, as the file system describes them (device, inode, size, times,
// owner, mode, links), without reading a file: a file replaced, written,
// added or removed changes it (the ctime of a file or of its directory at
// least), and so does a missing directory appearing. "" when the platform
// has no such description: then only a full VerifySpeech will do.
func speechStamp(dir string, m SpeechModel) string {
	root := SpeechDir(dir, m)
	var b strings.Builder
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		// Recorded as missing, so a failed check of a model not fetched
		// yet is remembered rather than run again.
		return root + "\x00missing\n"
	}
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		s := fileStamp(fi)
		if s == "" {
			return errors.New("no stamp")
		}
		b.WriteString(p)
		b.WriteByte('\x00')
		b.WriteString(s)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		return ""
	}
	return b.String()
}

// speechMissing reports whether err is a missing model file or directory.
func speechMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
