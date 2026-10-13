// Package convbackup keeps the manager's Claude conversation across a fresh
// create (`lever up --fresh`), which deletes the manager's scion record and
// with it the guest home where Claude Code keeps its transcripts
// (~/.claude/projects). A backup is a host copy of that directory (Store); a
// restore puts it back into a suspended manager's home, so the next resume
// (`claude --continue`, which picks the newest transcript) continues it.
//
// The agent wrote every byte copied and is assumed hostile: the guest side
// never follows a symbolic link it planted (Guest), and the host rewrites
// the archive into one holding only directories and regular files with
// clean relative names, within size limits (Copy). Only that rewritten
// archive is stored or sent back into the guest.
//
// ~/.claude.json is deliberately not carried over: it holds the account
// identity, the MCP configuration lever writes at boot, and possibly an API
// key. `claude --continue` needs only the transcripts.
package convbackup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits bound what lever accepts from the guest. Each is a hard limit: an
// archive past one is refused whole, never cut short.
type Limits struct {
	MaxFile    int64
	MaxTotal   int64
	MaxEntries int
}

// DefaultLimits leave a wide margin over a long-lived manager's transcripts
// and still stop an agent from filling the host's disk.
var DefaultLimits = Limits{MaxFile: 1 << 30, MaxTotal: 4 << 30, MaxEntries: 200_000}

type Stats struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Bytes int64 `json:"bytes"`
	// Skipped counts entries that were neither a directory nor a regular
	// file (symbolic and hard links, devices, FIFOs): never copied.
	Skipped int `json:"skipped"`
	// Newest is the transcript (a .jsonl file) with the latest modification
	// time, the one `claude --continue` resumes, and NewestTime that time.
	// Agent-written: sanitize it before printing.
	Newest     string    `json:"newest,omitempty"`
	NewestTime time.Time `json:"newest_time,omitzero"`
}

// Name rules. A component longer than a file system allows, a path deeper
// than any Claude Code writes, or a byte that is not printable UTF-8 is not a
// transcript.
const (
	maxNameLen      = 1024
	maxComponentLen = 255
	maxDepth        = 16
)

// ErrRefused wraps every reason Copy refuses an archive.
var ErrRefused = errors.New("conversation archive refused")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Copy reads the gzip tar archive src and writes to dst a gzip tar of its
// directories and regular files only.
//
// When prefix is not empty, every entry must be prefix or lie beneath it,
// and prefix is stripped from the names written. Each name must be a clean
// relative path of printable UTF-8, appear once, and not lie beneath a
// regular file. The output carries modes 0700 and 0600, no owner, and the
// entries' modification times, which decide the transcript `claude
// --continue` picks. Any violation or passed limit refuses the whole archive
// (ErrRefused); a link or special file is skipped and counted.
func Copy(dst io.Writer, src io.Reader, prefix string, lim Limits) (Stats, error) {
	var st Stats
	zr, err := gzip.NewReader(src)
	if err != nil {
		return st, refused("not a gzip stream: %v", err)
	}
	zr.Multistream(false)
	tr := tar.NewReader(zr)
	zw := gzip.NewWriter(dst)
	tw := tar.NewWriter(zw)
	seen := map[string]bool{}    // every name written
	files := map[string]bool{}   // the regular files among them
	parents := map[string]bool{} // every directory an earlier name lies beneath
	var skippedBytes int64
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, refused("reading the archive: %v", err)
		}
		entries++
		if entries > lim.MaxEntries {
			return st, refused("more than %d entries", lim.MaxEntries)
		}
		name, keep, err := stripPrefix(h.Name, prefix)
		if err != nil {
			return st, err
		}
		if !keep {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old tars write
		default:
			// Skipped, but its data still passes through the decompressor: it
			// counts against the total.
			if h.Size > 0 {
				if h.Size > lim.MaxTotal || skippedBytes+h.Size > lim.MaxTotal {
					return st, refused("more than %d bytes in all", lim.MaxTotal)
				}
				skippedBytes += h.Size
			}
			st.Skipped++
			continue
		}
		if err := checkName(name); err != nil {
			return st, err
		}
		if seen[name] {
			return st, refused("%q appears twice", name)
		}
		if h.Typeflag != tar.TypeDir && parents[name] {
			return st, refused("%q is a file, and an earlier entry lies beneath it", name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if files[p] {
				return st, refused("%q lies beneath the file %q", name, p)
			}
			parents[p] = true
		}
		seen[name] = true
		mtime := h.ModTime.UTC()
		if h.Typeflag == tar.TypeDir {
			st.Dirs++
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o700, ModTime: mtime, Format: tar.FormatPAX}); err != nil {
				return st, err
			}
			continue
		}
		if h.Size < 0 || h.Size > lim.MaxFile {
			return st, refused("%q is %d bytes, more than the %d a file may hold", name, h.Size, lim.MaxFile)
		}
		if st.Bytes+h.Size > lim.MaxTotal {
			return st, refused("more than %d bytes in all", lim.MaxTotal)
		}
		files[name] = true
		st.Files++
		st.Bytes += h.Size
		if strings.HasSuffix(name, ".jsonl") && (st.Newest == "" || mtime.After(st.NewestTime)) {
			st.Newest, st.NewestTime = name, mtime
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: h.Size, ModTime: mtime, Format: tar.FormatPAX}); err != nil {
			return st, err
		}
		if n, err := io.CopyN(tw, tr, h.Size); err != nil {
			return st, refused("%q: read %d of %d bytes: %v", name, n, h.Size, err)
		}
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	return st, nil
}

// stripPrefix: keep is false for the prefix directory itself, which the
// restore creates on its own.
func stripPrefix(name, prefix string) (string, bool, error) {
	name = strings.TrimSuffix(name, "/")
	name = strings.TrimPrefix(name, "./")
	if prefix == "" {
		return name, true, nil
	}
	if name == prefix {
		return "", false, nil
	}
	rest, ok := strings.CutPrefix(name, prefix+"/")
	if !ok {
		return "", false, refused("%q lies outside %s", name, prefix)
	}
	return rest, true, nil
}

func checkName(name string) error {
	switch {
	case name == "" || len(name) > maxNameLen:
		return refused("an entry name is empty or longer than %d bytes", maxNameLen)
	case !utf8.ValidString(name):
		return refused("an entry name is not UTF-8")
	case strings.HasPrefix(name, "/"):
		return refused("%q is absolute", name)
	case path.Clean(name) != name:
		return refused("%q is not a clean path", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\\' {
			return refused("%q holds a control character or a backslash", name)
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxDepth {
		return refused("%q is deeper than %d", name, maxDepth)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || len(p) > maxComponentLen {
			return refused("%q has an empty, dot or overlong component", name)
		}
	}
	return nil
}
