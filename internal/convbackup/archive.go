// Package convbackup keeps the manager's Claude conversation across a fresh
// create (`lever up --fresh`), which deletes the manager's scion record and,
// with it, the record's home in the guest, where Claude Code keeps its
// transcripts (~/.claude/projects/<cwd>/<session>.jsonl).
//
// A backup is a copy of that projects directory on the host, in the state
// directory (Store). A restore puts it back into the home of a suspended
// manager, so the next resume (`claude --continue`, which picks the newest
// transcript) continues the old conversation.
//
// The agent wrote every byte of what is copied, and is assumed hostile, so
// nothing it wrote is trusted: the guest side never follows a symbolic link
// it planted (Guest), and the host rewrites the archive it reads from the
// guest into one that holds only directories and regular files with clean
// relative names, within size limits (Copy). Only that rewritten archive is
// stored, and only it is ever sent back into the guest.
//
// ~/.claude.json is deliberately not carried over: it holds the account
// identity, onboarding state and the MCP server configuration lever writes
// when the agent boots, and with an API key a copy of the key. The fresh
// record's own file is the right one, and `claude --continue` needs only
// the transcripts.
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
	// MaxFile is the largest single file.
	MaxFile int64
	// MaxTotal is the sum of all file sizes.
	MaxTotal int64
	// MaxEntries is the number of files and directories.
	MaxEntries int
}

// DefaultLimits are the limits a backup and a restore apply. A long-lived
// manager's transcripts run to tens of megabytes; these leave a wide margin
// and still stop an agent from filling the host's disk.
var DefaultLimits = Limits{MaxFile: 1 << 30, MaxTotal: 4 << 30, MaxEntries: 200_000}

// Stats describes an archive Copy wrote.
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

// Copy reads the gzip-compressed tar archive src and writes to dst a
// gzip-compressed tar archive of its directories and regular files only.
//
// When prefix is not empty, every entry must be prefix itself or lie beneath
// it, and the prefix is removed from the names written (the guest archives
// "projects"; the stored archive is relative to it). Each name must be a
// clean relative path (no "..", no leading "/", no empty or "." component,
// printable UTF-8), appear once, and not lie beneath a regular file. The
// archive written carries modes 0700 and 0600, no owner, and the entries'
// modification times, which decide the transcript `claude --continue` picks.
// Any violation, or a limit passed, refuses the whole archive (ErrRefused);
// a link or a special file is skipped and counted.
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
	seen := map[string]bool{}  // every name written
	files := map[string]bool{} // the regular files among them
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
			st.Skipped++
			continue
		}
		if err := checkName(name); err != nil {
			return st, err
		}
		if seen[name] {
			return st, refused("%q appears twice", name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if files[p] {
				return st, refused("%q lies beneath the file %q", name, p)
			}
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

// stripPrefix removes prefix from an archive name. keep is false for the
// prefix directory itself, which the restore creates on its own.
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

// checkName refuses a name that is not a clean relative path of printable
// UTF-8 within the length and depth limits.
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
