package treebackup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrNoEntry means a path has no entry in the snapshot.
var ErrNoEntry = errors.New("no such entry in the snapshot")

// validateOnce reads a manifest through to the end; a nil error means the
// reader validated all of it.
func validateOnce(m OpenManifest) error {
	r, err := m()
	if err != nil {
		return err
	}
	defer r.Close()
	for {
		if _, err := r.Next(); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// lsLine renders type, mode, size, short hash, state and Q(path), plus the
// link target.
func lsLine(e *Entry) string {
	mode, size := "----", "-"
	if e.HasMeta {
		mode = fmt.Sprintf("%04o", e.Mode)
		if e.Type == TypeFile {
			size = fmt.Sprint(e.Size)
		}
	}
	state := "-"
	if e.State != StateOK || e.Listing != "" {
		state = stateWord(e)
	}
	s := fmt.Sprintf("%-7s %s %12s %-13s %-12s %s", word(e.Type), mode, size, shortHash(e.SHA256), state, Q(e.Path))
	if e.Type == TypeLink {
		if e.HasTarget {
			s += " -> " + Q(e.Target)
		} else {
			s += " -> (target unreadable)"
		}
	}
	return s
}

// Ls lists a snapshot's entries: the children of path (the whole subtree
// with recursive), or the single entry when path is not a directory. A nil
// path is the tree root. The manifest is validated in full first.
func Ls(w io.Writer, m OpenManifest, path []byte, recursive bool) error {
	if err := validateOnce(m); err != nil {
		return err
	}
	r, err := m()
	if err != nil {
		return err
	}
	defer r.Close()
	found := len(path) == 0
	isDir := found
	for {
		e, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch {
		case len(path) > 0 && bytes.Equal(e.Path, path):
			found = true
			if e.Type != TypeDir {
				_, err := io.WriteString(w, lsLine(e)+"\n")
				return err
			}
			isDir = true
			continue
		case !IsUnder(e.Path, path):
			if found && ComparePaths(e.Path, path) > 0 {
				return nil
			}
			continue
		}
		if !recursive && !bytes.Equal(ParentOf(e.Path), path) {
			continue
		}
		if _, err := io.WriteString(w, lsLine(e)+"\n"); err != nil {
			return err
		}
	}
	if !found || !isDir {
		return fmt.Errorf("%s: %w", Q(path), ErrNoEntry)
	}
	return nil
}
