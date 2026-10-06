// Package hostledger is the safe JSON-lines file handling shared by lever's
// host-side records (package chatledger: the web chat posts the remote proxy
// forwarded; package sentledger: the messages lever itself sent to agents).
//
// A host record is evidence only while no one but the host user can write
// it. So a record directory must be a real directory, not writable by group
// or others and owned by this uid (CheckDir), and a record file must be a
// regular file with the same properties, opened without following a symlink
// and checked to be the file that was inspected (ReadFile). Appends create
// and keep files 0600 and never follow a symlink (File.Append). A file past
// its cap is renamed to <path>.1 (replacing any previous .1) before the next
// append; readers read both.
package hostledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// ErrUnsafe means a record file or directory can be written by another user
// (or is not a plain file or directory), so its lines prove nothing.
var ErrUnsafe = errors.New("host record is writable by another user")

// MaxLine bounds one record line: a message is at most 16000 characters
// (scion's messages.MaxMessageLength), so this leaves room for JSON escaping.
// A longer line is skipped on read.
const MaxLine = 1 << 20

// File appends JSON lines to one record file. Safe for concurrent use by one
// process.
type File struct {
	// Path is the record file.
	Path string
	// Label prefixes every error ("chat ledger").
	Label string
	// Cap is the size past which the file is rotated to Path+".1" before the
	// next append.
	Cap int64
	// Lock, when set, is held across the rotate-and-append, so two processes
	// appending to the same file (a broker restart handoff) cannot both
	// rotate it and drop the lines the first rotation kept.
	Lock func() (unlock func(), err error)

	mu sync.Mutex
}

// openFile is os.OpenFile; tests replace it to fail the open after a rotate.
var openFile = os.OpenFile

// Append writes v as one JSON line, rotating first when the file has grown
// past Cap. The file is created 0600, and an existing file is set back to
// 0600, so no other user can add a line.
func (w *File) Append(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Lock != nil {
		unlock, err := w.Lock()
		if err != nil {
			return fmt.Errorf("%s: lock: %w", w.Label, err)
		}
		defer unlock()
	}
	rotated := false
	if fi, err := os.Stat(w.Path); err == nil && fi.Size() > w.Cap {
		if err := os.Rename(w.Path, w.Path+".1"); err != nil {
			return fmt.Errorf("%s: rotate: %w", w.Label, err)
		}
		rotated = true
	}
	// O_NOFOLLOW: never append to (or chmod) whatever a symlink here points
	// at; ReadFile refuses a symlinked file anyway.
	f, err := openFile(w.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND|ONoFollow, 0o600)
	if err != nil {
		if rotated {
			// No new file: move the full one back, so no reader is left
			// with a .1 and no main file (one that lists main files would
			// miss its records). It rotates again on the next append. A
			// link fails when another process created the main file in
			// between; then both stay (readers read .1 and the main file).
			if os.Link(w.Path+".1", w.Path) == nil {
				_ = os.Remove(w.Path + ".1")
			}
		}
		return fmt.Errorf("%s: %w", w.Label, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", w.Label, err)
	}
	// A crash mid-write leaves a torn last line with no newline. Start on a
	// new line, so the torn one is skipped on read and this one is kept.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", w.Label, err)
	}
	return f.Close()
}

// CheckDir refuses a record directory that is a symlink, not a directory,
// writable by others, or owned by another user.
func CheckDir(dir, label string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafe, dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is %v", ErrUnsafe, dir, fi.Mode().Perm())
	}
	if owner, ok := FileOwner(fi); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d", ErrUnsafe, dir, owner)
	}
	return nil
}

// ReadFile calls each with every line of the record file p (without its
// newline, trimmed). A missing file is no lines, not an error. A symlink, a
// file that is not regular, one with group or other write permission, one
// owned by another user, or one swapped while it was opened is ErrUnsafe. A
// line longer than MaxLine is skipped; each decides what a torn line means.
func ReadFile(p, label string, each func(line []byte)) error {
	// Lstat first: a symlink could point at a file some other process can
	// write, whatever its own mode says.
	li, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if li.Mode()&fs.ModeSymlink != 0 || !li.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUnsafe, p)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|ONoFollow, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !os.SameFile(li, fi) {
		return fmt.Errorf("%w: %s changed while it was opened", ErrUnsafe, p)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is %v", ErrUnsafe, p, fi.Mode().Perm())
	}
	if owner, ok := FileOwner(fi); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d", ErrUnsafe, p, owner)
	}
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := readLine(rd)
		if len(line) > 0 {
			each(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
}

// readLine returns the next line without its newline, or nil when the line
// is longer than MaxLine (the rest of it is consumed and dropped).
func readLine(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := rd.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > MaxLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, err
		}
		return bytes.TrimSpace(buf), err
	}
}
