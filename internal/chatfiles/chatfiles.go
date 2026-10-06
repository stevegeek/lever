// Package chatfiles is the file exchange between a login of the chat page
// and an agent (remote.files): where the files live in the agent's own
// workspace, what their names may be, and the three host operations on
// them — store an upload, hash a share, copy a recorded file for a download.
//
// Everything here runs as the operator on the host, in a directory the
// agent can write. So every path goes through fsutil's no-link walk, a new
// file is created O_EXCL, and a download serves a private copy whose hash
// was checked, never the tree file itself.
package chatfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

const (
	// Dir is the exchange directory in every agent's workspace.
	Dir = ".lever-files"
	// MaxNameLen bounds a file name, in bytes (names are ASCII).
	MaxNameLen = 120
	// ContainerWorkspace is where an agent's workspace is mounted in its
	// container (scion.ContainerWorkspace; a test pins the two together).
	ContainerWorkspace = "/workspace"
	// maxTries bounds the names Store tries in one second.
	maxTries = 10
)

// DefaultExtensions are the file types remote.files accepts when the
// config names none.
var DefaultExtensions = []string{"pdf", "xlsx", "xlsm", "xls", "csv", "docx", "doc", "png", "jpg", "jpeg", "txt", "zip"}

var (
	ErrTooLarge = errors.New("too-large")
	ErrChanged  = errors.New("changed")
	ErrBadPath  = errors.New("bad-path")
	ErrBusy     = errors.New("busy")
)

// Key names one login in paths: a fixed hash, so no login text (an email)
// appears in the agent's tree. Case-folded like every lever login record
// (the hub lowercases emails).
func Key(login string) string {
	h := sha256.Sum256([]byte("lever-files\x00" + strings.ToLower(login)))
	return "k" + hex.EncodeToString(h[:12])
}

// InDir and OutDir are a login's upload and share directories in the
// workspace ws, tree-relative ("." is the manager's workspace, the tree).
func InDir(ws, login string) string  { return path.Join(ws, Dir, "in", Key(login)) }
func OutDir(ws, login string) string { return path.Join(ws, Dir, "out", Key(login)) }

// SanitizeName reduces a client- or agent-given name to a plain file name:
// the part after the last / or \, every byte outside [A-Za-z0-9._ -] as
// "_", no leading or trailing space, no trailing dot, a leading "." or "-"
// as "_" (no hidden file, no "." or "..", nothing a shell reads as an
// option), a "_" before a Windows device name (CON, NUL, COM1, …: the
// operator may save the file on Windows), "file" when nothing is left, and
// at most MaxNameLen bytes with the extension kept.
func SanitizeName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	b := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == ' ', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	s := strings.TrimRight(strings.TrimLeft(string(b), " "), ". ")
	if s == "" {
		return "file"
	}
	if s[0] == '.' || s[0] == '-' {
		s = "_" + s[1:]
	}
	if base, _, _ := strings.Cut(s, "."); windowsDevice.MatchString(strings.TrimRight(base, " ")) {
		s = "_" + s
	}
	if len(s) > MaxNameLen {
		ext := path.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = strings.TrimRight(s[:MaxNameLen-len(ext)], ". ") + ext
	}
	return s
}

// windowsDevice is a base name Windows opens as a device, whatever the
// extension.
var windowsDevice = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

// ExtAllowed reports whether name's extension (case-folded) is one of exts.
func ExtAllowed(name string, exts []string) bool {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	return ext != "" && path.Ext(name) != name && slices.Contains(exts, ext)
}

// StoredName is an upload's name on disk: its UTC second, then the name;
// the n-th try in one second (n > 1) adds "-n" after the time.
func StoredName(now time.Time, name string, n int) string {
	ts := now.UTC().Format("20060102T150405Z")
	if n > 1 {
		ts += "-" + strconv.Itoa(n)
	}
	return ts + "-" + name
}

// Stored is one file Store wrote: tree-relative path, sha256, size.
type Stored struct {
	Rel, SHA256 string
	Size        int64
}

// Store writes src, at most max bytes, as a new file in dirRel (0600;
// missing directories 0700). The agent's container user is the host
// owner (rootless podman keep-id, as for .lever/bootstrap.json), so it reads
// them; no other user of the guest or the host does. A name
// already taken (by an earlier upload, or by anything the agent put there,
// a link included) is skipped for the next try. More than max bytes is
// ErrTooLarge, and nothing is left behind.
func Store(tree, dirRel, name string, src io.Reader, max int64, now time.Time) (Stored, error) {
	var f *os.File
	var rel string
	for n := 1; f == nil; n++ {
		if n > maxTries {
			return Stored{}, ErrBusy
		}
		rel = path.Join(dirRel, StoredName(now, name, n))
		var err error
		f, err = fsutil.CreateInTreeNoLinks(tree, rel, 0o700, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Stored{}, err
		}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, max+1))
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = fsutil.RemoveInTreeNoLinks(tree, rel)
		return Stored{}, err
	}
	return Stored{Rel: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Hash is the sha256 and size of the regular file at rel (no link on its
// path), at most max bytes.
func Hash(tree, rel string, max int64) (string, int64, error) {
	f, _, err := fsutil.OpenInTreeNoLinks(tree, rel, max)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, max+1))
	if err != nil {
		return "", 0, err
	}
	if n > max {
		return "", 0, ErrTooLarge
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Stage copies src, at most max bytes, into a private temp file outside the
// tree (0600, in os.TempDir, unlinked at once) while hashing it, and
// returns it at offset 0 with its sha256 and size. An upload is staged
// first, so nothing reaches the agent's tree before the whole body was read
// and every limit checked. More than max bytes is ErrTooLarge.
func Stage(src io.Reader, max int64) (*os.File, string, int64, error) {
	tmp, err := os.CreateTemp("", "lever-upload-*")
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: %w", ErrStage, err)
	}
	_ = os.Remove(tmp.Name())
	h := sha256.New()
	dst := &hostWriter{w: tmp}
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, max+1))
	switch {
	case dst.err != nil:
		err = fmt.Errorf("%w: %w", ErrStage, dst.err)
	case err == nil && n > max:
		err = ErrTooLarge
	case err == nil:
		if _, serr := tmp.Seek(0, io.SeekStart); serr != nil {
			err = fmt.Errorf("%w: %w", ErrStage, serr)
		}
	}
	if err != nil {
		tmp.Close()
		return nil, "", 0, err
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), n, nil
}

// ErrStage is a host fault while staging (the temp file), as opposed to
// an error reading the body.
var ErrStage = errors.New("staging the upload failed")

// hostWriter keeps the first write error, so Stage can tell a host fault
// from a body that could not be read.
type hostWriter struct {
	w   io.Writer
	err error
}

func (h *hostWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	if err != nil && h.err == nil {
		h.err = err
	}
	return n, err
}

// CopyVerified copies the file at rel into a private temp file (0600,
// unlinked at once, so only the returned handle reaches it) while hashing
// it, and returns the copy at offset 0 only when its sha256 is want. The
// copy, not the tree file, is what a download sends: the agent can rewrite
// the tree file at any moment, the copy it cannot reach. Bytes that hash
// otherwise, or more than max, are ErrChanged.
func CopyVerified(tree, rel, want string, max int64) (*os.File, int64, error) {
	src, _, err := fsutil.OpenInTreeNoLinks(tree, rel, max)
	if errors.Is(err, fsutil.ErrFileTooLarge) {
		return nil, 0, ErrChanged
	}
	if err != nil {
		return nil, 0, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp("", "lever-file-*")
	if err != nil {
		return nil, 0, err
	}
	_ = os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, max+1))
	if err == nil && (n > max || hex.EncodeToString(h.Sum(nil)) != want) {
		err = ErrChanged
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		return nil, 0, err
	}
	return tmp, n, nil
}

// ShareRel checks the path an agent names in share_file: its container
// path (/workspace/…) or workspace-relative, and exactly
// <Dir>/out/<Key(to)>/<name> with a name SanitizeName leaves as it is (no
// subdirectory, no hidden file, no unclean path). It returns the
// tree-relative path and the name, or ErrBadPath.
func ShareRel(ws, to, p string) (string, string, error) {
	p = strings.TrimPrefix(p, ContainerWorkspace+"/")
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", "", ErrBadPath
	}
	dir, name := path.Split(p)
	if dir != path.Join(Dir, "out", Key(to))+"/" || name == "" || SanitizeName(name) != name {
		return "", "", ErrBadPath
	}
	return path.Join(ws, p), name, nil
}

// ContainerPath is where the agent of workspace ws sees the tree-relative
// rel: under /workspace, with ws itself removed.
func ContainerPath(ws, rel string) string {
	if ws != "." {
		rel = strings.TrimPrefix(rel, ws+"/")
	}
	return path.Join(ContainerWorkspace, rel)
}
