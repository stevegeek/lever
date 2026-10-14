package treebackup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ErrNoContent means the entry has no stored content to show.
var ErrNoContent = errors.New("no stored content")

// ErrUntrusted means content was written that did not match its digest.
var ErrUntrusted = errors.New("the content written does not match the snapshot: the output is not trustworthy")

// Cat writes one file's stored content to w. The manifest is validated in
// full first. It refuses entries with no stored content and says why. The
// content is hashed while it streams; on a mismatch the bytes already
// written must be discarded, which the error says.
func Cat(w io.Writer, m OpenManifest, blobs BlobSource, path []byte) error {
	if err := validateOnce(m); err != nil {
		return err
	}
	e, err := findEntry(m, path)
	if err != nil {
		return err
	}
	if err := hasContent(e); err != nil {
		return fmt.Errorf("%s: %w", Q(path), err)
	}
	rc, size, err := blobs.OpenBlob(e.SHA256)
	if err != nil {
		return fmt.Errorf("%s: blob %s: %w", Q(path), shortHash(e.SHA256), err)
	}
	defer rc.Close()
	if size != e.Size {
		return fmt.Errorf("%s: blob %s has size %d, the snapshot says %d: run lever tree verify", Q(path), shortHash(e.SHA256), size, e.Size)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(rc, e.Size))
	if err != nil {
		return fmt.Errorf("%s: %w", Q(path), err)
	}
	var extra [1]byte
	more, _ := rc.Read(extra[:])
	if n != e.Size || more > 0 || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%s: %w; run lever tree verify", Q(path), ErrUntrusted)
	}
	return nil
}

func findEntry(m OpenManifest, path []byte) (*Entry, error) {
	r, err := m()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for {
		e, err := r.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s: %w", Q(path), ErrNoEntry)
		}
		if err != nil {
			return nil, err
		}
		if c := ComparePaths(e.Path, path); c == 0 {
			return cloneEntry(e), nil
		} else if c > 0 {
			return nil, fmt.Errorf("%s: %w", Q(path), ErrNoEntry)
		}
	}
}

func hasContent(e *Entry) error {
	switch {
	case e.Type != TypeFile:
		return fmt.Errorf("%w: its type is %s, not file", ErrNoContent, typeWord(e))
	case e.State == StateSecret:
		return fmt.Errorf("%w: it is the broker ticket, whose content is never stored", ErrNoContent)
	case e.SHA256 == "":
		return fmt.Errorf("%w: its content was not captured (%s)", ErrNoContent, stateWord(e))
	}
	return nil
}
